// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package status

import (
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
	"github.com/zeroroot-ai/setec/internal/podspec"
)

// applyPolicyCondition sets the condition NetworkPolicyEnforced of a Sandbox
// whose Pod runs a policy gate (docs/design/network-policy.md). The launcher
// holds the machine off the network, and the Pod not Ready, until a probe shows
// that the network plugin enforces the policy. So:
//
//   - A Pod that runs and is not yet Ready: Unknown, WaitingForPolicy.
//   - A Pod that is Ready: True, PolicyProven.
//   - A Pod that failed with the launcher reason NetworkPolicyNotEnforced: False,
//     and the Sandbox is Failed with that reason. The message is the text that
//     the launcher wrote, which names the destination that failed its check.
//
// A Sandbox with no Pod, a Pod with no gate, and a Sandbox that is already
// terminal keep their conditions.
func applyPolicyCondition(
	sb *setecv1alpha1.Sandbox, pod *corev1.Pod, out setecv1alpha1.SandboxStatus, now time.Time,
) setecv1alpha1.SandboxStatus {
	if pod == nil || pod.Annotations[podspec.PolicyGateAnnotation] != "true" || isTerminal(sb.Status.Phase) {
		return out
	}
	cond := metav1.Condition{
		Type:               setecv1alpha1.ConditionNetworkPolicyEnforced,
		ObservedGeneration: sb.Generation,
	}
	switch {
	case pod.Status.Phase == corev1.PodFailed:
		msg, ok := policyFailure(pod)
		if !ok {
			return out
		}
		cond.Status, cond.Reason, cond.Message = metav1.ConditionFalse, setecv1alpha1.ReasonNetworkPolicyNotEnforced, msg
		out = setPhase(out, setecv1alpha1.SandboxPhaseFailed, setecv1alpha1.ReasonNetworkPolicyNotEnforced, now)
	case podReady(pod):
		cond.Status, cond.Reason = metav1.ConditionTrue, setecv1alpha1.ReasonNetworkPolicyProven
		cond.Message = "the launcher proved that the network plugin enforces the policy before the machine got network access"
	case pod.Status.Phase == corev1.PodRunning:
		cond.Status, cond.Reason = metav1.ConditionUnknown, setecv1alpha1.ReasonNetworkPolicyWaiting
		cond.Message = "the launcher waits for a probe to show that the network plugin enforces the policy"
	default:
		return out
	}
	conds := append([]metav1.Condition(nil), out.Conditions...)
	meta.SetStatusCondition(&conds, cond)
	out.Conditions = conds
	return out
}

// policyFailure returns the message of a launcher container that ended with the
// reason NetworkPolicyNotEnforced, without the reason, and true.
func policyFailure(pod *corev1.Pod) (string, bool) {
	prefix := setecv1alpha1.ReasonNetworkPolicyNotEnforced + ":"
	for _, cs := range pod.Status.ContainerStatuses {
		if t := cs.State.Terminated; t != nil && strings.HasPrefix(t.Message, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(t.Message, prefix)), true
		}
	}
	return "", false
}
