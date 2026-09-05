// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package webhook

import (
	"context"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
	"github.com/zeroroot-ai/setec/internal/class"
)

// TestSandboxClassSessionCheckpointValidation covers the coherence
// rules of spec.sessionCheckpoint.
func TestSandboxClassSessionCheckpointValidation(t *testing.T) {
	cases := []struct {
		name    string
		spec    *setecv1alpha1.SessionCheckpointSpec
		wantErr string
	}{
		{name: "nil disables checkpoints", spec: nil},
		{name: "empty spec ok", spec: &setecv1alpha1.SessionCheckpointSpec{}},
		{name: "positive interval ok", spec: &setecv1alpha1.SessionCheckpointSpec{
			Interval: &metav1.Duration{Duration: 10 * time.Minute},
		}},
		{name: "zero interval rejected", spec: &setecv1alpha1.SessionCheckpointSpec{
			Interval: &metav1.Duration{Duration: 0},
		}, wantErr: "interval"},
		{name: "negative interval rejected", spec: &setecv1alpha1.SessionCheckpointSpec{
			Interval: &metav1.Duration{Duration: -time.Minute},
		}, wantErr: "interval"},
		{name: "s3 backend ok", spec: &setecv1alpha1.SessionCheckpointSpec{Backend: "s3"}},
		{name: "unknown backend rejected", spec: &setecv1alpha1.SessionCheckpointSpec{Backend: "nfs"},
			wantErr: "backend"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cls := &setecv1alpha1.SandboxClass{
				ObjectMeta: metav1.ObjectMeta{Name: "c"},
				Spec:       setecv1alpha1.SandboxClassSpec{SessionCheckpoint: tc.spec},
			}
			errs := validateSessionCheckpoint(cls)
			if tc.wantErr == "" {
				if len(errs) != 0 {
					t.Fatalf("unexpected errors: %v", errs)
				}
				return
			}
			if len(errs) == 0 {
				t.Fatal("expected a validation error")
			}
			if !strings.Contains(errs.ToAggregate().Error(), tc.wantErr) {
				t.Fatalf("error %q does not mention %q", errs.ToAggregate().Error(), tc.wantErr)
			}
		})
	}
}

// TestCheckpointBackendDefault pins the effective-backend helper.
func TestCheckpointBackendDefault(t *testing.T) {
	var nilSpec *setecv1alpha1.SessionCheckpointSpec
	if got := nilSpec.CheckpointBackend(); got != "s3" {
		t.Fatalf("nil spec backend = %q, want s3", got)
	}
	if got := (&setecv1alpha1.SessionCheckpointSpec{}).CheckpointBackend(); got != "s3" {
		t.Fatalf("empty backend = %q, want s3", got)
	}
	if got := (&setecv1alpha1.SessionCheckpointSpec{Backend: "s3"}).CheckpointBackend(); got != "s3" {
		t.Fatalf("explicit backend = %q, want s3", got)
	}
}

// TestValidateCreate_SuspendedDesiredState covers the suspend
// admission rules: Suspended requires the session lifecycle AND a
// class that enables sessionCheckpoint.
func TestValidateCreate_SuspendedDesiredState(t *testing.T) {
	t.Parallel()

	withCheckpoint := func(c *setecv1alpha1.SandboxClass) *setecv1alpha1.SandboxClass {
		out := c.DeepCopy()
		out.Spec.SessionCheckpoint = &setecv1alpha1.SessionCheckpointSpec{Backend: "s3"}
		return out
	}
	suspended := func(sb *setecv1alpha1.Sandbox) *setecv1alpha1.Sandbox {
		out := sb.DeepCopy()
		out.Spec.DesiredState = setecv1alpha1.SandboxDesiredStateSuspended
		return out
	}

	t.Run("ephemeral suspended denied", func(t *testing.T) {
		v := lifecycleValidator(t)
		sb := suspended(mkSandbox("standard", 2, "2Gi", ""))
		if _, err := v.ValidateCreate(context.Background(), sb); err == nil ||
			!strings.Contains(err.Error(), "session") {
			t.Fatalf("want session-required rejection, got %v", err)
		}
	})

	t.Run("session without class checkpoint denied", func(t *testing.T) {
		v := lifecycleValidator(t)
		sb := suspended(withMode(mkSandbox("standard", 2, "2Gi", ""), setecv1alpha1.LifecycleModeSession))
		if _, err := v.ValidateCreate(context.Background(), sb); err == nil ||
			!strings.Contains(err.Error(), "sessionCheckpoint") {
			t.Fatalf("want sessionCheckpoint-required rejection, got %v", err)
		}
	})

	t.Run("session with class checkpoint admitted", func(t *testing.T) {
		seed := []client.Object{withCheckpoint(mkClass("standard", true, "8Gi"))}
		v := &SandboxValidator{Resolver: class.NewResolver(newFakeClient(t, seed...))}
		sb := suspended(withMode(mkSandbox("standard", 2, "2Gi", ""), setecv1alpha1.LifecycleModeSession))
		if _, err := v.ValidateCreate(context.Background(), sb); err != nil {
			t.Fatalf("want admission, got %v", err)
		}
	})
}
