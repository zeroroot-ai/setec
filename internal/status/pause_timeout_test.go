/*
Copyright 2026 The Setec Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package status

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
)

// pauseClass builds a SandboxClass with the given maxPauseDuration
// pointer (nil = knob absent).
func pauseClass(max *time.Duration) *setecv1alpha1.SandboxClass {
	cls := &setecv1alpha1.SandboxClass{
		ObjectMeta: metav1.ObjectMeta{Name: "pause-class"},
	}
	if max != nil {
		cls.Spec.MaxPauseDuration = &metav1.Duration{Duration: *max}
	}
	return cls
}

func durPtr(d time.Duration) *time.Duration { return &d }

func TestPauseDeadline(t *testing.T) {
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	pausedAt := metav1.NewTime(now.Add(-10 * time.Minute))

	tests := []struct {
		name     string
		st       setecv1alpha1.SandboxStatus
		cls      *setecv1alpha1.SandboxClass
		wantOK   bool
		wantTime time.Time
	}{
		{
			name:     "pausedAt + positive cap → deadline",
			st:       setecv1alpha1.SandboxStatus{PausedAt: &pausedAt},
			cls:      pauseClass(durPtr(30 * time.Minute)),
			wantOK:   true,
			wantTime: pausedAt.Add(30 * time.Minute),
		},
		{
			name:   "no pausedAt → no bound",
			st:     setecv1alpha1.SandboxStatus{},
			cls:    pauseClass(durPtr(30 * time.Minute)),
			wantOK: false,
		},
		{
			name:   "nil class → no bound",
			st:     setecv1alpha1.SandboxStatus{PausedAt: &pausedAt},
			cls:    nil,
			wantOK: false,
		},
		{
			name:   "cap unset → no bound",
			st:     setecv1alpha1.SandboxStatus{PausedAt: &pausedAt},
			cls:    pauseClass(nil),
			wantOK: false,
		},
		{
			name:   "cap zero → no bound (fail-open)",
			st:     setecv1alpha1.SandboxStatus{PausedAt: &pausedAt},
			cls:    pauseClass(durPtr(0)),
			wantOK: false,
		},
		{
			name:   "cap negative → no bound (fail-open)",
			st:     setecv1alpha1.SandboxStatus{PausedAt: &pausedAt},
			cls:    pauseClass(durPtr(-time.Minute)),
			wantOK: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := PauseDeadline(tc.st, tc.cls)
			if ok != tc.wantOK {
				t.Fatalf("PauseDeadline ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && !got.Equal(tc.wantTime) {
				t.Fatalf("PauseDeadline = %v, want %v", got, tc.wantTime)
			}
		})
	}
}

func TestApplyPausePolicy(t *testing.T) {
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	overdue := metav1.NewTime(now.Add(-time.Hour))
	fresh := metav1.NewTime(now.Add(-time.Minute))

	session := &setecv1alpha1.Sandbox{
		Spec: setecv1alpha1.SandboxSpec{
			Lifecycle: &setecv1alpha1.Lifecycle{
				Mode: setecv1alpha1.LifecycleModeSession,
			},
		},
	}
	ephemeral := &setecv1alpha1.Sandbox{}

	withCheckpoint := pauseClass(durPtr(30 * time.Minute))
	withCheckpoint.Spec.SessionCheckpoint = &setecv1alpha1.SessionCheckpointSpec{}

	paused := func(at metav1.Time) setecv1alpha1.SandboxStatus {
		return setecv1alpha1.SandboxStatus{
			Phase:    setecv1alpha1.SandboxPhasePaused,
			Reason:   "UserPaused",
			PausedAt: &at,
		}
	}

	tests := []struct {
		name       string
		sb         *setecv1alpha1.Sandbox
		cls        *setecv1alpha1.SandboxClass
		in         setecv1alpha1.SandboxStatus
		wantPhase  setecv1alpha1.SandboxPhase
		wantReason string
	}{
		{
			name:       "paused past cap → Failed/PauseTimeoutExceeded",
			sb:         ephemeral,
			cls:        pauseClass(durPtr(30 * time.Minute)),
			in:         paused(overdue),
			wantPhase:  setecv1alpha1.SandboxPhaseFailed,
			wantReason: ReasonPauseTimeout,
		},
		{
			name:       "paused within cap → untouched",
			sb:         ephemeral,
			cls:        pauseClass(durPtr(30 * time.Minute)),
			in:         paused(fresh),
			wantPhase:  setecv1alpha1.SandboxPhasePaused,
			wantReason: "UserPaused",
		},
		{
			name:       "paused, cap unset → untouched",
			sb:         ephemeral,
			cls:        pauseClass(nil),
			in:         paused(overdue),
			wantPhase:  setecv1alpha1.SandboxPhasePaused,
			wantReason: "UserPaused",
		},
		{
			name:       "non-checkpoint session past cap → Failed (IdleTimeout precedent)",
			sb:         session,
			cls:        pauseClass(durPtr(30 * time.Minute)),
			in:         paused(overdue),
			wantPhase:  setecv1alpha1.SandboxPhaseFailed,
			wantReason: ReasonPauseTimeout,
		},
		{
			name:       "checkpoint session past cap → untouched (suspend machinery owns it)",
			sb:         session,
			cls:        withCheckpoint,
			in:         paused(overdue),
			wantPhase:  setecv1alpha1.SandboxPhasePaused,
			wantReason: "UserPaused",
		},
		{
			name:       "ephemeral in checkpoint-enabled class past cap → Failed (suspend serves sessions only)",
			sb:         ephemeral,
			cls:        withCheckpoint,
			in:         paused(overdue),
			wantPhase:  setecv1alpha1.SandboxPhaseFailed,
			wantReason: ReasonPauseTimeout,
		},
		{
			name: "Running past a stale pausedAt → untouched (only Paused is bounded)",
			sb:   ephemeral,
			cls:  pauseClass(durPtr(30 * time.Minute)),
			in: setecv1alpha1.SandboxStatus{
				Phase:    setecv1alpha1.SandboxPhaseRunning,
				PausedAt: &overdue,
			},
			wantPhase:  setecv1alpha1.SandboxPhaseRunning,
			wantReason: "",
		},
		{
			name: "Suspended is never bounded by the pause cap",
			sb:   session,
			cls:  pauseClass(durPtr(30 * time.Minute)),
			in: setecv1alpha1.SandboxStatus{
				Phase:    setecv1alpha1.SandboxPhaseSuspended,
				Reason:   "SuspendedPauseTimeout",
				PausedAt: &overdue,
			},
			wantPhase:  setecv1alpha1.SandboxPhaseSuspended,
			wantReason: "SuspendedPauseTimeout",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out := ApplyPausePolicy(tc.sb, tc.cls, tc.in, now)
			if out.Phase != tc.wantPhase {
				t.Fatalf("phase = %q, want %q", out.Phase, tc.wantPhase)
			}
			if out.Reason != tc.wantReason {
				t.Fatalf("reason = %q, want %q", out.Reason, tc.wantReason)
			}
			if out.Phase == setecv1alpha1.SandboxPhaseFailed && tc.in.Phase != setecv1alpha1.SandboxPhaseFailed {
				if out.LastTransitionTime == nil || !out.LastTransitionTime.Time.Equal(now) {
					t.Fatalf("LastTransitionTime not bumped to now on the Failed transition")
				}
			}
		})
	}
}

// TestApplyPausePolicy_ExactDeadlineFires pins the boundary: at the
// exact deadline instant the cap fires (now.Before(deadline) is false).
func TestApplyPausePolicy_ExactDeadlineFires(t *testing.T) {
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	pausedAt := metav1.NewTime(now.Add(-30 * time.Minute))
	in := setecv1alpha1.SandboxStatus{
		Phase:    setecv1alpha1.SandboxPhasePaused,
		PausedAt: &pausedAt,
	}
	out := ApplyPausePolicy(&setecv1alpha1.Sandbox{}, pauseClass(durPtr(30*time.Minute)), in, now)
	if out.Phase != setecv1alpha1.SandboxPhaseFailed || out.Reason != ReasonPauseTimeout {
		t.Fatalf("at the exact deadline got %s/%s, want Failed/%s", out.Phase, out.Reason, ReasonPauseTimeout)
	}
}
