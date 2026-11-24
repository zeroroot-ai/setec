// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package status

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
	"github.com/zeroroot-ai/setec/internal/podspec"
)

func gatedPod(mutators ...func(*corev1.Pod)) *corev1.Pod {
	return newPod(append([]func(*corev1.Pod){func(p *corev1.Pod) {
		p.Annotations = map[string]string{podspec.PolicyGateAnnotation: "true"}
		p.Spec.Containers = []corev1.Container{{Name: "launcher", ReadinessProbe: &corev1.Probe{}}}
	}}, mutators...)...)
}

func policyCondition(st setecv1alpha1.SandboxStatus) *metav1.Condition {
	return meta.FindStatusCondition(st.Conditions, setecv1alpha1.ConditionNetworkPolicyEnforced)
}

func TestPolicyCondition(t *testing.T) {
	t.Parallel()
	running := func(ready corev1.ConditionStatus) func(*corev1.Pod) {
		return func(p *corev1.Pod) {
			p.Status.Phase = corev1.PodRunning
			p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: ready}}
		}
	}
	failedWith := func(msg string) func(*corev1.Pod) {
		return func(p *corev1.Pod) {
			p.Status.Phase = corev1.PodFailed
			p.Status.ContainerStatuses = []corev1.ContainerStatus{{
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
					ExitCode: 125, Reason: "Error", Message: msg,
				}},
			}}
		}
	}
	for name, tc := range map[string]struct {
		pod       *corev1.Pod
		status    metav1.ConditionStatus // "" for no condition
		reason    string
		phase     setecv1alpha1.SandboxPhase
		inMessage string
	}{
		"waits while the Pod is not ready": {
			pod: gatedPod(running(corev1.ConditionFalse)), status: metav1.ConditionUnknown,
			reason: setecv1alpha1.ReasonNetworkPolicyWaiting, phase: setecv1alpha1.SandboxPhasePending,
		},
		"is proven when the Pod is ready": {
			pod: gatedPod(running(corev1.ConditionTrue)), status: metav1.ConditionTrue,
			reason: setecv1alpha1.ReasonNetworkPolicyProven, phase: setecv1alpha1.SandboxPhaseRunning,
		},
		"fails with the reason and the text of the launcher": {
			pod:    gatedPod(failedWith("NetworkPolicyNotEnforced: tcp://10.43.0.1:443 is reachable and the policy must block it")),
			status: metav1.ConditionFalse, reason: setecv1alpha1.ReasonNetworkPolicyNotEnforced,
			phase: setecv1alpha1.SandboxPhaseFailed, inMessage: "tcp://10.43.0.1:443 is reachable",
		},
		"a Pod that failed for another reason has no False condition": {
			pod: gatedPod(failedWith("VMMStartFailed: no kvm")), status: "",
			phase: setecv1alpha1.SandboxPhaseFailed,
		},
		"a Pod with no gate has no condition": {
			pod: newPod(running(corev1.ConditionTrue), func(p *corev1.Pod) {
				p.Spec.Containers = []corev1.Container{{Name: "launcher", ReadinessProbe: &corev1.Probe{}}}
			}),
			status: "", phase: setecv1alpha1.SandboxPhaseRunning,
		},
	} {
		got := Derive(newSandbox(), tc.pod, tMin(1))
		if got.Phase != tc.phase {
			t.Errorf("%s: phase %q, want %q", name, got.Phase, tc.phase)
		}
		c := policyCondition(got)
		if tc.status == "" {
			if c != nil {
				t.Errorf("%s: unexpected condition %+v", name, c)
			}
			continue
		}
		if c == nil || c.Status != tc.status || c.Reason != tc.reason || !strings.Contains(c.Message, tc.inMessage) {
			t.Errorf("%s: condition %+v, want %s %s containing %q", name, c, tc.status, tc.reason, tc.inMessage)
		}
		if tc.status == metav1.ConditionFalse && got.Reason != setecv1alpha1.ReasonNetworkPolicyNotEnforced {
			t.Errorf("%s: Sandbox reason %q, want the reason of the condition", name, got.Reason)
		}
	}
}

// TestPolicyConditionIsSticky: a terminal Sandbox keeps its condition.
func TestPolicyConditionIsSticky(t *testing.T) {
	t.Parallel()
	failed := Derive(newSandbox(), gatedPod(func(p *corev1.Pod) {
		p.Status.Phase = corev1.PodFailed
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{State: corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{ExitCode: 125, Message: "NetworkPolicyNotEnforced: x"},
		}}}
	}), tMin(1))
	again := Derive(newSandbox(withStatus(failed)), nil, tMin(2))
	if c := policyCondition(again); c == nil || c.Status != metav1.ConditionFalse {
		t.Fatalf("the condition of a terminal Sandbox changed: %+v", c)
	}
}
