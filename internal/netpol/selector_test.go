// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package netpol

import (
	"errors"
	"slices"
	"testing"

	"github.com/google/go-cmp/cmp"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
)

// The kind profile from setec#76: the kube-dns ClusterIP is a configured
// resolver, and the address sits inside the reserved 10.0.0.0/8.
const clusterDNSIP = "10.96.0.10"

// kubeDNSAllowance is the selector allowance that lets a class query
// cluster DNS: the kube-dns Pods in kube-system on 53/UDP and 53/TCP.
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

// edgeAllowance is the platform-edge allowance: the Envoy Pods in the
// platform namespace on their named https port. The protocol is left
// unset to prove the TCP default.
func edgeAllowance() setecv1alpha1.EgressAllowSelector {
	return setecv1alpha1.EgressAllowSelector{
		NamespaceSelector: &metav1.LabelSelector{
			MatchLabels: map[string]string{"kubernetes.io/metadata.name": "gibson"},
		},
		PodSelector: &metav1.LabelSelector{
			MatchLabels: map[string]string{"app.kubernetes.io/name": "envoy"},
		},
		Ports: []setecv1alpha1.EgressAllowPort{{Port: intstr.FromString("https")}},
	}
}

// classWith returns a class carrying the given selector allowances and
// nothing else that affects the policy.
func classWith(allowances ...setecv1alpha1.EgressAllowSelector) *setecv1alpha1.SandboxClass {
	return &setecv1alpha1.SandboxClass{
		ObjectMeta: metav1.ObjectMeta{Name: "agent"},
		Spec:       setecv1alpha1.SandboxClassSpec{EgressAllowSelectors: allowances},
	}
}

// selectorRuleFor finds the egress rule whose single peer carries the
// allowance's selectors. Nil when no rule does.
func selectorRuleFor(np *networkingv1.NetworkPolicy, a setecv1alpha1.EgressAllowSelector) *networkingv1.NetworkPolicyEgressRule {
	for i := range np.Spec.Egress {
		r := &np.Spec.Egress[i]
		if len(r.To) != 1 || r.To[0].IPBlock != nil {
			continue
		}
		if cmp.Equal(r.To[0].NamespaceSelector, a.NamespaceSelector) &&
			cmp.Equal(r.To[0].PodSelector, a.PodSelector) {
			return r
		}
	}
	return nil
}

// wantPorts renders an allowance's ports the way the generator must.
func wantPorts(a setecv1alpha1.EgressAllowSelector) []networkingv1.NetworkPolicyPort {
	out := make([]networkingv1.NetworkPolicyPort, 0, len(a.Ports))
	for _, p := range a.Ports {
		proto := p.Protocol
		if proto == "" {
			proto = corev1.ProtocolTCP
		}
		port := p.Port
		out = append(out, networkingv1.NetworkPolicyPort{Protocol: &proto, Port: &port})
	}
	return out
}

// requireNoOpenPeerRule fails when any egress rule carries no peer. Such
// a rule permits its ports to every address, which is the one shape the
// DNS rule must never take when it has no reachable resolver.
func requireNoOpenPeerRule(t *testing.T, np *networkingv1.NetworkPolicy) {
	t.Helper()
	for i, r := range np.Spec.Egress {
		if len(r.To) == 0 {
			t.Fatalf("egress[%d] has no peers: it permits %v to every address", i, r.Ports)
		}
	}
}

// TestGenerate_SelectorAllowanceRendersPeersWithPorts is acceptance
// criterion 1 of setec#76: a class allowance renders as a rule with a
// namespaceSelector and podSelector peer and the named or numbered
// ports, beside the ipBlock rules the mode produces.
func TestGenerate_SelectorAllowanceRendersPeersWithPorts(t *testing.T) {
	t.Parallel()

	for _, mode := range []setecv1alpha1.NetworkMode{
		setecv1alpha1.NetworkModeExternalOnly,
		setecv1alpha1.NetworkModeEgressAllowList,
	} {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			cls := classWith(kubeDNSAllowance(), edgeAllowance())
			got, err := testCfg().GenerateForClass(t.Context(),
				sb(mode, setecv1alpha1.NetworkAllow{Host: "api.example.com", Port: 443}), cls)
			if err != nil {
				t.Fatalf("GenerateForClass() err: %v", err)
			}
			requireNoOpenPeerRule(t, got)

			for _, a := range []setecv1alpha1.EgressAllowSelector{kubeDNSAllowance(), edgeAllowance()} {
				rule := selectorRuleFor(got, a)
				if rule == nil {
					t.Fatalf("no egress rule carries the selector peer %v", a)
				}
				if diff := cmp.Diff(wantPorts(a), rule.Ports); diff != "" {
					t.Errorf("selector rule ports diff (-want +got):\n%s", diff)
				}
			}

			// The ipBlock rules are still there: DNS to the public
			// resolvers, and the mode's own destination rule.
			if diff := cmp.Diff(dnsRuleWant(), got.Spec.Egress[0]); diff != "" {
				t.Errorf("DNS rule diff (-want +got):\n%s", diff)
			}
			var ipBlocks int
			for _, r := range got.Spec.Egress[1:] {
				for _, p := range r.To {
					if p.IPBlock != nil {
						ipBlocks++
					}
				}
			}
			if ipBlocks == 0 {
				t.Errorf("the mode's ipBlock rule is missing beside the selector rules:\n%+v", got.Spec.Egress)
			}
		})
	}
}

// TestGenerate_SelectorAllowanceKeepsReservedRangesBlocked is acceptance
// criterion 2: the reserved ranges stay subtracted with an allowance
// present. A selector adds a peer; it exempts nothing.
func TestGenerate_SelectorAllowanceKeepsReservedRangesBlocked(t *testing.T) {
	t.Parallel()

	cls := classWith(kubeDNSAllowance(), edgeAllowance())

	t.Run("external-only still excepts every reserved range", func(t *testing.T) {
		t.Parallel()
		// The rule a class with no allowance gets is the reference: an
		// allowance must leave it byte-identical.
		ref, err := testCfg().GenerateForClass(t.Context(), sb(setecv1alpha1.NetworkModeExternalOnly), classWith())
		if err != nil {
			t.Fatalf("GenerateForClass(reference) err: %v", err)
		}
		got, err := testCfg().GenerateForClass(t.Context(), sb(setecv1alpha1.NetworkModeExternalOnly), cls)
		if err != nil {
			t.Fatalf("GenerateForClass() err: %v", err)
		}
		want := ref.Spec.Egress[len(ref.Spec.Egress)-1]
		last := got.Spec.Egress[len(got.Spec.Egress)-1]
		if last.To[0].IPBlock == nil || last.To[0].IPBlock.CIDR != AllCIDR {
			t.Fatalf("last rule is not the %s rule: %+v", AllCIDR, last)
		}
		if len(want.To[0].IPBlock.Except) == 0 {
			t.Fatal("reference rule excepts nothing; the test would prove nothing")
		}
		if diff := cmp.Diff(want, last); diff != "" {
			t.Errorf("the %s rule changed with an allowance present (-want +got):\n%s", AllCIDR, diff)
		}
	})

	t.Run("allow-list entry into a reserved range is still suppressed", func(t *testing.T) {
		t.Parallel()
		got, err := testCfg().GenerateForClass(t.Context(), sb(setecv1alpha1.NetworkModeEgressAllowList,
			setecv1alpha1.NetworkAllow{Host: "internal.example.com", Port: 443}), cls)
		if err != nil {
			t.Fatalf("GenerateForClass() err: %v", err)
		}
		for _, r := range got.Spec.Egress {
			for _, p := range r.To {
				if p.IPBlock != nil && p.IPBlock.CIDR == "10.20.30.40/32" {
					t.Fatalf("a reserved-range destination was written beside a selector allowance:\n%+v", r)
				}
			}
		}
		if got.Annotations[AnnotationSuppressed] == "" {
			t.Error("suppressed entry is not recorded")
		}
	})
}

// TestGenerate_ModeNoneIgnoresSelectorAllowance keeps "none" meaning
// none: a class allowance does not open a deny-all Sandbox.
func TestGenerate_ModeNoneIgnoresSelectorAllowance(t *testing.T) {
	t.Parallel()

	got, err := testCfg().GenerateForClass(t.Context(), sb(setecv1alpha1.NetworkModeNone),
		classWith(kubeDNSAllowance(), edgeAllowance()))
	if err != nil {
		t.Fatalf("GenerateForClass() err: %v", err)
	}
	if len(got.Spec.Egress) != 0 || len(got.Spec.Ingress) != 0 {
		t.Fatalf("mode=none rendered rules with a class allowance present: egress=%v ingress=%v",
			got.Spec.Egress, got.Spec.Ingress)
	}
}

// TestGenerate_NoClassRendersNoSelectorRule: the class-less entry point
// has no allowances to render, so its output is the pre-setec#76 shape.
func TestGenerate_NoClassRendersNoSelectorRule(t *testing.T) {
	t.Parallel()

	got, err := testCfg().Generate(t.Context(), sb(setecv1alpha1.NetworkModeExternalOnly))
	if err != nil {
		t.Fatalf("Generate() err: %v", err)
	}
	for _, r := range got.Spec.Egress {
		for _, p := range r.To {
			if p.NamespaceSelector != nil || p.PodSelector != nil {
				t.Fatalf("selector peer rendered with no class: %+v", p)
			}
		}
	}
}

// clusterCfg is the kind profile: the kube-dns ClusterIP first, then a
// public resolver.
func clusterCfg() Config {
	c := testCfg()
	c.ResolverIPs = []string{clusterDNSIP, "8.8.8.8"}
	return c
}

// dnsRuleOf returns the port-53 ipBlock rule, or nil when the policy has
// none.
func dnsRuleOf(np *networkingv1.NetworkPolicy) *networkingv1.NetworkPolicyEgressRule {
	for i := range np.Spec.Egress {
		r := &np.Spec.Egress[i]
		if len(r.To) == 0 || r.To[0].IPBlock == nil {
			continue
		}
		for _, p := range r.Ports {
			if p.Port != nil && p.Port.IntValue() == 53 {
				return r
			}
		}
	}
	return nil
}

// TestResolversFor_ClusterDNSNeedsAnAllowance covers the resolver side of
// setec#76. A resolver inside the reserved ranges is reached only
// through a selector allowance on port 53, so a class without one never
// has its Pods pointed at it, and the DNS rule never writes an ipBlock
// for it: after kube-proxy's translation that ipBlock would match
// nothing.
func TestResolversFor_ClusterDNSNeedsAnAllowance(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		cfg     Config
		class   *setecv1alpha1.SandboxClass
		wantPod []string
		wantDNS []string // ipBlock peers on the DNS rule; nil means no DNS rule
		wantErr error
	}{
		{
			name:    "production default: public resolvers, no class",
			cfg:     testCfg(),
			class:   nil,
			wantPod: testResolvers,
			wantDNS: testResolvers,
		},
		{
			name:    "cluster DNS is refused for a class with no allowance",
			cfg:     clusterCfg(),
			class:   classWith(),
			wantPod: []string{"8.8.8.8"},
			wantDNS: []string{"8.8.8.8"},
		},
		{
			name:    "cluster DNS is refused for a class with an allowance on another port",
			cfg:     clusterCfg(),
			class:   classWith(edgeAllowance()),
			wantPod: []string{"8.8.8.8"},
			wantDNS: []string{"8.8.8.8"},
		},
		{
			name: "a named DNS port does not count: the operator cannot see the Pod's port names",
			cfg:  clusterCfg(),
			class: classWith(setecv1alpha1.EgressAllowSelector{
				NamespaceSelector: kubeDNSAllowance().NamespaceSelector,
				PodSelector:       kubeDNSAllowance().PodSelector,
				Ports:             []setecv1alpha1.EgressAllowPort{{Protocol: corev1.ProtocolUDP, Port: intstr.FromString("dns")}},
			}),
			wantPod: []string{"8.8.8.8"},
			wantDNS: []string{"8.8.8.8"},
		},
		{
			name:    "cluster DNS is accepted for a class with a kube-dns allowance, in configured order",
			cfg:     clusterCfg(),
			class:   classWith(kubeDNSAllowance()),
			wantPod: []string{clusterDNSIP, "8.8.8.8"},
			wantDNS: []string{"8.8.8.8"},
		},
		{
			// egressExemptCIDRs drops a reserved entry only when the
			// exemption covers it whole (subtractExempt), so the class
			// exempts the range the resolver sits in. The resolver is
			// then outside the effective reserved ranges, and an ipBlock
			// reaches it: this is the corporate-DNS shape, not the
			// ClusterIP one.
			name: "an exempted resolver is outside the effective reserved ranges and gets an ipBlock",
			cfg:  clusterCfg(),
			class: &setecv1alpha1.SandboxClass{
				ObjectMeta: metav1.ObjectMeta{Name: "corp"},
				Spec:       setecv1alpha1.SandboxClassSpec{EgressExemptCIDRs: []string{"10.0.0.0/8"}},
			},
			wantPod: []string{clusterDNSIP, "8.8.8.8"},
			wantDNS: []string{clusterDNSIP, "8.8.8.8"},
		},
		{
			name: "no reachable resolver fails closed",
			cfg: func() Config {
				c := testCfg()
				c.ResolverIPs = []string{clusterDNSIP}
				return c
			}(),
			class:   classWith(),
			wantErr: ErrNoReachableResolver,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			pod, err := tc.cfg.ResolversFor(tc.class)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("ResolversFor() err = %v, want %v", err, tc.wantErr)
				}
				for _, mode := range []setecv1alpha1.NetworkMode{
					setecv1alpha1.NetworkModeExternalOnly,
					setecv1alpha1.NetworkModeEgressAllowList,
				} {
					if _, gerr := tc.cfg.GenerateForClass(t.Context(), sb(mode), tc.class); !errors.Is(gerr, tc.wantErr) {
						t.Errorf("GenerateForClass(%s) err = %v, want %v", mode, gerr, tc.wantErr)
					}
				}
				// Deny-all needs no resolver and still renders.
				if _, gerr := tc.cfg.GenerateForClass(t.Context(), sb(setecv1alpha1.NetworkModeNone), tc.class); gerr != nil {
					t.Errorf("GenerateForClass(none) err = %v, want nil", gerr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolversFor() err: %v", err)
			}
			if !slices.Equal(pod, tc.wantPod) {
				t.Errorf("ResolversFor() = %v, want %v", pod, tc.wantPod)
			}

			np, err := tc.cfg.GenerateForClass(t.Context(), sb(setecv1alpha1.NetworkModeExternalOnly), tc.class)
			if err != nil {
				t.Fatalf("GenerateForClass() err: %v", err)
			}
			requireNoOpenPeerRule(t, np)
			var gotDNS []string
			if r := dnsRuleOf(np); r != nil {
				for _, p := range r.To {
					gotDNS = append(gotDNS, p.IPBlock.CIDR)
				}
			}
			var wantDNS []string
			for _, ip := range tc.wantDNS {
				wantDNS = append(wantDNS, ip+"/32")
			}
			if !slices.Equal(gotDNS, wantDNS) {
				t.Errorf("DNS rule ipBlock peers = %v, want %v", gotDNS, wantDNS)
			}
		})
	}
}

// TestResolversFor_EmptyConfiguredListIsNotAnError pins the test-only
// shape: a Config with no resolvers (which Validate refuses at startup)
// yields no resolvers and no error, so an envtest reconciler built with
// a zero Config still creates Pods, and the generated policy carries no
// DNS rule rather than one with no peers.
func TestResolversFor_EmptyConfiguredListIsNotAnError(t *testing.T) {
	t.Parallel()

	cfg := testCfg()
	cfg.ResolverIPs = nil
	pod, err := cfg.ResolversFor(classWith())
	if err != nil {
		t.Fatalf("ResolversFor() err = %v, want nil", err)
	}
	if len(pod) != 0 {
		t.Fatalf("ResolversFor() = %v, want empty", pod)
	}
	np, err := cfg.GenerateForClass(t.Context(), sb(setecv1alpha1.NetworkModeExternalOnly), classWith())
	if err != nil {
		t.Fatalf("GenerateForClass() err: %v", err)
	}
	requireNoOpenPeerRule(t, np)
	if dnsRuleOf(np) != nil {
		t.Fatalf("a DNS rule was rendered with no resolvers configured: %+v", np.Spec.Egress)
	}
}

// TestResolversFor_ProductionDefaultUnchanged pins the shipped posture:
// public resolvers and no selectors give every class the full list and
// the DNS rule of before setec#76.
func TestResolversFor_ProductionDefaultUnchanged(t *testing.T) {
	t.Parallel()

	for _, cls := range []*setecv1alpha1.SandboxClass{nil, classWith()} {
		pod, err := testCfg().ResolversFor(cls)
		if err != nil {
			t.Fatalf("ResolversFor(%v) err: %v", cls, err)
		}
		if !slices.Equal(pod, testResolvers) {
			t.Errorf("ResolversFor(%v) = %v, want %v", cls, pod, testResolvers)
		}
		np, err := testCfg().GenerateForClass(t.Context(), sb(setecv1alpha1.NetworkModeExternalOnly), cls)
		if err != nil {
			t.Fatalf("GenerateForClass() err: %v", err)
		}
		if diff := cmp.Diff(dnsRuleWant(), np.Spec.Egress[0]); diff != "" {
			t.Errorf("DNS rule diff (-want +got):\n%s", diff)
		}
	}
}
