// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package status

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
)

// withSessionMode marks the Sandbox as session-lifecycle.
func withSessionMode() func(*setecv1alpha1.Sandbox) {
	return func(sb *setecv1alpha1.Sandbox) {
		if sb.Spec.Lifecycle == nil {
			sb.Spec.Lifecycle = &setecv1alpha1.Lifecycle{}
		}
		sb.Spec.Lifecycle.Mode = setecv1alpha1.LifecycleModeSession
	}
}

// TestDerive_SessionPodExitIsNotTerminal asserts that a session
// Sandbox's Pod reaching Succeeded or Failed maps to
// Pending/SessionVMRestarting rather than a terminal phase: sessions
// end only on explicit teardown (ADR-0006), so the controller restarts
// the VM against the durable workspace instead of finishing the
// Sandbox.
func TestDerive_SessionPodExitIsNotTerminal(t *testing.T) {
	t.Parallel()

	for _, podPhase := range []corev1.PodPhase{corev1.PodSucceeded, corev1.PodFailed} {
		sb := newSandbox(withSessionMode(), withStatus(setecv1alpha1.SandboxStatus{
			Phase:              setecv1alpha1.SandboxPhaseRunning,
			PodName:            "demo-vm",
			LastTransitionTime: ptrTime(t0),
		}))
		pod := newPod(func(p *corev1.Pod) {
			p.Status.Phase = podPhase
			p.Status.ContainerStatuses = []corev1.ContainerStatus{{
				State: corev1.ContainerState{
					Terminated: &corev1.ContainerStateTerminated{ExitCode: 0},
				},
			}}
		})

		got := Derive(sb, pod, tMin(1))
		if got.Phase != setecv1alpha1.SandboxPhasePending {
			t.Errorf("pod %s: phase = %q, want Pending", podPhase, got.Phase)
		}
		if got.Reason != ReasonSessionVMRestarting {
			t.Errorf("pod %s: reason = %q, want %q", podPhase, got.Reason, ReasonSessionVMRestarting)
		}
	}
}

// TestDerive_SessionTimeoutStillTerminal asserts the lifecycle timeout
// still fails a session Sandbox terminally: the restart loop must stay
// bounded by spec.lifecycle.timeout.
func TestDerive_SessionTimeoutStillTerminal(t *testing.T) {
	t.Parallel()

	sb := newSandbox(withSessionMode(), withTimeout(30*time.Minute), withStatus(setecv1alpha1.SandboxStatus{
		Phase:              setecv1alpha1.SandboxPhaseRunning,
		PodName:            "demo-vm",
		StartedAt:          ptrTime(t0),
		LastTransitionTime: ptrTime(t0),
	}))
	pod := newPod(func(p *corev1.Pod) {
		p.Status.Phase = corev1.PodRunning
		st := metav1.NewTime(t0)
		p.Status.StartTime = &st
	})

	got := Derive(sb, pod, t0.Add(31*time.Minute))
	if got.Phase != setecv1alpha1.SandboxPhaseFailed {
		t.Fatalf("phase = %q, want Failed", got.Phase)
	}
	if got.Reason != ReasonTimeout {
		t.Fatalf("reason = %q, want %q", got.Reason, ReasonTimeout)
	}
}

// TestDerive_EphemeralPodExitUnchanged asserts an explicit
// "ephemeral" mode derives exactly what the implicit default derives
// for a terminal Pod — the byte-for-byte back-compat contract.
func TestDerive_EphemeralPodExitUnchanged(t *testing.T) {
	t.Parallel()

	mk := func(mode setecv1alpha1.LifecycleMode) setecv1alpha1.SandboxStatus {
		mods := []func(*setecv1alpha1.Sandbox){withStatus(setecv1alpha1.SandboxStatus{
			Phase:              setecv1alpha1.SandboxPhaseRunning,
			PodName:            "demo-vm",
			LastTransitionTime: ptrTime(t0),
		})}
		if mode != "" {
			mods = append(mods, func(sb *setecv1alpha1.Sandbox) {
				sb.Spec.Lifecycle = &setecv1alpha1.Lifecycle{Mode: mode}
			})
		}
		sb := newSandbox(mods...)
		pod := newPod(func(p *corev1.Pod) {
			p.Status.Phase = corev1.PodSucceeded
		})
		return Derive(sb, pod, tMin(1))
	}

	implicit := mk("")
	explicit := mk(setecv1alpha1.LifecycleModeEphemeral)

	if implicit.Phase != setecv1alpha1.SandboxPhaseCompleted {
		t.Fatalf("implicit ephemeral phase = %q, want Completed", implicit.Phase)
	}
	if explicit.Phase != implicit.Phase || explicit.Reason != implicit.Reason {
		t.Fatalf("explicit ephemeral derives (%q,%q); implicit derives (%q,%q) — must match",
			explicit.Phase, explicit.Reason, implicit.Phase, implicit.Reason)
	}
}
