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
	"errors"
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

// testCfg is the Config under test.
func testCfg() Config {
	return Config{
		ReservedCIDRs: slices.Clone(testReserved),
		ResolverIPs:   slices.Clone(testResolvers),
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
			got, err := testCfg().Generate(s)
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

	got, err := testCfg().Generate(sb(""))
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

	got, err := testCfg().Generate(sb(setecv1alpha1.NetworkModeNone))
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

	got, err := testCfg().Generate(sb(setecv1alpha1.NetworkModeExternalOnly))
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

	got, err := testCfg().Generate(sb(setecv1alpha1.NetworkModeExternalOnly))
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

	got, err := testCfg().Generate(sb(setecv1alpha1.NetworkModeEgressAllowList,
		setecv1alpha1.NetworkAllow{Host: "api.example.com", Port: 443}))
	if err != nil {
		t.Fatalf("Generate() err: %v", err)
	}

	// Every peer that permits a broad block must subtract the reserved
	// ranges. A bare 0.0.0.0/0 anywhere in the policy is the bug.
	for i, rule := range got.Spec.Egress {
		for j, peer := range rule.To {
			blk := peer.IPBlock
			if blk == nil || blk.CIDR != AllCIDR {
				continue
			}
			for _, reserved := range testReserved {
				if !slices.Contains(blk.Except, reserved) {
					t.Errorf("egress[%d].to[%d] permits %s without excepting %s; in-cluster addresses are reachable",
						i, j, blk.CIDR, reserved)
				}
			}
		}
	}
}

// TestGenerate_AllowListRuleShape pins the whole rendered policy for the
// common single-entry case.
func TestGenerate_AllowListRuleShape(t *testing.T) {
	t.Parallel()

	got, err := testCfg().Generate(sb(setecv1alpha1.NetworkModeEgressAllowList,
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
					To: []networkingv1.NetworkPolicyPeer{{
						IPBlock: &networkingv1.IPBlock{CIDR: "0.0.0.0/0", Except: testReserved},
					}},
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

	got, err := testCfg().Generate(sb(setecv1alpha1.NetworkModeEgressAllowList,
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

	got, err := testCfg().Generate(sb(setecv1alpha1.NetworkModeEgressAllowList,
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
	got, err := testCfg().GenerateForClass(sb(setecv1alpha1.NetworkModeEgressAllowList,
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
			got, err := testCfg().Generate(sb(mode))
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
	_, err := testCfg().Generate(nil)
	if err == nil || !errors.Is(err, ErrNilSandbox) {
		t.Fatalf("expected ErrNilSandbox, got %v", err)
	}
}

func TestGenerate_UnknownMode(t *testing.T) {
	t.Parallel()
	_, err := testCfg().Generate(sb(setecv1alpha1.NetworkMode("mystery")))
	if err == nil || !errors.Is(err, ErrUnknownMode) {
		t.Fatalf("expected ErrUnknownMode, got %v", err)
	}
}

// TestGenerate_DeclaredHostIsNotResolved locks in that the translator
// records a declared hostname for audit and never turns it into an
// address. Resolving here would bake a DNS answer into a long-lived
// object and put a resolver in the reconcile path.
func TestGenerate_DeclaredHostIsNotResolved(t *testing.T) {
	t.Parallel()

	got, err := testCfg().Generate(sb(setecv1alpha1.NetworkModeEgressAllowList,
		setecv1alpha1.NetworkAllow{Host: "private.internal.corp", Port: 8080}))
	if err != nil {
		t.Fatalf("Generate() err: %v", err)
	}

	if got.Annotations["setec.zeroroot.ai/allow-8080"] != "private.internal.corp" {
		t.Fatalf("declared host should be recorded as an annotation, got %v", got.Annotations)
	}
	if want := AllCIDR; got.Spec.Egress[1].To[0].IPBlock.CIDR != want {
		t.Errorf("egress CIDR = %q, want %q", got.Spec.Egress[1].To[0].IPBlock.CIDR, want)
	}
}
