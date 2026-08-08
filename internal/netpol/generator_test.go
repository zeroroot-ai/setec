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

package netpol

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/google/go-cmp/cmp"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
	"github.com/zeroroot-ai/setec/internal/podspec"
)

// testReserved is the reserved list every test builds against. It mirrors
// the operator default plus a Service CIDR, which is the shape a real
// cluster runs with.
var testReserved = []string{
	"10.0.0.0/8",
	"172.16.0.0/12",
	"192.168.0.0/16",
	"169.254.0.0/16",
	"100.64.0.0/10",
	"127.0.0.0/8",
	"224.0.0.0/4",
}

var testResolvers = []string{"1.1.1.1", "8.8.8.8"}

// testHostAddrs is the DNS the generator tests run against. Every host a
// test declares has an entry; a host that is absent models a name that
// does not resolve.
//
// The addresses are drawn from 203.0.113.0/24 (TEST-NET-3) and
// 198.51.100.0/24 (TEST-NET-2) so they are unmistakably fixtures and, more
// importantly, are outside every entry in testReserved — a resolved
// address inside a reserved range is suppressed, which would silently
// change what these cases assert.
var testHostAddrs = map[string][]string{
	"api.example.com":     {"203.0.113.10", "203.0.113.11"},
	"metrics.example.com": {"198.51.100.7"},
	"mirror.example.com":  {"203.0.113.20"},
	"vendor.example.com":  {"203.0.113.30"},
	"h":                   {"203.0.113.40"},
	// Resolves into a reserved range on purpose: the rule must be
	// suppressed rather than written with an ipBlock that grants nothing.
	"internal.example.com": {"10.20.30.40"},
}

// stubResolver is a HostResolver backed by testHostAddrs. It performs no
// I/O, so the generator tests never touch real DNS.
type stubResolver struct{}

func (stubResolver) Resolve(_ context.Context, host string) ([]string, error) {
	addrs, ok := testHostAddrs[host]
	if !ok {
		return nil, fmt.Errorf("%w: %q: stub has no record", ErrResolveFailed, host)
	}
	out := make([]string, 0, len(addrs))
	for _, a := range addrs {
		out = append(out, a+"/32")
	}
	return out, nil
}

// hostCIDRs renders a testHostAddrs entry as the peer CIDRs the generator
// is expected to produce for it.
func hostCIDRs(host string) []string {
	addrs := testHostAddrs[host]
	out := make([]string, 0, len(addrs))
	for _, a := range addrs {
		out = append(out, a+"/32")
	}
	return out
}

// testCfg is the Config under test.
func testCfg() Config {
	return Config{
		ReservedCIDRs: slices.Clone(testReserved),
		ResolverIPs:   slices.Clone(testResolvers),
		Resolver:      stubResolver{},
	}
}

// sb returns a baseline Sandbox used by every case; individual tests
// override the Network field to exercise the branch under test.
func sb(mode setecv1alpha1.NetworkMode, allow ...setecv1alpha1.NetworkAllow) *setecv1alpha1.Sandbox {
	var netSpec *setecv1alpha1.Network
	if mode != "" {
		netSpec = &setecv1alpha1.Network{
			Mode:  mode,
			Allow: allow,
		}
	}
	return &setecv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "my-sb", Namespace: "team-a"},
		Spec: setecv1alpha1.SandboxSpec{
			Image:   "alpine:3.19",
			Command: []string{"sh"},
			Resources: setecv1alpha1.Resources{
				VCPU:   1,
				Memory: resource.MustParse("256Mi"),
			},
			Network: netSpec,
		},
	}
}

// portRule returns the canonical NetworkPolicyPort pointer pair for a TCP
// port. Keeps assertion tables readable.
func portRule(port int32) networkingv1.NetworkPolicyPort {
	is := intstr.FromInt32(port)
	proto := corev1.ProtocolTCP
	return networkingv1.NetworkPolicyPort{
		Protocol: &proto,
		Port:     &is,
	}
}

func udpRule(port int32) networkingv1.NetworkPolicyPort {
	is := intstr.FromInt32(port)
	proto := corev1.ProtocolUDP
	return networkingv1.NetworkPolicyPort{
		Protocol: &proto,
		Port:     &is,
	}
}

// resolverPeers is the expected DNS peer set: one /32 per configured
// resolver, and nothing else.
func resolverPeers() []networkingv1.NetworkPolicyPeer {
	return []networkingv1.NetworkPolicyPeer{
		{IPBlock: &networkingv1.IPBlock{CIDR: "1.1.1.1/32"}},
		{IPBlock: &networkingv1.IPBlock{CIDR: "8.8.8.8/32"}},
	}
}

// dnsRuleWant is the expected DNS egress rule.
func dnsRuleWant() networkingv1.NetworkPolicyEgressRule {
	return networkingv1.NetworkPolicyEgressRule{
		To:    resolverPeers(),
		Ports: []networkingv1.NetworkPolicyPort{udpRule(53), portRule(53)},
	}
}

// --- The invariant: a Sandbox always gets a policy -------------------------

// TestGenerate_NeverReturnsNilPolicy is the load-bearing test of this
// package. Any input that produced no policy would put an unpoliced
// microVM on the cluster, so no input may.
func TestGenerate_NeverReturnsNilPolicy(t *testing.T) {
	t.Parallel()

	cases := map[string]*setecv1alpha1.Sandbox{
		"network absent":    sb(""),
		"mode none":         sb(setecv1alpha1.NetworkModeNone),
		"mode external":     sb(setecv1alpha1.NetworkModeExternalOnly),
		"mode allow-list":   sb(setecv1alpha1.NetworkModeEgressAllowList),
		"allow-list w/ort":  sb(setecv1alpha1.NetworkModeEgressAllowList, setecv1alpha1.NetworkAllow{Host: "h", Port: 443}),
		"empty allow entry": sb(setecv1alpha1.NetworkModeEgressAllowList),
	}

	for name, s := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := testCfg().Generate(t.Context(), s)
			if err != nil {
				t.Fatalf("Generate() err: %v", err)
			}
			if got == nil {
				t.Fatal("Generate() returned a nil policy; every Sandbox must be policed")
			}
			if !slices.Contains(got.Spec.PolicyTypes, networkingv1.PolicyTypeEgress) {
				t.Errorf("PolicyTypes %v omits Egress; egress would be unrestricted", got.Spec.PolicyTypes)
			}
			if !slices.Contains(got.Spec.PolicyTypes, networkingv1.PolicyTypeIngress) {
				t.Errorf("PolicyTypes %v omits Ingress", got.Spec.PolicyTypes)
			}
		})
	}
}

// TestGenerate_AbsentNetworkIsDenyAll pins the resolution of an
// unstated posture. An omitted spec.network used to mean "no policy";
// it now means deny-all.
func TestGenerate_AbsentNetworkIsDenyAll(t *testing.T) {
	t.Parallel()

	got, err := testCfg().Generate(t.Context(), sb(""))
	if err != nil {
		t.Fatalf("Generate() err: %v", err)
	}
	if len(got.Spec.Egress) != 0 {
		t.Fatalf("absent network must yield zero egress rules, got %+v", got.Spec.Egress)
	}
	if len(got.Spec.Ingress) != 0 {
		t.Fatalf("absent network must yield zero ingress rules, got %+v", got.Spec.Ingress)
	}
}

func TestGenerate_ModeNoneDeniesAll(t *testing.T) {
	t.Parallel()

	got, err := testCfg().Generate(t.Context(), sb(setecv1alpha1.NetworkModeNone))
	if err != nil {
		t.Fatalf("Generate() err: %v", err)
	}

	want := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-sb-netpol",
			Namespace: "team-a",
			Labels:    map[string]string{podspec.SandboxLabelKey: "my-sb"},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{podspec.SandboxLabelKey: "my-sb"},
			},
			PolicyTypes: []networkingv1.PolicyType{
				networkingv1.PolicyTypeIngress,
				networkingv1.PolicyTypeEgress,
			},
			// Ingress and Egress are nil → deny all in their types.
		},
	}

	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("Generate() diff (-want +got):\n%s", diff)
	}
}

// --- external-only ---------------------------------------------------------

// TestGenerate_ExternalOnlyShape locks in the posture a scanning workload
// runs under: every port reachable on public address space, every
// reserved range subtracted, DNS pinned to the configured resolvers.
func TestGenerate_ExternalOnlyShape(t *testing.T) {
	t.Parallel()

	got, err := testCfg().Generate(t.Context(), sb(setecv1alpha1.NetworkModeExternalOnly))
	if err != nil {
		t.Fatalf("Generate() err: %v", err)
	}

	want := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-sb-netpol",
			Namespace: "team-a",
			Labels:    map[string]string{podspec.SandboxLabelKey: "my-sb"},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{podspec.SandboxLabelKey: "my-sb"},
			},
			PolicyTypes: []networkingv1.PolicyType{
				networkingv1.PolicyTypeIngress,
				networkingv1.PolicyTypeEgress,
			},
			Egress: []networkingv1.NetworkPolicyEgressRule{
				dnsRuleWant(),
				{
					To: []networkingv1.NetworkPolicyPeer{{
						IPBlock: &networkingv1.IPBlock{
							CIDR:   "0.0.0.0/0",
							Except: testReserved,
						},
					}},
					// No Ports: arbitrary destination ports are the point.
				},
			},
		},
	}

	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("Generate() diff (-want +got):\n%s", diff)
	}
}

// TestGenerate_ExternalOnlyKeepsArbitraryPortsOpen states the product
// requirement explicitly: confinement is by address space, not by port.
// A change that starts scoping this rule to a port set breaks scanning.
func TestGenerate_ExternalOnlyKeepsArbitraryPortsOpen(t *testing.T) {
	t.Parallel()

	got, err := testCfg().Generate(t.Context(), sb(setecv1alpha1.NetworkModeExternalOnly))
	if err != nil {
		t.Fatalf("Generate() err: %v", err)
	}

	var found bool
	for _, rule := range got.Spec.Egress {
		if len(rule.To) == 1 && rule.To[0].IPBlock != nil && rule.To[0].IPBlock.CIDR == AllCIDR {
			found = true
			if len(rule.Ports) != 0 {
				t.Errorf("public-egress rule is port-scoped to %+v; arbitrary-port scanning would break", rule.Ports)
			}
		}
	}
	if !found {
		t.Fatal("external-only produced no 0.0.0.0/0 egress rule; external targets would be unreachable")
	}
}

// --- egress-allow-list -----------------------------------------------------

// TestGenerate_AllowListDoesNotOpenInClusterCIDRs is the regression test
// for the defect this change fixes. An allow-list entry naming an
// external host used to open that port to every address, including the
// cluster's own ranges.
func TestGenerate_AllowListDoesNotOpenInClusterCIDRs(t *testing.T) {
	t.Parallel()

	got, err := testCfg().Generate(t.Context(), sb(setecv1alpha1.NetworkModeEgressAllowList,
		setecv1alpha1.NetworkAllow{Host: "api.example.com", Port: 443}))
	if err != nil {
		t.Fatalf("Generate() err: %v", err)
	}

	// An allow-list entry must never name all of public address space.
	// Before setec#130 it always did — the declared host went to an
	// annotation and the rule went to 0.0.0.0/0 on the declared port — so
	// this is the assertion that says the destination is enforced and not
	// just the port.
	for i, rule := range got.Spec.Egress {
		for j, peer := range rule.To {
			blk := peer.IPBlock
			if blk == nil {
				continue
			}
			if blk.CIDR == AllCIDR {
				t.Errorf("egress[%d].to[%d] permits %s; an allow-list entry must name its "+
					"destination, not the whole internet on one port", i, j, AllCIDR)
				continue
			}
			// Belt and braces: whatever block the rule does name must
			// not overlap a reserved range without excepting it.
			for _, reserved := range testReserved {
				if blk.CIDR == reserved && !slices.Contains(blk.Except, reserved) {
					t.Errorf("egress[%d].to[%d] permits reserved range %s", i, j, reserved)
				}
			}
		}
	}

	// And it must actually name the resolved addresses.
	var seen []string
	for _, peer := range got.Spec.Egress[1].To {
		seen = append(seen, peer.IPBlock.CIDR)
	}
	if want := hostCIDRs("api.example.com"); !slices.Equal(seen, want) {
		t.Errorf("allow rule peers = %v, want %v", seen, want)
	}
}

// TestGenerate_AllowListRuleShape pins the whole rendered policy for the
// common single-entry case.
func TestGenerate_AllowListRuleShape(t *testing.T) {
	t.Parallel()

	got, err := testCfg().Generate(t.Context(), sb(setecv1alpha1.NetworkModeEgressAllowList,
		setecv1alpha1.NetworkAllow{Host: "api.example.com", Port: 443}))
	if err != nil {
		t.Fatalf("Generate() err: %v", err)
	}

	want := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-sb-netpol",
			Namespace: "team-a",
			Labels:    map[string]string{podspec.SandboxLabelKey: "my-sb"},
			Annotations: map[string]string{
				"setec.zeroroot.ai/allow-443": "api.example.com",
			},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{podspec.SandboxLabelKey: "my-sb"},
			},
			PolicyTypes: []networkingv1.PolicyType{
				networkingv1.PolicyTypeIngress,
				networkingv1.PolicyTypeEgress,
			},
			Egress: []networkingv1.NetworkPolicyEgressRule{
				dnsRuleWant(),
				{
					// One peer per resolved address, and no except list:
					// a /32 outside every reserved range has nothing to
					// subtract. This is the whole point of setec#130 —
					// the rule names the destination, not "anywhere on
					// 443".
					To: []networkingv1.NetworkPolicyPeer{
						{IPBlock: &networkingv1.IPBlock{CIDR: "203.0.113.10/32"}},
						{IPBlock: &networkingv1.IPBlock{CIDR: "203.0.113.11/32"}},
					},
					Ports: []networkingv1.NetworkPolicyPort{portRule(443)},
				},
			},
		},
	}

	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("Generate() diff (-want +got):\n%s", diff)
	}
}

// TestGenerate_AllowListPinnedCIDR covers the opt-in narrowing path: a
// caller that knows the destination range states it, and the rule is
// built on that block rather than on all of public address space.
func TestGenerate_AllowListPinnedCIDR(t *testing.T) {
	t.Parallel()

	got, err := testCfg().Generate(t.Context(), sb(setecv1alpha1.NetworkModeEgressAllowList,
		setecv1alpha1.NetworkAllow{Host: "vendor.example.com", Port: 443, CIDR: "203.0.113.0/24"}))
	if err != nil {
		t.Fatalf("Generate() err: %v", err)
	}

	rule := got.Spec.Egress[1]
	if want := "203.0.113.0/24"; rule.To[0].IPBlock.CIDR != want {
		t.Fatalf("pinned rule CIDR = %q, want %q", rule.To[0].IPBlock.CIDR, want)
	}
	// The reserved ranges are disjoint from a public /24, so none of them
	// belong in except — Kubernetes rejects an except outside its block.
	if len(rule.To[0].IPBlock.Except) != 0 {
		t.Fatalf("except = %v, want empty: reserved ranges are disjoint from the pinned block",
			rule.To[0].IPBlock.Except)
	}
}

// TestGenerate_AllowListReservedTargetIsSuppressed proves an allow-list
// entry cannot be used to reach reserved space: naming a reserved block
// drops the rule instead of emitting one.
func TestGenerate_AllowListReservedTargetIsSuppressed(t *testing.T) {
	t.Parallel()

	got, err := testCfg().Generate(t.Context(), sb(setecv1alpha1.NetworkModeEgressAllowList,
		setecv1alpha1.NetworkAllow{Host: "daemon.platform.svc", Port: 50051, CIDR: "10.96.0.0/12"}))
	if err != nil {
		t.Fatalf("Generate() err: %v", err)
	}

	// Only the DNS rule survives.
	if len(got.Spec.Egress) != 1 {
		t.Fatalf("egress rules = %d, want 1 (DNS only); a reserved destination was granted", len(got.Spec.Egress))
	}
	if got.Annotations[AnnotationSuppressed] == "" {
		t.Error("suppressed entry is not recorded; the policy would look permissive but grant nothing")
	}
}

// TestGenerate_ClassExemptionReopensReservedRange covers the deliberate,
// audited hole a class may punch for one in-cluster endpoint.
func TestGenerate_ClassExemptionReopensReservedRange(t *testing.T) {
	t.Parallel()

	cls := &setecv1alpha1.SandboxClass{
		ObjectMeta: metav1.ObjectMeta{Name: "connector"},
		Spec: setecv1alpha1.SandboxClassSpec{
			EgressExemptCIDRs: []string{"10.0.0.0/8"},
		},
	}
	got, err := testCfg().GenerateForClass(t.Context(), sb(setecv1alpha1.NetworkModeEgressAllowList,
		setecv1alpha1.NetworkAllow{Host: "platform", Port: 8080, CIDR: "10.96.0.0/12"}), cls)
	if err != nil {
		t.Fatalf("GenerateForClass() err: %v", err)
	}

	if len(got.Spec.Egress) != 2 {
		t.Fatalf("egress rules = %d, want 2 (DNS + exempted entry)", len(got.Spec.Egress))
	}
	if want := "10.96.0.0/12"; got.Spec.Egress[1].To[0].IPBlock.CIDR != want {
		t.Errorf("exempted rule CIDR = %q, want %q", got.Spec.Egress[1].To[0].IPBlock.CIDR, want)
	}
}

// --- DNS -------------------------------------------------------------------

// TestGenerate_DNSTargetsOnlyConfiguredResolvers proves a Sandbox cannot
// query cluster DNS. Reaching kube-dns would let it enumerate every
// in-cluster Service by name even before a single packet is dropped.
func TestGenerate_DNSTargetsOnlyConfiguredResolvers(t *testing.T) {
	t.Parallel()

	for _, mode := range []setecv1alpha1.NetworkMode{
		setecv1alpha1.NetworkModeExternalOnly,
		setecv1alpha1.NetworkModeEgressAllowList,
	} {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			got, err := testCfg().Generate(t.Context(), sb(mode))
			if err != nil {
				t.Fatalf("Generate() err: %v", err)
			}

			var dns *networkingv1.NetworkPolicyEgressRule
			for i := range got.Spec.Egress {
				for _, p := range got.Spec.Egress[i].Ports {
					if p.Port != nil && p.Port.IntValue() == 53 {
						dns = &got.Spec.Egress[i]
					}
				}
			}
			if dns == nil {
				t.Fatal("no DNS rule generated; name resolution would fail entirely")
			}
			if diff := cmp.Diff(resolverPeers(), dns.To); diff != "" {
				t.Fatalf("DNS peers diff (-want +got):\n%s", diff)
			}
		})
	}
}

// --- Config validation -----------------------------------------------------

func TestConfigValidate(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		cfg     Config
		wantErr error
	}{
		"valid":              {testCfg(), nil},
		"no reserved cidrs":  {Config{ResolverIPs: testResolvers}, ErrNoReservedCIDRs},
		"no resolvers":       {Config{ReservedCIDRs: testReserved}, ErrNoResolvers},
		"bad reserved cidr":  {Config{ReservedCIDRs: []string{"not-a-cidr"}, ResolverIPs: testResolvers}, ErrInvalidCIDR},
		"bad resolver":       {Config{ReservedCIDRs: testReserved, ResolverIPs: []string{"nope"}}, ErrInvalidResolver},
		"resolver with mask": {Config{ReservedCIDRs: testReserved, ResolverIPs: []string{"1.1.1.1/32"}}, ErrInvalidResolver},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := tc.cfg.Validate()
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("Validate() err: %v", err)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Validate() = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

// --- errors ----------------------------------------------------------------

func TestGenerate_NilSandbox(t *testing.T) {
	t.Parallel()
	_, err := testCfg().Generate(t.Context(), nil)
	if err == nil || !errors.Is(err, ErrNilSandbox) {
		t.Fatalf("expected ErrNilSandbox, got %v", err)
	}
}

func TestGenerate_UnknownMode(t *testing.T) {
	t.Parallel()
	_, err := testCfg().Generate(t.Context(), sb(setecv1alpha1.NetworkMode("mystery")))
	if err == nil || !errors.Is(err, ErrUnknownMode) {
		t.Fatalf("expected ErrUnknownMode, got %v", err)
	}
}

// TestGenerate_DeclaredHostIsNotResolved locks in that the translator
// records a declared hostname for audit and never turns it into an
// address. Resolving here would bake a DNS answer into a long-lived
// TestGenerate_UnresolvableHostIsDroppedNotWidened is the regression test
// for setec#130.
//
// A declared host the operator cannot locate used to fall back to a base
// of 0.0.0.0/0, so the rule enforced the port and nothing else. The entry
// is dropped now, and the drop is recorded where an operator will see it.
func TestGenerate_UnresolvableHostIsDroppedNotWidened(t *testing.T) {
	t.Parallel()

	got, err := testCfg().Generate(t.Context(), sb(setecv1alpha1.NetworkModeEgressAllowList,
		setecv1alpha1.NetworkAllow{Host: "private.internal.corp", Port: 8080}))
	if err != nil {
		t.Fatalf("Generate() err: %v", err)
	}

	if got.Annotations["setec.zeroroot.ai/allow-8080"] != "private.internal.corp" {
		t.Fatalf("declared host should be recorded as an annotation, got %v", got.Annotations)
	}
	if want := "private.internal.corp:8080"; got.Annotations[AnnotationUnresolved] != want {
		t.Errorf("%s = %q, want %q", AnnotationUnresolved,
			got.Annotations[AnnotationUnresolved], want)
	}

	// DNS only. No rule for the unresolvable destination, and above all
	// no rule naming 0.0.0.0/0.
	if len(got.Spec.Egress) != 1 {
		t.Fatalf("expected only the DNS rule, got %d rules: %+v", len(got.Spec.Egress), got.Spec.Egress)
	}
	for i, rule := range got.Spec.Egress {
		for j, peer := range rule.To {
			if peer.IPBlock != nil && peer.IPBlock.CIDR == AllCIDR {
				t.Errorf("egress[%d].to[%d] is %s; an unresolvable host must not widen to all of "+
					"public address space", i, j, AllCIDR)
			}
		}
	}
}

// TestGenerate_NoResolverConfiguredFailsClosed pins the behaviour of a
// Config with no Resolver. A nil resolver is not a licence to fall back to
// the old 0.0.0.0/0 base: the entry is dropped exactly as an unresolvable
// name is.
func TestGenerate_NoResolverConfiguredFailsClosed(t *testing.T) {
	t.Parallel()

	cfg := testCfg()
	cfg.Resolver = nil

	got, err := cfg.Generate(t.Context(), sb(setecv1alpha1.NetworkModeEgressAllowList,
		setecv1alpha1.NetworkAllow{Host: "api.example.com", Port: 443}))
	if err != nil {
		t.Fatalf("Generate() err: %v", err)
	}
	if len(got.Spec.Egress) != 1 {
		t.Fatalf("expected only the DNS rule, got %d rules: %+v", len(got.Spec.Egress), got.Spec.Egress)
	}
	if got.Annotations[AnnotationUnresolved] == "" {
		t.Errorf("drop should be recorded on %s; annotations = %v", AnnotationUnresolved, got.Annotations)
	}
}

// TestGenerate_LiteralAddressHostNeedsNoResolver covers the second source
// of an ipBlock: a host that already is an address. No lookup should
// happen, so this passes with the resolver removed.
func TestGenerate_LiteralAddressHostNeedsNoResolver(t *testing.T) {
	t.Parallel()

	cfg := testCfg()
	cfg.Resolver = nil

	got, err := cfg.Generate(t.Context(), sb(setecv1alpha1.NetworkModeEgressAllowList,
		setecv1alpha1.NetworkAllow{Host: "203.0.113.55", Port: 443}))
	if err != nil {
		t.Fatalf("Generate() err: %v", err)
	}
	if len(got.Spec.Egress) != 2 {
		t.Fatalf("expected DNS + 1 allow rule, got %d: %+v", len(got.Spec.Egress), got.Spec.Egress)
	}
	if got := got.Spec.Egress[1].To[0].IPBlock.CIDR; got != "203.0.113.55/32" {
		t.Errorf("egress CIDR = %q, want %q", got, "203.0.113.55/32")
	}
}

// TestGenerate_ResolvedIntoReservedRangeIsSuppressed covers the
// interaction between resolution and the reserved list. A destination
// whose name points at cluster-internal address space must not become a
// reachable rule just because the caller wrote a public-looking name —
// this is the DNS-rebinding shape of the same attack the reserved list
// exists to stop.
func TestGenerate_ResolvedIntoReservedRangeIsSuppressed(t *testing.T) {
	t.Parallel()

	got, err := testCfg().Generate(t.Context(), sb(setecv1alpha1.NetworkModeEgressAllowList,
		setecv1alpha1.NetworkAllow{Host: "internal.example.com", Port: 8443}))
	if err != nil {
		t.Fatalf("Generate() err: %v", err)
	}
	if len(got.Spec.Egress) != 1 {
		t.Fatalf("expected only the DNS rule, got %d rules: %+v", len(got.Spec.Egress), got.Spec.Egress)
	}
	if want := "internal.example.com:8443"; got.Annotations[AnnotationSuppressed] != want {
		t.Errorf("%s = %q, want %q", AnnotationSuppressed,
			got.Annotations[AnnotationSuppressed], want)
	}
}

// TestDependsOnDNS pins which postures need a periodic requeue. Only a
// name does; a literal address and an explicit CIDR are already final.
func TestDependsOnDNS(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		sandbox *setecv1alpha1.Sandbox
		want    bool
	}{
		"nil sandbox":   {nil, false},
		"deny-all":      {sb(setecv1alpha1.NetworkModeNone), false},
		"external-only": {sb(setecv1alpha1.NetworkModeExternalOnly), false},
		"allow-list w/name": {sb(setecv1alpha1.NetworkModeEgressAllowList,
			setecv1alpha1.NetworkAllow{Host: "api.example.com", Port: 443}), true},
		"allow-list w/literal": {sb(setecv1alpha1.NetworkModeEgressAllowList,
			setecv1alpha1.NetworkAllow{Host: "203.0.113.55", Port: 443}), false},
		"allow-list w/pinned CIDR": {sb(setecv1alpha1.NetworkModeEgressAllowList,
			setecv1alpha1.NetworkAllow{Host: "api.example.com", Port: 443, CIDR: "203.0.113.0/24"}), false},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := DependsOnDNS(tc.sandbox, nil); got != tc.want {
				t.Errorf("DependsOnDNS() = %v, want %v", got, tc.want)
			}
		})
	}
}
