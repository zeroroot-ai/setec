// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package frontend

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"strconv"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	setecv1grpc "github.com/zeroroot-ai/setec/api/grpc/v1"
	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
	"github.com/zeroroot-ai/setec/internal/tenancy"
)

// The snapshot of the Snapshot call (setec#242).
const (
	// defaultSnapshotTTL is the lifetime of a snapshot when the request
	// sets none. A rewind can come days after the run ends.
	defaultSnapshotTTL = 7 * 24 * time.Hour
	// snapshotPrefix starts the name of each snapshot of the call.
	snapshotPrefix = "snap-"
	// snapshotCommandAnnotation records the command of the source on the
	// snapshot, so a sandbox from the snapshot reports the same command
	// after the source is gone.
	snapshotCommandAnnotation = "setec.zeroroot.ai/command"
	// fromSnapshotLabel marks a sandbox that loads a snapshot of the call.
	fromSnapshotLabel = "setec.zeroroot.ai/from-snapshot"
)

// Snapshot takes a full snapshot of a running launcher sandbox of the
// caller (setec#242). The snapshot is forkable: a later Launch with
// from_snapshot loads it into a new sandbox of the same pair, with a new
// identity and the network of that request.
func (s *Service) Snapshot(ctx context.Context, req *setecv1grpc.SnapshotRequest) (*setecv1grpc.SnapshotResponse, error) {
	ns, name, err := parseSandboxID(req.GetSandboxId())
	if err != nil {
		return nil, err
	}
	if err := s.checkTenantNamespace(ctx, req.GetTenant(), ns); err != nil {
		return nil, err
	}
	ttl := defaultSnapshotTTL
	if t := req.GetTtlSeconds(); t < 0 {
		return nil, status.Errorf(codes.InvalidArgument, "ttl_seconds must not be negative, got %d", t)
	} else if t > 0 {
		ttl = time.Duration(t) * time.Second
	}
	src := &setecv1alpha1.Sandbox{}
	if err := s.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, src); err != nil {
		return nil, status.Errorf(grpcCodeFor(err), "get Sandbox: %v", err)
	}
	if err := forkableSource(src); err != nil {
		return nil, err
	}
	snapName, err := s.snapshotAndWait(ctx, src, snapshotPrefix, func(sp *setecv1alpha1.SandboxSnapshotSpec) {
		sp.Forkable = true
		sp.TTL = &metav1.Duration{Duration: ttl}
	})
	if err != nil {
		return nil, err
	}
	if err := s.recordCommand(ctx, ns, snapName, src.Spec.Command); err != nil {
		return &setecv1grpc.SnapshotResponse{Snapshot: snapName}, err
	}
	return &setecv1grpc.SnapshotResponse{Snapshot: snapName}, nil
}

// recordCommand writes the command of the source on the snapshot.
func (s *Service) recordCommand(ctx context.Context, ns, snapName string, command []string) error {
	raw, err := json.Marshal(command)
	if err != nil {
		return status.Errorf(codes.Internal, "encode the command: %v", err)
	}
	snap := &setecv1alpha1.Snapshot{}
	if err := s.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: snapName}, snap); err != nil {
		return status.Errorf(grpcCodeFor(err), "read the snapshot: %v", err)
	}
	original := snap.DeepCopy()
	if snap.Annotations == nil {
		snap.Annotations = map[string]string{}
	}
	snap.Annotations[snapshotCommandAnnotation] = string(raw)
	if err := s.Client.Patch(ctx, snap, client.MergeFrom(original)); err != nil {
		return status.Errorf(grpcCodeFor(err), "record the command on the snapshot: %v", err)
	}
	return nil
}

// launchFromSnapshot starts a normal sandbox of the pair from a snapshot of
// the Snapshot call (setec#242).
func (s *Service) launchFromSnapshot(
	ctx context.Context, ns string, pair tenancy.Pair, req *setecv1grpc.LaunchRequest,
) (*setecv1grpc.LaunchResponse, error) {
	if req.GetReviewSnapshot() != "" {
		return nil, status.Error(codes.InvalidArgument, "set from_snapshot or review_snapshot, not both")
	}
	// The namespace of the pair holds the snapshot, so a snapshot of
	// another pair is NotFound here: only the owner pair loads it.
	snap := &setecv1alpha1.Snapshot{}
	if err := s.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: req.GetFromSnapshot()}, snap); err != nil {
		return nil, status.Errorf(grpcCodeFor(err), "get the snapshot: %v", err)
	}
	sb, err := sandboxFromSnapshot(snap, req)
	if err != nil {
		return nil, err
	}
	sb.Labels = map[string]string{fromSnapshotLabel: snap.Name}
	maps.Copy(sb.Labels, pairLabels(pair))
	if err := s.Client.Create(ctx, sb); err != nil {
		return nil, status.Errorf(grpcCodeFor(err), "create Sandbox: %v", err)
	}
	return &setecv1grpc.LaunchResponse{
		SandboxId: fmt.Sprintf("%s/%s/%s", sb.Namespace, sb.Name, string(sb.UID)),
		Name:      sb.Name, Namespace: sb.Namespace, SandboxClass: sb.Spec.SandboxClassName,
	}, nil
}

// sandboxFromSnapshot is the sandbox that loads snap: the class, image and
// machine size of the snapshot, and the network, lifecycle and env of the
// request. It is a pure function, so each rule has a unit test.
func sandboxFromSnapshot(snap *setecv1alpha1.Snapshot, req *setecv1grpc.LaunchRequest) (*setecv1alpha1.Sandbox, error) {
	if err := checkSnapshotOpens(snap, req); err != nil {
		return nil, err
	}
	vcpu, mem, err := snapshotMachine(snap, req)
	if err != nil {
		return nil, err
	}
	var lifecycle *setecv1alpha1.Lifecycle
	if lc := req.GetLifecycle(); lc != nil {
		spec, err := lifecycleFromRequest(lc)
		if err != nil {
			return nil, err
		}
		lifecycle = spec
	}
	if lifecycle != nil && lifecycle.Mode == setecv1alpha1.LifecycleModeSession {
		return nil, status.Error(codes.InvalidArgument,
			"a session does not start from a snapshot: its workspace belongs to one sandbox")
	}
	command, err := snapshotCommand(snap, req)
	if err != nil {
		return nil, err
	}
	sb := &setecv1alpha1.Sandbox{
		GenerateName: "sbx-",
		Namespace:    snap.Namespace,
		Spec: setecv1alpha1.SandboxSpec{
			SandboxClassName: snap.Spec.SandboxClass,
			Image:            snap.Spec.ImageRef,
			Command:          command,
			Resources:        setecv1alpha1.Resources{VCPU: int32(vcpu), Memory: mem}, //nolint:gosec // a small count
			SnapshotRef:      &setecv1alpha1.SandboxSnapshotRef{Name: snap.Name},
			Lifecycle:        lifecycle,
		},
	}
	// The network of the request, never the network of the source.
	if n := req.GetNetwork(); n != nil {
		sb.Spec.Network = &setecv1alpha1.Network{Mode: setecv1alpha1.NetworkMode(n.GetMode())}
		for _, a := range n.GetAllow() {
			sb.Spec.Network.Allow = append(sb.Spec.Network.Allow, networkAllowFromProto(a))
		}
	}
	for k, v := range req.GetEnv() {
		sb.Spec.Env = append(sb.Spec.Env, corev1.EnvVar{Name: k, Value: v})
	}
	return sb, nil
}

// checkSnapshotOpens reports why req cannot open snap, or nil.
func checkSnapshotOpens(snap *setecv1alpha1.Snapshot, req *setecv1grpc.LaunchRequest) error {
	switch {
	case snap.Spec.Kept:
		return status.Errorf(codes.FailedPrecondition,
			"snapshot %q is kept: it opens only in a review sandbox (review_snapshot)", snap.Name)
	case !snap.Spec.Forkable:
		return status.Errorf(codes.FailedPrecondition,
			"snapshot %q was not taken by the Snapshot call or by Fork", snap.Name)
	case snap.Status.Phase != setecv1alpha1.SnapshotPhaseReady:
		return status.Errorf(codes.FailedPrecondition, "snapshot %q is %q, not Ready", snap.Name, snap.Status.Phase)
	}
	if c := req.GetSandboxClass(); c != "" && c != snap.Spec.SandboxClass {
		return status.Errorf(codes.InvalidArgument,
			"sandbox_class %q differs from the class %q of the snapshot", c, snap.Spec.SandboxClass)
	}
	if img := req.GetImage(); img != "" && img != snap.Spec.ImageRef {
		return status.Errorf(codes.InvalidArgument,
			"image %q differs from the image %q of the snapshot", img, snap.Spec.ImageRef)
	}
	return nil
}

// snapshotMachine is the machine size of snap. A size in req must match it.
func snapshotMachine(snap *setecv1alpha1.Snapshot, req *setecv1grpc.LaunchRequest) (vcpu int, mem resource.Quantity, err error) {
	vcpu, err = strconv.Atoi(snap.Annotations[setecv1alpha1.SnapshotVCPUAnnotation])
	if err != nil || vcpu < 1 {
		return 0, mem, status.Errorf(codes.FailedPrecondition, "snapshot %q has no machine size", snap.Name)
	}
	mem, err = resource.ParseQuantity(snap.Annotations[setecv1alpha1.SnapshotMemoryAnnotation])
	if err != nil {
		return 0, mem, status.Errorf(codes.FailedPrecondition, "snapshot %q has no memory size", snap.Name)
	}
	if r := req.GetResources(); r != nil {
		if r.GetVcpu() != 0 && int(r.GetVcpu()) != vcpu {
			return 0, mem, status.Errorf(codes.InvalidArgument,
				"resources.vcpu %d differs from the %d of the snapshot", r.GetVcpu(), vcpu)
		}
		if m := r.GetMemory(); m != "" {
			q, err := resource.ParseQuantity(m)
			if err != nil || q.Cmp(mem) != 0 {
				return 0, mem, status.Errorf(codes.InvalidArgument,
					"resources.memory %q differs from the %s of the snapshot", m, mem.String())
			}
		}
	}
	return vcpu, mem, nil
}

// snapshotCommand is the command of req, or else the command that snap
// recorded.
func snapshotCommand(snap *setecv1alpha1.Snapshot, req *setecv1grpc.LaunchRequest) ([]string, error) {
	command := append([]string(nil), req.GetCommand()...)
	if len(command) == 0 {
		if raw := snap.Annotations[snapshotCommandAnnotation]; raw != "" {
			if err := json.Unmarshal([]byte(raw), &command); err != nil {
				return nil, status.Errorf(codes.FailedPrecondition, "snapshot %q has a bad command record", snap.Name)
			}
		}
	}
	if len(command) == 0 {
		return nil, status.Errorf(codes.InvalidArgument, "snapshot %q records no command: set command", snap.Name)
	}
	return command, nil
}
