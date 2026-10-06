// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package status

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
)

// withReadinessProbe gives the Pod's workload container a readiness
// probe, as podspec does for the launcher container.
func withReadinessProbe(p *corev1.Pod) {
	p.Spec.Containers = []corev1.Container{{
		Name: "workload",
		ReadinessProbe: &corev1.Probe{
			Exec: &corev1.ExecAction{Command: []string{"/setec/keepalive/setec-keepalive", "--workspace-ready", "/workspace"}}},
	}}
}

func withRunning(ready corev1.ConditionStatus) func(*corev1.Pod) {
	return func(p *corev1.Pod) {
		p.Status.Phase = corev1.PodRunning
		p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: ready}}
	}
}

// TestDerive_ProbedPodIsPendingUntilReady asserts that a Running Pod
// whose container declares a readiness probe does not make the Sandbox
// Running until the Pod is Ready. The launcher container is Ready once
// the guest agent answers, and a turn that runs earlier has no machine.
func TestDerive_ProbedPodIsPendingUntilReady(t *testing.T) {
	sb := newSandbox()

	notReady := Derive(sb, newPod(withReadinessProbe, withRunning(corev1.ConditionFalse)), t0)
	if notReady.Phase != setecv1alpha1.SandboxPhasePending {
		t.Fatalf("probed Running Pod, not Ready: phase = %q, want Pending", notReady.Phase)
	}
	if notReady.StartedAt != nil {
		t.Errorf("probed Running Pod, not Ready: startedAt = %v, want unset until Running", notReady.StartedAt)
	}

	ready := Derive(sb, newPod(withReadinessProbe, withRunning(corev1.ConditionTrue)), t0)
	if ready.Phase != setecv1alpha1.SandboxPhaseRunning {
		t.Fatalf("probed Running Pod, Ready: phase = %q, want Running", ready.Phase)
	}
}

// TestDerive_UnprobedPodIsRunningAtOnce asserts that a Pod with no
// readiness probe keeps the old mapping: Running as soon as the Pod is.
func TestDerive_UnprobedPodIsRunningAtOnce(t *testing.T) {
	got := Derive(newSandbox(), newPod(withRunning(corev1.ConditionFalse)), t0)
	if got.Phase != setecv1alpha1.SandboxPhaseRunning {
		t.Fatalf("unprobed Running Pod: phase = %q, want Running", got.Phase)
	}
}
