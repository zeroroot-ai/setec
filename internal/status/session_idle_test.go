// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package status

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
)

// idleTestNow is the fixed "current instant" every scenario reasons
// from, so the tables read as offsets rather than wall-clock values.
var idleTestNow = time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)

// sessionSandbox builds a session Sandbox created at the given offset
// before idleTestNow, optionally carrying a last-activity annotation.
func sessionSandbox(createdAgo time.Duration, lastActivity string) *setecv1alpha1.Sandbox {
	sb := &setecv1alpha1.Sandbox{}
	sb.CreationTimestamp = metav1.NewTime(idleTestNow.Add(-createdAgo))
	sb.Spec.Lifecycle = &setecv1alpha1.Lifecycle{Mode: setecv1alpha1.LifecycleModeSession}
	if lastActivity != "" {
		sb.Annotations = map[string]string{
			setecv1alpha1.AnnotationLastActivity: lastActivity,
		}
	}
	return sb
}

// classWithIdle builds a SandboxClass with the given sessionIdleTimeout.
func classWithIdle(d time.Duration) *setecv1alpha1.SandboxClass {
	return &setecv1alpha1.SandboxClass{
		Spec: setecv1alpha1.SandboxClassSpec{
			SessionIdleTimeout: &metav1.Duration{Duration: d},
		},
	}
}

func TestLastSessionActivity_PrefersFreshestSource(t *testing.T) {
	t.Parallel()

	started := metav1.NewTime(idleTestNow.Add(-30 * time.Minute))
	annotated := idleTestNow.Add(-5 * time.Minute)

	sb := sessionSandbox(time.Hour, annotated.Format(time.RFC3339))
	sb.Status.StartedAt = &started

	if got := LastSessionActivity(sb); !got.Equal(annotated) {
		t.Fatalf("LastSessionActivity = %v, want annotation time %v", got, annotated)
	}

	// Without the annotation, startedAt wins over creation.
	sb.Annotations = nil
	if got := LastSessionActivity(sb); !got.Equal(started.Time) {
		t.Fatalf("LastSessionActivity = %v, want startedAt %v", got, started.Time)
	}

	// A malformed annotation is ignored, not trusted.
	sb.Annotations = map[string]string{setecv1alpha1.AnnotationLastActivity: "yesterday-ish"}
	if got := LastSessionActivity(sb); !got.Equal(started.Time) {
		t.Fatalf("LastSessionActivity with malformed annotation = %v, want startedAt %v", got, started.Time)
	}
}

func TestSessionIdleDeadline_PolicyApplicability(t *testing.T) {
	t.Parallel()

	ephemeral := &setecv1alpha1.Sandbox{}
	ephemeral.CreationTimestamp = metav1.NewTime(idleTestNow.Add(-time.Hour))

	cases := []struct {
		name string
		sb   *setecv1alpha1.Sandbox
		cls  *setecv1alpha1.SandboxClass
		want bool
	}{
		{"ephemeral sandbox has no idle policy", ephemeral, classWithIdle(time.Minute), false},
		{"no class resolved", sessionSandbox(time.Hour, ""), nil, false},
		{"class without knob", sessionSandbox(time.Hour, ""), &setecv1alpha1.SandboxClass{}, false},
		{"zero duration disables", sessionSandbox(time.Hour, ""), classWithIdle(0), false},
		{"negative duration disables", sessionSandbox(time.Hour, ""), classWithIdle(-time.Minute), false},
		{"session with positive knob", sessionSandbox(time.Hour, ""), classWithIdle(time.Minute), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := SessionIdleDeadline(tc.sb, tc.cls); ok != tc.want {
				t.Fatalf("SessionIdleDeadline applicable = %v, want %v", ok, tc.want)
			}
		})
	}
}

func TestApplySessionIdlePolicy(t *testing.T) {
	t.Parallel()

	running := setecv1alpha1.SandboxStatus{Phase: setecv1alpha1.SandboxPhaseRunning}

	t.Run("idle session past deadline fails with IdleTimeout", func(t *testing.T) {
		t.Parallel()
		sb := sessionSandbox(2*time.Hour, idleTestNow.Add(-30*time.Minute).Format(time.RFC3339))
		got := ApplySessionIdlePolicy(sb, classWithIdle(15*time.Minute), running, idleTestNow)
		if got.Phase != setecv1alpha1.SandboxPhaseFailed || got.Reason != ReasonIdleTimeout {
			t.Fatalf("status = %s/%s, want Failed/%s", got.Phase, got.Reason, ReasonIdleTimeout)
		}
		if got.LastTransitionTime == nil || !got.LastTransitionTime.Time.Equal(idleTestNow) {
			t.Fatalf("LastTransitionTime = %v, want %v", got.LastTransitionTime, idleTestNow)
		}
	})

	t.Run("recent activity keeps the session alive", func(t *testing.T) {
		t.Parallel()
		sb := sessionSandbox(2*time.Hour, idleTestNow.Add(-5*time.Minute).Format(time.RFC3339))
		got := ApplySessionIdlePolicy(sb, classWithIdle(15*time.Minute), running, idleTestNow)
		if got.Phase != setecv1alpha1.SandboxPhaseRunning {
			t.Fatalf("phase = %s, want Running (active sessions are never idle-reaped)", got.Phase)
		}
	})

	t.Run("fresh session gets a full idle window from creation", func(t *testing.T) {
		t.Parallel()
		sb := sessionSandbox(5*time.Minute, "")
		got := ApplySessionIdlePolicy(sb, classWithIdle(15*time.Minute), running, idleTestNow)
		if got.Phase != setecv1alpha1.SandboxPhaseRunning {
			t.Fatalf("phase = %s, want Running", got.Phase)
		}
	})

	t.Run("only Running is evaluated", func(t *testing.T) {
		t.Parallel()
		sb := sessionSandbox(2*time.Hour, "")
		for _, phase := range []setecv1alpha1.SandboxPhase{
			setecv1alpha1.SandboxPhasePending,
			setecv1alpha1.SandboxPhasePaused,
			setecv1alpha1.SandboxPhaseSnapshotting,
			setecv1alpha1.SandboxPhaseRestoring,
			setecv1alpha1.SandboxPhaseCompleted,
			setecv1alpha1.SandboxPhaseFailed,
		} {
			in := setecv1alpha1.SandboxStatus{Phase: phase}
			if got := ApplySessionIdlePolicy(sb, classWithIdle(time.Minute), in, idleTestNow); got.Phase != phase {
				t.Fatalf("phase %s mutated to %s; idle policy must only touch Running", phase, got.Phase)
			}
		}
	})

	t.Run("no class means never evicted", func(t *testing.T) {
		t.Parallel()
		sb := sessionSandbox(200*time.Hour, "")
		if got := ApplySessionIdlePolicy(sb, nil, running, idleTestNow); got.Phase != setecv1alpha1.SandboxPhaseRunning {
			t.Fatalf("phase = %s, want Running when no class resolved", got.Phase)
		}
	})
}

// TestApplySessionIdlePolicy_CheckpointClassDefersToSuspend asserts
// that a class enabling sessionCheckpoint opts its sessions out of
// hard idle eviction: the suspend machinery (setec#194) owns the idle
// deadline instead, so the derived status passes through untouched no
// matter how stale the activity clock is.
func TestApplySessionIdlePolicy_CheckpointClassDefersToSuspend(t *testing.T) {
	sb := &setecv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			CreationTimestamp: metav1.NewTime(time.Now().Add(-24 * time.Hour)),
		},
		Spec: setecv1alpha1.SandboxSpec{
			Lifecycle: &setecv1alpha1.Lifecycle{Mode: setecv1alpha1.LifecycleModeSession},
		},
	}
	cls := &setecv1alpha1.SandboxClass{
		Spec: setecv1alpha1.SandboxClassSpec{
			SessionIdleTimeout: &metav1.Duration{Duration: time.Minute},
			SessionCheckpoint:  &setecv1alpha1.SessionCheckpointSpec{Backend: "s3"},
		},
	}
	in := setecv1alpha1.SandboxStatus{Phase: setecv1alpha1.SandboxPhaseRunning}
	out := ApplySessionIdlePolicy(sb, cls, in, time.Now())
	if out.Phase != setecv1alpha1.SandboxPhaseRunning || out.Reason != "" {
		t.Fatalf("checkpoint-enabled class must not idle-evict; got %s/%s", out.Phase, out.Reason)
	}
}
