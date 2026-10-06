// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package frontend

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	setecv1grpc "github.com/zeroroot-ai/setec/api/grpc/v1"
	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
	runtimepkg "github.com/zeroroot-ai/setec/internal/runtime"
)

// The limits of a fork (setec#195).
const (
	maxForks           = 32
	defaultForkTTL     = time.Hour
	forkSnapshotWait   = 3 * time.Minute
	forkSnapshotPoll   = 500 * time.Millisecond
	forkOfLabel        = "setec.zeroroot.ai/fork-of"
	forkSnapshotPrefix = "fork-"
)

// Fork snapshots a running launcher sandbox and starts count sandboxes from
// the snapshot. The operator takes the snapshot, and each fork loads it
// through the restore path, which gives it a new identity, new randomness
// and its own writable layer.
func (s *Service) Fork(ctx context.Context, req *setecv1grpc.ForkRequest) (*setecv1grpc.ForkResponse, error) {
	ns, name, err := parseSandboxID(req.GetSandboxId())
	if err != nil {
		return nil, err
	}
	if err := s.checkTenantNamespace(ctx, req.GetTenant(), ns); err != nil {
		return nil, err
	}
	count := int(req.GetCount())
	if count < 1 || count > maxForks {
		return nil, status.Errorf(codes.InvalidArgument, "count must be 1 to %d, got %d", maxForks, count)
	}
	ttl := defaultForkTTL
	if t := req.GetSnapshotTtlSeconds(); t < 0 {
		return nil, status.Errorf(codes.InvalidArgument, "snapshot_ttl_seconds must not be negative, got %d", t)
	} else if t > 0 {
		ttl = time.Duration(t) * time.Second
	}

	src := &setecv1alpha1.Sandbox{}
	if err := s.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, src); err != nil {
		return nil, status.Errorf(grpcCodeFor(err), "get Sandbox: %v", err)
	}
	switch {
	case src.Status.Runtime == nil || src.Status.Runtime.Chosen != runtimepkg.BackendLauncher:
		return nil, status.Error(codes.FailedPrecondition, "only a launcher sandbox forks")
	case src.Spec.IsSession():
		return nil, status.Error(codes.FailedPrecondition,
			"a session does not fork: its workspace belongs to one sandbox")
	case src.Status.Phase != setecv1alpha1.SandboxPhaseRunning:
		return nil, status.Errorf(codes.FailedPrecondition, "the sandbox is %s, not Running", src.Status.Phase)
	}

	snapName, err := s.forkSnapshot(ctx, src, ttl)
	if err != nil {
		return nil, err
	}
	resp := &setecv1grpc.ForkResponse{Snapshot: snapName}
	for range count {
		fork := forkOf(src, snapName, req.GetNetwork())
		if err := s.Client.Create(ctx, fork); err != nil {
			return resp, status.Errorf(grpcCodeFor(err), "create a fork: %v", err)
		}
		resp.SandboxIds = append(resp.SandboxIds, fmt.Sprintf("%s/%s/%s", fork.Namespace, fork.Name, fork.UID))
	}
	return resp, nil
}

// forkSnapshot asks the operator for a forkable snapshot of src and waits
// until it is Ready.
func (s *Service) forkSnapshot(ctx context.Context, src *setecv1alpha1.Sandbox, ttl time.Duration) (string, error) {
	suffix := make([]byte, 4)
	_, _ = rand.Read(suffix)
	snapName := forkSnapshotPrefix + src.Name + "-" + hex.EncodeToString(suffix)
	original := src.DeepCopy()
	src.Spec.Snapshot = &setecv1alpha1.SandboxSnapshotSpec{
		Create: true, Name: snapName, Forkable: true,
		AfterCreate: setecv1alpha1.SandboxSnapshotAfterCreateRunning,
		TTL:         &metav1.Duration{Duration: ttl},
	}
	if err := s.Client.Patch(ctx, src, client.MergeFrom(original)); err != nil {
		return "", status.Errorf(grpcCodeFor(err), "ask for the snapshot: %v", err)
	}
	wctx, cancel := context.WithTimeout(ctx, forkSnapshotWait)
	defer cancel()
	for {
		snap := &setecv1alpha1.Snapshot{}
		err := s.Client.Get(wctx, types.NamespacedName{Namespace: src.Namespace, Name: snapName}, snap)
		switch {
		case err == nil && snap.Status.Phase == setecv1alpha1.SnapshotPhaseReady:
			return snapName, nil
		case err == nil && snap.Status.Phase == setecv1alpha1.SnapshotPhaseFailed:
			return "", status.Errorf(codes.Aborted, "the snapshot failed: %s", snap.Status.Reason)
		case err != nil && !apierrors.IsNotFound(err):
			return "", status.Errorf(grpcCodeFor(err), "read the snapshot: %v", err)
		}
		select {
		case <-wctx.Done():
			return "", status.Errorf(codes.DeadlineExceeded, "the snapshot %s is not Ready", snapName)
		case <-time.After(forkSnapshotPoll):
		}
	}
}

// forkOf is one fork of src: the class, image, size and command of src, the
// snapshot as its source, and the network of the request.
func forkOf(src *setecv1alpha1.Sandbox, snapName string, net *setecv1grpc.Network) *setecv1alpha1.Sandbox {
	labels := map[string]string{forkOfLabel: src.Name}
	for k, v := range src.Labels {
		if k != forkOfLabel {
			labels[k] = v
		}
	}
	fork := &setecv1alpha1.Sandbox{
		GenerateName: src.Name + "-fork-",
		Namespace:    src.Namespace,
		Labels:       labels,
		Spec: setecv1alpha1.SandboxSpec{
			SandboxClassName: src.Spec.SandboxClassName,
			Image:            src.Spec.Image,
			Command:          append([]string(nil), src.Spec.Command...),
			Resources:        src.Spec.Resources,
			SnapshotRef:      &setecv1alpha1.SandboxSnapshotRef{Name: snapName},
		},
	}
	fork.Spec.Env = append([]corev1.EnvVar(nil), src.Spec.Env...)
	if net != nil {
		fork.Spec.Network = &setecv1alpha1.Network{Mode: setecv1alpha1.NetworkMode(net.GetMode())}
		for _, a := range net.GetAllow() {
			fork.Spec.Network.Allow = append(fork.Spec.Network.Allow, networkAllowFromProto(a))
		}
	}
	return fork
}
