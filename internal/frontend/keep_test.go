// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package frontend

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	setecv1grpc "github.com/zeroroot-ai/setec/api/grpc/v1"
	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
)

// TestPinAndReview pins the caller side of setec#196: a kept snapshot is
// pinned and unpinned, a snapshot that is not kept is refused, and a review
// launch makes a review Sandbox with no network and the machine size of
// the source.
func TestPinAndReview(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	scheme := runtime.NewScheme()
	utilruntime.Must(setecv1alpha1.AddToScheme(scheme))
	kept := &setecv1alpha1.Snapshot{Name: "kept-a", Namespace: "team-a", Annotations: map[string]string{
		setecv1alpha1.SnapshotVCPUAnnotation: "2", setecv1alpha1.SnapshotMemoryAnnotation: "2Gi",
	}}
	kept.Spec = setecv1alpha1.SnapshotSpec{Kept: true, SandboxClass: "launcher", ImageRef: "img@sha256:abc"}
	plain := &setecv1alpha1.Snapshot{Name: "plain", Namespace: "team-a"}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(kept, plain).Build()
	s := &Service{Client: c, AuthDisabled: true, DefaultNamespace: "team-a"}

	if _, err := s.Pin(ctx, &setecv1grpc.PinRequest{Snapshot: "kept-a", Pinned: true}); err != nil {
		t.Fatalf("Pin: %v", err)
	}
	got := &setecv1alpha1.Snapshot{}
	_ = c.Get(ctx, types.NamespacedName{Namespace: "team-a", Name: "kept-a"}, got)
	if !got.Spec.Pinned {
		t.Fatal("the snapshot is not pinned")
	}
	if _, err := s.Pin(ctx, &setecv1grpc.PinRequest{Snapshot: "plain", Pinned: true}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("pin of a plain snapshot = %v", err)
	}

	resp, err := s.Launch(ctx, &setecv1grpc.LaunchRequest{ReviewSnapshot: "kept-a"})
	if err != nil {
		t.Fatalf("review Launch: %v", err)
	}
	sbs := &setecv1alpha1.SandboxList{}
	_ = c.List(ctx, sbs, client.InNamespace("team-a"))
	if len(sbs.Items) != 1 || resp.GetName() == "" {
		t.Fatalf("review Sandboxes = %d", len(sbs.Items))
	}
	r := sbs.Items[0].Spec
	if !r.Review || r.Network == nil || r.Network.Mode != setecv1alpha1.NetworkModeNone ||
		r.SnapshotRef.Name != "kept-a" || r.Resources.VCPU != 2 || r.Resources.Memory.String() != "2Gi" {
		t.Fatalf("review Sandbox = %+v", r)
	}
	if _, err := s.Launch(ctx, &setecv1grpc.LaunchRequest{ReviewSnapshot: "plain"}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("review of a plain snapshot = %v", err)
	}
}
