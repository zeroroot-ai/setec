// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package frontend

import (
	"context"
	"strings"
	"sync"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"sigs.k8s.io/controller-runtime/pkg/client"

	setecv1grpc "github.com/zeroroot-ai/setec/api/grpc/v1"
	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
)

func launchReq() *setecv1grpc.LaunchRequest {
	return &setecv1grpc.LaunchRequest{
		SandboxClass: testSandboxClass, Image: "alpine:3.19", Command: []string{"true"},
	}
}

func limited(t *testing.T, limit int) *Service {
	t.Helper()
	return &Service{Client: newClient(t), AuthDisabled: true, DefaultNamespace: "team-a", MaxSandboxesPerTenant: limit}
}

func setPhase(t *testing.T, c client.Client, ns string, phase setecv1alpha1.SandboxPhase) {
	t.Helper()
	list := &setecv1alpha1.SandboxList{}
	if err := c.List(context.Background(), list, client.InNamespace(ns)); err != nil {
		t.Fatal(err)
	}
	for i := range list.Items {
		list.Items[i].Status.Phase = phase
		if err := c.Status().Update(context.Background(), &list.Items[i]); err != nil {
			t.Fatal(err)
		}
	}
}

// TestLaunch_RefusesAboveTheTenantLimit proves that Launch refuses the
// Sandbox above the limit with ResourceExhausted, and that another tenant
// is not affected.
func TestLaunch_RefusesAboveTheTenantLimit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := limited(t, 2)
	for range 2 {
		if _, err := s.Launch(ctx, launchReq()); err != nil {
			t.Fatalf("a launch under the limit: %v", err)
		}
	}
	_, err := s.Launch(ctx, launchReq())
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("code = %s, want ResourceExhausted (%v)", status.Code(err), err)
	}
	if !strings.Contains(err.Error(), "runs 2 Sandboxes and the limit is 2") {
		t.Fatalf("the message does not name the count and the limit: %v", err)
	}

	other := &Service{Client: s.Client, AuthDisabled: true, DefaultNamespace: "team-b", MaxSandboxesPerTenant: 2}
	if _, err := other.Launch(ctx, launchReq()); err != nil {
		t.Fatalf("a launch of another tenant: %v", err)
	}
}

// TestLaunch_AFinishedOrSuspendedSandboxHasNoPlace proves that only a
// Sandbox that holds a machine has a place in the count.
func TestLaunch_AFinishedOrSuspendedSandboxHasNoPlace(t *testing.T) {
	t.Parallel()
	for _, phase := range []setecv1alpha1.SandboxPhase{
		setecv1alpha1.SandboxPhaseFailed, setecv1alpha1.SandboxPhaseCompleted, setecv1alpha1.SandboxPhaseSuspended,
	} {
		t.Run(string(phase), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			s := limited(t, 1)
			if _, err := s.Launch(ctx, launchReq()); err != nil {
				t.Fatal(err)
			}
			setPhase(t, s.Client, "team-a", phase)
			if _, err := s.Launch(ctx, launchReq()); err != nil {
				t.Fatalf("a launch next to a %s Sandbox: %v", phase, err)
			}
		})
	}
	for _, phase := range []setecv1alpha1.SandboxPhase{
		setecv1alpha1.SandboxPhasePending, setecv1alpha1.SandboxPhaseRunning, setecv1alpha1.SandboxPhasePaused,
		setecv1alpha1.SandboxPhaseSnapshotting, setecv1alpha1.SandboxPhaseRestoring,
	} {
		t.Run(string(phase), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			s := limited(t, 1)
			if _, err := s.Launch(ctx, launchReq()); err != nil {
				t.Fatal(err)
			}
			setPhase(t, s.Client, "team-a", phase)
			if _, err := s.Launch(ctx, launchReq()); status.Code(err) != codes.ResourceExhausted {
				t.Fatalf("a launch next to a %s Sandbox: code = %s, want ResourceExhausted", phase, status.Code(err))
			}
		})
	}
}

// TestLaunch_LaunchesAtTheSameTimeDoNotPassTheLimit runs many launches at
// once against one frontend and requires that exactly the limit pass.
func TestLaunch_LaunchesAtTheSameTimeDoNotPassTheLimit(t *testing.T) {
	t.Parallel()
	const limit, calls = 3, 12
	s := limited(t, limit)
	var wg sync.WaitGroup
	var mu sync.Mutex
	passed, refused := 0, 0
	for range calls {
		wg.Go(func() {
			_, err := s.Launch(context.Background(), launchReq())
			mu.Lock()
			defer mu.Unlock()
			switch status.Code(err) {
			case codes.OK:
				passed++
			case codes.ResourceExhausted:
				refused++
			default:
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
	wg.Wait()
	if passed != limit || refused != calls-limit {
		t.Fatalf("passed = %d, refused = %d, want %d and %d", passed, refused, limit, calls-limit)
	}
}

// TestLaunch_NoLimitReadsNothing proves that the default sets no limit.
func TestLaunch_NoLimitReadsNothing(t *testing.T) {
	t.Parallel()
	s := limited(t, 0)
	for range 5 {
		if _, err := s.Launch(context.Background(), launchReq()); err != nil {
			t.Fatal(err)
		}
	}
}
