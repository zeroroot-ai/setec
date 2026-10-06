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
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	setecv1grpc "github.com/zeroroot-ai/setec/api/grpc/v1"
	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
)

// TestSuspendAndResume_SetTheDesiredStateOfASession pins the API of
// setec#193: a caller suspends and resumes a session of a class with
// checkpoints, and an ephemeral Sandbox or a class with no checkpoints is
// refused.
func TestSuspendAndResume_SetTheDesiredStateOfASession(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	scheme := runtime.NewScheme()
	utilruntime.Must(setecv1alpha1.AddToScheme(scheme))
	withCk := &setecv1alpha1.SandboxClass{Name: "sessions"}
	withCk.Spec.SessionCheckpoint = &setecv1alpha1.SessionCheckpointSpec{}
	noCk := &setecv1alpha1.SandboxClass{Name: "plain"}
	mk := func(name, class string, session bool) *setecv1alpha1.Sandbox {
		sb := &setecv1alpha1.Sandbox{Name: name, Namespace: "team-a"}
		sb.Spec.SandboxClassName = class
		if session {
			sb.Spec.Lifecycle = &setecv1alpha1.Lifecycle{Mode: setecv1alpha1.LifecycleModeSession}
		}
		return sb
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(withCk, noCk,
		mk("s", "sessions", true), mk("e", "sessions", false), mk("p", "plain", true)).Build()
	s := &Service{Client: c, AuthDisabled: true, DefaultNamespace: "team-a"}

	if _, err := s.Suspend(ctx, &setecv1grpc.SuspendRequest{SandboxId: "team-a/s/uid-s"}); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	got := &setecv1alpha1.Sandbox{}
	_ = c.Get(ctx, types.NamespacedName{Namespace: "team-a", Name: "s"}, got)
	if got.Spec.DesiredState != setecv1alpha1.SandboxDesiredStateSuspended {
		t.Fatalf("desired state = %q after Suspend", got.Spec.DesiredState)
	}
	if _, err := s.Resume(ctx, &setecv1grpc.ResumeRequest{SandboxId: "team-a/s/uid-s"}); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	_ = c.Get(ctx, types.NamespacedName{Namespace: "team-a", Name: "s"}, got)
	if got.Spec.DesiredState != setecv1alpha1.SandboxDesiredStateRunning {
		t.Fatalf("desired state = %q after Resume", got.Spec.DesiredState)
	}
	for _, id := range []string{"team-a/e/uid-e", "team-a/p/uid-p"} {
		if _, err := s.Suspend(ctx, &setecv1grpc.SuspendRequest{SandboxId: id}); status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("Suspend(%s) = %v, want FailedPrecondition", id, err)
		}
	}
}
