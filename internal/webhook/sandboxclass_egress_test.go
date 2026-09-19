// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package webhook

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
)

// kubeDNSAllowance is the well-formed reference entry: kube-dns Pods in
// kube-system on 53/UDP and 53/TCP.
func kubeDNSAllowance() setecv1alpha1.EgressAllowSelector {
	return setecv1alpha1.EgressAllowSelector{
		NamespaceSelector: &metav1.LabelSelector{
			MatchLabels: map[string]string{"kubernetes.io/metadata.name": "kube-system"},
		},
		PodSelector: &metav1.LabelSelector{
			MatchLabels: map[string]string{"k8s-app": "kube-dns"},
		},
		Ports: []setecv1alpha1.EgressAllowPort{
			{Protocol: corev1.ProtocolUDP, Port: intstr.FromInt32(53)},
			{Protocol: corev1.ProtocolTCP, Port: intstr.FromInt32(53)},
		},
	}
}

// TestSandboxClassWebhook_EgressAllowSelectors checks the shape rules for
// spec.egressAllowSelectors (setec#76): no empty peer, valid selectors,
// at least one well-formed port.
func TestSandboxClassWebhook_EgressAllowSelectors(t *testing.T) {
	t.Parallel()
	w := webhookWith(fakeClientWithNS(t), baseConfig())

	classWith := func(entries ...setecv1alpha1.EgressAllowSelector) *setecv1alpha1.SandboxClass {
		cls := mkSandboxClass("agent", setecv1alpha1.VMMFirecracker, mkRuntime("kata-fc"))
		cls.Spec.EgressAllowSelectors = entries
		return cls
	}

	accepted := map[string]setecv1alpha1.EgressAllowSelector{
		"kube-dns by number": kubeDNSAllowance(),
		"named port with the TCP default": {
			NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": "gibson"}},
			PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": "envoy"}},
			Ports:             []setecv1alpha1.EgressAllowPort{{Port: intstr.FromString("https")}},
		},
		"namespaceSelector alone": {
			NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"team": "platform"}},
			Ports:             []setecv1alpha1.EgressAllowPort{{Port: intstr.FromInt32(8443)}},
		},
		"podSelector alone": {
			PodSelector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
				{Key: "role", Operator: metav1.LabelSelectorOpIn, Values: []string{"edge"}},
			}},
			Ports: []setecv1alpha1.EgressAllowPort{{Protocol: corev1.ProtocolSCTP, Port: intstr.FromInt32(3868)}},
		},
	}
	for name, entry := range accepted {
		t.Run("accepts "+name, func(t *testing.T) {
			if _, err := w.ValidateCreate(context.Background(), classWith(entry)); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}

	rejected := map[string]struct {
		entry setecv1alpha1.EgressAllowSelector
		want  string
	}{
		"no selector at all": {
			entry: setecv1alpha1.EgressAllowSelector{
				Ports: []setecv1alpha1.EgressAllowPort{{Port: intstr.FromInt32(53)}},
			},
			want: "egressAllowSelectors[0]: Required value: at least one of namespaceSelector or podSelector",
		},
		"no ports": {
			entry: setecv1alpha1.EgressAllowSelector{
				NamespaceSelector: kubeDNSAllowance().NamespaceSelector,
				PodSelector:       kubeDNSAllowance().PodSelector,
			},
			want: "egressAllowSelectors[0].ports: Required value",
		},
		"port number out of range": {
			entry: func() setecv1alpha1.EgressAllowSelector {
				a := kubeDNSAllowance()
				a.Ports = []setecv1alpha1.EgressAllowPort{{Port: intstr.FromInt32(70000)}}
				return a
			}(),
			want: "egressAllowSelectors[0].ports[0].port: Invalid value: 70000",
		},
		"port number zero": {
			entry: func() setecv1alpha1.EgressAllowSelector {
				a := kubeDNSAllowance()
				a.Ports = []setecv1alpha1.EgressAllowPort{{Port: intstr.FromInt32(0)}}
				return a
			}(),
			want: "egressAllowSelectors[0].ports[0].port: Invalid value: 0",
		},
		"port name malformed": {
			entry: func() setecv1alpha1.EgressAllowSelector {
				a := kubeDNSAllowance()
				a.Ports = []setecv1alpha1.EgressAllowPort{{Port: intstr.FromString("Not_A_Port_Name_Because_It_Is_Far_Too_Long")}}
				return a
			}(),
			want: "egressAllowSelectors[0].ports[0].port: Invalid value",
		},
		"unknown protocol": {
			entry: func() setecv1alpha1.EgressAllowSelector {
				a := kubeDNSAllowance()
				a.Ports = []setecv1alpha1.EgressAllowPort{{Protocol: "ICMP", Port: intstr.FromInt32(53)}}
				return a
			}(),
			want: "egressAllowSelectors[0].ports[0].protocol: Unsupported value: \"ICMP\"",
		},
		"malformed selector": {
			entry: setecv1alpha1.EgressAllowSelector{
				PodSelector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
					{Key: "role", Operator: metav1.LabelSelectorOpExists, Values: []string{"must-be-empty"}},
				}},
				Ports: []setecv1alpha1.EgressAllowPort{{Port: intstr.FromInt32(53)}},
			},
			want: "egressAllowSelectors[0].podSelector.matchExpressions[0].values",
		},
	}
	for name, tc := range rejected {
		t.Run("rejects "+name, func(t *testing.T) {
			_, err := w.ValidateCreate(context.Background(), classWith(tc.entry))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}

	t.Run("reports every bad entry at once", func(t *testing.T) {
		_, err := w.ValidateCreate(context.Background(), classWith(
			setecv1alpha1.EgressAllowSelector{Ports: []setecv1alpha1.EgressAllowPort{{Port: intstr.FromInt32(53)}}},
			setecv1alpha1.EgressAllowSelector{NamespaceSelector: kubeDNSAllowance().NamespaceSelector},
		))
		if err == nil {
			t.Fatal("expected an error")
		}
		for _, want := range []string{"egressAllowSelectors[0]", "egressAllowSelectors[1].ports"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error lacks %q: %v", want, err)
			}
		}
	})

	t.Run("holds with Runtime nil", func(t *testing.T) {
		cls := mkSandboxClass("noruntime", setecv1alpha1.VMMFirecracker, nil)
		cls.Spec.EgressAllowSelectors = []setecv1alpha1.EgressAllowSelector{
			{Ports: []setecv1alpha1.EgressAllowPort{{Port: intstr.FromInt32(53)}}},
		}
		_, err := w.ValidateCreate(context.Background(), cls)
		if err == nil || !strings.Contains(err.Error(), "egressAllowSelectors[0]") {
			t.Fatalf("expected the selector error with nil Runtime, got %v", err)
		}
	})
}

// TestSandboxClassWebhook_EgressExemptCIDRs checks that a malformed
// exemption is refused at admission rather than at reconcile.
func TestSandboxClassWebhook_EgressExemptCIDRs(t *testing.T) {
	t.Parallel()
	w := webhookWith(fakeClientWithNS(t), baseConfig())

	t.Run("accepts prefixes", func(t *testing.T) {
		cls := mkSandboxClass("ok", setecv1alpha1.VMMFirecracker, mkRuntime("kata-fc"))
		cls.Spec.EgressExemptCIDRs = []string{"10.96.0.0/12", "10.96.0.250/32"}
		if _, err := w.ValidateCreate(context.Background(), cls); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("rejects a bare address and garbage", func(t *testing.T) {
		cls := mkSandboxClass("bad", setecv1alpha1.VMMFirecracker, mkRuntime("kata-fc"))
		cls.Spec.EgressExemptCIDRs = []string{"10.96.0.250", "not-a-cidr"}
		_, err := w.ValidateCreate(context.Background(), cls)
		if err == nil {
			t.Fatal("expected an error")
		}
		for _, want := range []string{"egressExemptCIDRs[0]", "egressExemptCIDRs[1]"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error lacks %q: %v", want, err)
			}
		}
	})
}
