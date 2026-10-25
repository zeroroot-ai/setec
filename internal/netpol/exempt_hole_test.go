package netpol

import (
	"net/netip"
	"testing"

	networkingv1 "k8s.io/api/networking/v1"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
)

// reachedBy reports whether any ipBlock rule of the policy lets addr
// through: the address is inside cidr and outside every except entry.
// It also reports the rule's ports for the matching rule.
func reachedBy(t *testing.T, np *networkingv1.NetworkPolicy, addr string) bool {
	t.Helper()
	a := netip.MustParseAddr(addr)
	for _, rule := range np.Spec.Egress {
		for _, peer := range rule.To {
			if peer.IPBlock == nil {
				continue
			}
			base := netip.MustParsePrefix(peer.IPBlock.CIDR)
			if !base.Contains(a) {
				continue
			}
			blocked := false
			for _, ex := range peer.IPBlock.Except {
				if netip.MustParsePrefix(ex).Contains(a) {
					blocked = true
					break
				}
			}
			if !blocked {
				return true
			}
		}
	}
	return false
}

func exemptClass(name string, cidrs ...string) *setecv1alpha1.SandboxClass {
	return &setecv1alpha1.SandboxClass{
		Name: name,
		Spec: setecv1alpha1.SandboxClassSpec{EgressExemptCIDRs: cidrs},
	}
}

// TestSubtractExempt_HoleInsideReservedRange fails on the code before
// setec exempted a prefix that lies inside a reserved range: the /32 had
// no effect and the whole range stayed denied.
func TestSubtractExempt_HoleInsideReservedRange(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		exempt  []string
		open    []string
		denied  []string
		wantLen int // expected entries for reserved entries other than the cut one
	}{
		{
			name:   "a /32 inside 10.0.0.0/8 opens only that address",
			exempt: []string{"10.60.0.66/32"},
			open:   []string{"10.60.0.66"},
			denied: []string{"10.60.0.65", "10.60.0.67", "10.0.0.1", "10.255.255.255", "172.16.0.1", "169.254.169.254"},
		},
		{
			name:   "a /24 inside 10.0.0.0/8 opens only that block",
			exempt: []string{"10.60.0.0/24"},
			open:   []string{"10.60.0.0", "10.60.0.255"},
			denied: []string{"10.59.255.255", "10.60.1.0", "10.0.0.1"},
		},
		{
			name:   "two exemptions inside one range open both",
			exempt: []string{"10.60.0.66/32", "10.70.1.2/32"},
			open:   []string{"10.60.0.66", "10.70.1.2"},
			denied: []string{"10.60.0.67", "10.70.1.3"},
		},
		{
			name:   "an exemption that covers a whole range drops it",
			exempt: []string{"10.0.0.0/8"},
			open:   []string{"10.0.0.1", "10.60.0.66"},
			denied: []string{"172.16.0.1", "192.168.1.1"},
		},
		{
			name:   "a wider exemption than the range drops it",
			exempt: []string{"10.0.0.0/7"},
			open:   []string{"10.0.0.1"},
			denied: []string{"172.16.0.1"},
		},
		{
			name:   "an exemption outside every reserved range changes nothing",
			exempt: []string{"203.0.113.0/24"},
			open:   []string{"203.0.113.9", "8.8.8.8"},
			denied: []string{"10.60.0.66", "10.0.0.1", "172.16.0.1"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := testCfg().GenerateForClass(t.Context(),
				sb(setecv1alpha1.NetworkModeExternalOnly), exemptClass("tool", tc.exempt...))
			if err != nil {
				t.Fatalf("GenerateForClass() err: %v", err)
			}
			for _, a := range tc.open {
				if !reachedBy(t, got, a) {
					t.Errorf("%s is not reachable, want reachable", a)
				}
			}
			for _, a := range tc.denied {
				if reachedBy(t, got, a) {
					t.Errorf("%s is reachable, want denied", a)
				}
			}
		})
	}
}

// TestGenerate_ExemptionHoleIsPerClass proves another class keeps the
// full reserved range, and the Config reserved list is not changed.
func TestGenerate_ExemptionHoleIsPerClass(t *testing.T) {
	t.Parallel()

	cfg := testCfg()
	tool, err := cfg.GenerateForClass(t.Context(),
		sb(setecv1alpha1.NetworkModeExternalOnly), exemptClass("tool", "10.60.0.66/32"))
	if err != nil {
		t.Fatalf("tool class: %v", err)
	}
	other, err := cfg.GenerateForClass(t.Context(),
		sb(setecv1alpha1.NetworkModeExternalOnly), exemptClass("other"))
	if err != nil {
		t.Fatalf("other class: %v", err)
	}
	if !reachedBy(t, tool, "10.60.0.66") {
		t.Error("tool class cannot reach 10.60.0.66")
	}
	if reachedBy(t, other, "10.60.0.66") {
		t.Error("another class reaches 10.60.0.66")
	}
	if len(cfg.ReservedCIDRs) != len(testReserved) || cfg.ReservedCIDRs[0] != "10.0.0.0/8" {
		t.Errorf("Config.ReservedCIDRs changed: %v", cfg.ReservedCIDRs)
	}
}

// TestGenerate_ExemptionHoleAllowListPort proves an allow-list entry for
// the exempt /32 renders one rule that reaches that /32 on its port only.
func TestGenerate_ExemptionHoleAllowListPort(t *testing.T) {
	t.Parallel()

	allow := setecv1alpha1.NetworkAllow{Host: "10.60.0.66", Port: 6443}
	tool, err := testCfg().GenerateForClass(t.Context(),
		sb(setecv1alpha1.NetworkModeEgressAllowList, allow), exemptClass("tool", "10.60.0.66/32"))
	if err != nil {
		t.Fatalf("tool class: %v", err)
	}
	if len(tool.Spec.Egress) != 2 {
		t.Fatalf("egress rules = %d, want 2 (DNS + the entry): %+v", len(tool.Spec.Egress), tool.Spec.Egress)
	}
	rule := tool.Spec.Egress[1]
	if len(rule.Ports) != 1 || rule.Ports[0].Port.IntValue() != 6443 {
		t.Errorf("rule ports = %+v, want only 6443", rule.Ports)
	}
	if !reachedBy(t, tool, "10.60.0.66") || reachedBy(t, tool, "10.60.0.67") {
		t.Error("the rule must reach 10.60.0.66 and not 10.60.0.67")
	}

	other, err := testCfg().GenerateForClass(t.Context(),
		sb(setecv1alpha1.NetworkModeEgressAllowList, allow), exemptClass("other"))
	if err != nil {
		t.Fatalf("other class: %v", err)
	}
	if len(other.Spec.Egress) != 1 {
		t.Errorf("another class got %d egress rules, want 1 (DNS only)", len(other.Spec.Egress))
	}
}

// TestSubtractExempt_IPv6Hole covers the same cut in IPv6.
func TestSubtractExempt_IPv6Hole(t *testing.T) {
	t.Parallel()

	got, err := subtractExempt([]string{"fc00::/7", "10.0.0.0/8"}, []string{"fd00:1::5/128"})
	if err != nil {
		t.Fatalf("subtractExempt() err: %v", err)
	}
	covers := func(a string) bool {
		for _, g := range got {
			if netip.MustParsePrefix(g).Contains(netip.MustParseAddr(a)) {
				return true
			}
		}
		return false
	}
	if covers("fd00:1::5") {
		t.Error("the exempt IPv6 address is still reserved")
	}
	for _, a := range []string{"fd00:1::4", "fd00:1::6", "fc00::1", "fdff::1", "10.1.1.1"} {
		if !covers(a) {
			t.Errorf("%s left the reserved list", a)
		}
	}
}
