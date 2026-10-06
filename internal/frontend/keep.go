// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package frontend

import (
	"context"
	"strconv"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	setecv1grpc "github.com/zeroroot-ai/setec/api/grpc/v1"
	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
	runtimepkg "github.com/zeroroot-ai/setec/internal/runtime"
)

// keptSnapshotPrefix starts the name of each kept snapshot.
const keptSnapshotPrefix = "kept-"

// Keep asks the operator for a kept snapshot of a running launcher sandbox
// (setec#196) and waits until it is Ready.
func (s *Service) Keep(ctx context.Context, req *setecv1grpc.KeepRequest) (*setecv1grpc.KeepResponse, error) {
	ns, name, err := parseSandboxID(req.GetSandboxId())
	if err != nil {
		return nil, err
	}
	if err := s.checkTenantNamespace(ctx, req.GetTenant(), ns); err != nil {
		return nil, err
	}
	src := &setecv1alpha1.Sandbox{}
	if err := s.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, src); err != nil {
		return nil, status.Errorf(grpcCodeFor(err), "get Sandbox: %v", err)
	}
	if src.Status.Runtime == nil || src.Status.Runtime.Chosen != runtimepkg.BackendLauncher ||
		src.Status.Phase != setecv1alpha1.SandboxPhaseRunning {
		return nil, status.Error(codes.FailedPrecondition, "only a running launcher sandbox is kept")
	}
	snapName, err := s.snapshotAndWait(ctx, src, keptSnapshotPrefix, func(sp *setecv1alpha1.SandboxSnapshotSpec) {
		sp.Kept = true
	})
	if err != nil {
		return nil, err
	}
	if req.GetPinned() {
		if err := s.setPinned(ctx, ns, snapName, true); err != nil {
			return &setecv1grpc.KeepResponse{Snapshot: snapName}, err
		}
	}
	return &setecv1grpc.KeepResponse{Snapshot: snapName}, nil
}

// Pin pins or unpins a kept snapshot of the tenant of the caller.
func (s *Service) Pin(ctx context.Context, req *setecv1grpc.PinRequest) (*setecv1grpc.PinResponse, error) {
	ns, _, err := s.resolveNamespace(ctx, req.GetTenant())
	if err != nil {
		return nil, err
	}
	if err := s.setPinned(ctx, ns, req.GetSnapshot(), req.GetPinned()); err != nil {
		return nil, err
	}
	return &setecv1grpc.PinResponse{}, nil
}

// setPinned writes spec.pinned of a kept snapshot. The admission webhook
// refuses a pin above the limit of the tenant.
func (s *Service) setPinned(ctx context.Context, ns, name string, pinned bool) error {
	snap := &setecv1alpha1.Snapshot{}
	if err := s.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, snap); err != nil {
		return status.Errorf(grpcCodeFor(err), "get the snapshot: %v", err)
	}
	if !snap.Spec.Kept {
		return status.Error(codes.FailedPrecondition, "only a kept snapshot is pinned")
	}
	if snap.Spec.Pinned == pinned {
		return nil
	}
	original := snap.DeepCopy()
	snap.Spec.Pinned = pinned
	if err := s.Client.Patch(ctx, snap, client.MergeFrom(original)); err != nil {
		code := grpcCodeFor(err)
		if code == codes.InvalidArgument || code == codes.PermissionDenied {
			code = codes.FailedPrecondition
		}
		return status.Errorf(code, "pin the snapshot: %v", err)
	}
	return nil
}

// reviewSandbox is the review Sandbox of a kept snapshot: it loads the
// snapshot with the class, image and machine size of its source, and has
// no network.
func reviewSandbox(snap *setecv1alpha1.Snapshot) (*setecv1alpha1.Sandbox, error) {
	if !snap.Spec.Kept {
		return nil, status.Errorf(codes.FailedPrecondition, "snapshot %q is not kept", snap.Name)
	}
	vcpu, err := strconv.Atoi(snap.Annotations[setecv1alpha1.SnapshotVCPUAnnotation])
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "snapshot %q has no machine size", snap.Name)
	}
	mem, err := resource.ParseQuantity(snap.Annotations[setecv1alpha1.SnapshotMemoryAnnotation])
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "snapshot %q has no memory size", snap.Name)
	}
	sb := &setecv1alpha1.Sandbox{
		GenerateName: "review-",
		Namespace:    snap.Namespace,
		Spec: setecv1alpha1.SandboxSpec{
			SandboxClassName: snap.Spec.SandboxClass,
			Image:            snap.Spec.ImageRef,
			Command:          []string{"sh"},
			Resources:        setecv1alpha1.Resources{VCPU: int32(vcpu), Memory: mem}, //nolint:gosec // a small count
			SnapshotRef:      &setecv1alpha1.SandboxSnapshotRef{Name: snap.Name},
			Review:           true,
			Network:          &setecv1alpha1.Network{Mode: setecv1alpha1.NetworkModeNone},
		},
	}
	return sb, nil
}
