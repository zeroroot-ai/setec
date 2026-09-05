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

package netpol_test

import (
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/zeroroot-ai/setec/internal/netpol"
)

// TestNamespaceBaselineSelectsEveryPod is the whole point of the object:
// a selector that matched a label would only confine workloads that
// cooperate by wearing it. If this test is ever "fixed" by adding a
// matchLabels or matchExpressions entry, the control is gone.
func TestNamespaceBaselineSelectsEveryPod(t *testing.T) {
	np := netpol.NamespaceBaseline("sandboxes")

	if got := len(np.Spec.PodSelector.MatchLabels); got != 0 {
		t.Fatalf("podSelector.matchLabels must be empty so the policy selects every Pod; got %d entries", got)
	}
	if got := len(np.Spec.PodSelector.MatchExpressions); got != 0 {
		t.Fatalf("podSelector.matchExpressions must be empty so the policy selects every Pod; got %d entries", got)
	}
}

func TestNamespaceBaselineDeniesBothDirections(t *testing.T) {
	np := netpol.NamespaceBaseline("sandboxes")

	if len(np.Spec.Ingress) != 0 {
		t.Fatalf("baseline must carry no ingress rules; got %d", len(np.Spec.Ingress))
	}
	if len(np.Spec.Egress) != 0 {
		t.Fatalf("baseline must carry no egress rules; got %d", len(np.Spec.Egress))
	}
	var hasIngress, hasEgress bool
	for _, pt := range np.Spec.PolicyTypes {
		switch pt {
		case networkingv1.PolicyTypeIngress:
			hasIngress = true
		case networkingv1.PolicyTypeEgress:
			hasEgress = true
		}
	}
	if !hasIngress || !hasEgress {
		t.Fatalf("both policy types must be listed — an unlisted type means allow-all for that direction; got %v", np.Spec.PolicyTypes)
	}
	if np.Namespace != "sandboxes" || np.Name != netpol.BaselineName {
		t.Fatalf("unexpected identity: %s/%s", np.Namespace, np.Name)
	}
}

func TestBaselineIsIntact(t *testing.T) {
	both := []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress}

	cases := []struct {
		name string
		np   *networkingv1.NetworkPolicy
		want bool
	}{
		{
			name: "generated baseline",
			np:   netpol.NamespaceBaseline("sandboxes"),
			want: true,
		},
		{
			name: "nil",
			np:   nil,
			want: false,
		},
		{
			name: "selector narrowed to a label",
			np: &networkingv1.NetworkPolicy{Spec: networkingv1.NetworkPolicySpec{
				PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"setec.zeroroot.ai/sandbox": "x"}},
				PolicyTypes: both,
			}},
			want: false,
		},
		{
			name: "selector narrowed by expression",
			np: &networkingv1.NetworkPolicy{Spec: networkingv1.NetworkPolicySpec{
				PodSelector: metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{
					Key: "setec.zeroroot.ai/sandbox", Operator: metav1.LabelSelectorOpExists,
				}}},
				PolicyTypes: both,
			}},
			want: false,
		},
		{
			name: "egress dropped from policyTypes",
			np: &networkingv1.NetworkPolicy{Spec: networkingv1.NetworkPolicySpec{
				PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			}},
			want: false,
		},
		{
			name: "an allow rule was added",
			np: &networkingv1.NetworkPolicy{Spec: networkingv1.NetworkPolicySpec{
				PolicyTypes: both,
				Egress:      []networkingv1.NetworkPolicyEgressRule{{}},
			}},
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := netpol.BaselineIsIntact(tc.np); got != tc.want {
				t.Fatalf("BaselineIsIntact = %v, want %v", got, tc.want)
			}
		})
	}
}
