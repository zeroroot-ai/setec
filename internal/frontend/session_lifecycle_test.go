// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package frontend

import (
	"context"
	"testing"

	setecv1grpc "github.com/zeroroot-ai/setec/api/grpc/v1"
	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// launchAndFetch launches a Sandbox via the frontend and returns the
// created CR read back from the fake client.
func launchAndFetch(t *testing.T, req *setecv1grpc.LaunchRequest) *setecv1alpha1.Sandbox {
	t.Helper()
	c := newClient(t)
	s := &Service{Client: c, AuthDisabled: true, DefaultNamespace: "team-a"}

	resp, err := s.Launch(context.Background(), req)
	if err != nil {
		t.Fatalf("Launch(): %v", err)
	}
	list := &setecv1alpha1.SandboxList{}
	if err := c.List(context.Background(), list, client.InNamespace(resp.Namespace)); err != nil {
		t.Fatalf("list sandboxes: %v", err)
	}
	if len(list.Items) != 1 {
		t.Fatalf("expected exactly one Sandbox, got %d", len(list.Items))
	}
	return &list.Items[0]
}

// TestLaunch_SessionLifecycleMapped asserts the wire lifecycle mode and
// workspace fields land on the Sandbox CR.
func TestLaunch_SessionLifecycleMapped(t *testing.T) {
	t.Parallel()
	sb := launchAndFetch(t, &setecv1grpc.LaunchRequest{
		Image:     "alpine:3.19",
		Command:   []string{"sh", "-c", "sleep infinity"},
		Resources: &setecv1grpc.Resources{Vcpu: 1, Memory: "256Mi"},
		Lifecycle: &setecv1grpc.Lifecycle{
			Mode: "session",
			Workspace: &setecv1grpc.Workspace{
				Size:             "20Gi",
				StorageClassName: "encrypted-gp3",
			},
		},
	})

	if !sb.Spec.IsSession() {
		t.Fatalf("effective mode = %q, want session", sb.Spec.EffectiveLifecycleMode())
	}
	ws := sb.Spec.Lifecycle.Workspace
	if ws == nil {
		t.Fatalf("spec.lifecycle.workspace is nil")
	}
	if ws.Size == nil || ws.Size.String() != "20Gi" {
		t.Errorf("workspace size = %v, want 20Gi", ws.Size)
	}
	if ws.StorageClassName == nil || *ws.StorageClassName != "encrypted-gp3" {
		t.Errorf("storageClassName = %v, want encrypted-gp3", ws.StorageClassName)
	}
}

// TestLaunch_DefaultLifecycleStaysUnset asserts a request without a
// lifecycle message (and one with an empty message) leaves the CR's
// lifecycle unset so the ephemeral default applies untouched.
func TestLaunch_DefaultLifecycleStaysUnset(t *testing.T) {
	t.Parallel()
	for name, lc := range map[string]*setecv1grpc.Lifecycle{
		"absent": nil,
		"empty":  {},
	} {
		sb := launchAndFetch(t, &setecv1grpc.LaunchRequest{
			Image:     "alpine:3.19",
			Command:   []string{"true"},
			Resources: &setecv1grpc.Resources{Vcpu: 1, Memory: "256Mi"},
			Lifecycle: lc,
		})
		if sb.Spec.Lifecycle != nil && (sb.Spec.Lifecycle.Mode != "" || sb.Spec.Lifecycle.Workspace != nil || sb.Spec.Lifecycle.Timeout != nil) {
			t.Errorf("%s lifecycle: spec.lifecycle = %+v, want unset", name, sb.Spec.Lifecycle)
		}
		if sb.Spec.EffectiveLifecycleMode() != setecv1alpha1.LifecycleModeEphemeral {
			t.Errorf("%s lifecycle: effective mode = %q, want ephemeral", name, sb.Spec.EffectiveLifecycleMode())
		}
	}
}

// TestLaunch_LifecycleValidation asserts the frontend rejects a bad
// mode, a workspace without session mode, and a malformed size.
func TestLaunch_LifecycleValidation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		lc   *setecv1grpc.Lifecycle
	}{
		{"unknown mode", &setecv1grpc.Lifecycle{Mode: "perpetual"}},
		{"workspace without session", &setecv1grpc.Lifecycle{Workspace: &setecv1grpc.Workspace{Size: "10Gi"}}},
		{"workspace on ephemeral", &setecv1grpc.Lifecycle{Mode: "ephemeral", Workspace: &setecv1grpc.Workspace{}}},
		{"bad size", &setecv1grpc.Lifecycle{Mode: "session", Workspace: &setecv1grpc.Workspace{Size: "ten-gigs"}}},
		{"bad timeout", &setecv1grpc.Lifecycle{Timeout: "soon"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &Service{Client: newClient(t), AuthDisabled: true, DefaultNamespace: "team-a"}
			_, err := s.Launch(context.Background(), &setecv1grpc.LaunchRequest{
				Image:     "alpine:3.19",
				Command:   []string{"true"},
				Resources: &setecv1grpc.Resources{Vcpu: 1, Memory: "256Mi"},
				Lifecycle: tc.lc,
			})
			if status.Code(err) != codes.InvalidArgument {
				t.Fatalf("code = %s (err=%v), want InvalidArgument", status.Code(err), err)
			}
		})
	}
}
