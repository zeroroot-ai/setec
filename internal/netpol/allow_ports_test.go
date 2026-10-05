package netpol

import (
	"errors"
	"testing"

	"github.com/google/go-cmp/cmp"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
)

// rangeRule returns the NetworkPolicyPort of a port range.
func rangeRule(proto corev1.Protocol, port, endPort int32) networkingv1.NetworkPolicyPort {
	is := intstr.FromInt32(port)
	return networkingv1.NetworkPolicyPort{Protocol: &proto, Port: &is, EndPort: &endPort}
}

// TestGenerate_AllowListPortsForm covers the list form of an entry: a
// range, a single port and a UDP port become the ports of one rule, on
// the same peers as the one-port form.
func TestGenerate_AllowListPortsForm(t *testing.T) {
	t.Parallel()

	got, err := testCfg().Generate(t.Context(), sb(setecv1alpha1.NetworkModeEgressAllowList,
		setecv1alpha1.NetworkAllow{
			Host: "api.example.com",
			Ports: []setecv1alpha1.NetworkAllowPort{
				{Port: 1, EndPort: new(int32(65535))},
				{Protocol: corev1.ProtocolTCP, Port: 8443, EndPort: new(int32(8443))},
				{Protocol: corev1.ProtocolUDP, Port: 161},
				{Protocol: corev1.ProtocolUDP, Port: 5000, EndPort: new(int32(5100))},
			},
		}))
	if err != nil {
		t.Fatalf("Generate() err: %v", err)
	}

	if n := len(got.Spec.Egress); n != 2 {
		t.Fatalf("len(egress) = %d, want 2 (DNS and one rule for the entry)", n)
	}
	rule := got.Spec.Egress[1]

	wantPeers := []networkingv1.NetworkPolicyPeer{
		{IPBlock: &networkingv1.IPBlock{CIDR: "203.0.113.10/32"}},
		{IPBlock: &networkingv1.IPBlock{CIDR: "203.0.113.11/32"}},
	}
	if diff := cmp.Diff(wantPeers, rule.To); diff != "" {
		t.Errorf("rule peers diff (-want +got):\n%s", diff)
	}

	wantPorts := []networkingv1.NetworkPolicyPort{
		rangeRule(corev1.ProtocolTCP, 1, 65535),
		// An endPort equal to the port is one port, so the rule carries
		// no endPort for it.
		portRule(8443),
		udpRule(161),
		rangeRule(corev1.ProtocolUDP, 5000, 5100),
	}
	if diff := cmp.Diff(wantPorts, rule.Ports); diff != "" {
		t.Errorf("rule ports diff (-want +got):\n%s", diff)
	}

	wantAnnotations := map[string]string{
		"setec.zeroroot.ai/allow-1-65535":       "api.example.com",
		"setec.zeroroot.ai/allow-8443":          "api.example.com",
		"setec.zeroroot.ai/allow-udp-161":       "api.example.com",
		"setec.zeroroot.ai/allow-udp-5000-5100": "api.example.com",
	}
	if diff := cmp.Diff(wantAnnotations, got.Annotations); diff != "" {
		t.Errorf("annotations diff (-want +got):\n%s", diff)
	}
}

// TestGenerate_AllowListRefusesMalformedPorts is the failing fixture of
// the port check. A NetworkPolicy rule with no ports permits every port
// of every protocol, so an entry that names no valid port must abort the
// policy. It must never become a rule.
func TestGenerate_AllowListRefusesMalformedPorts(t *testing.T) {
	t.Parallel()

	cases := map[string]setecv1alpha1.NetworkAllow{
		"neither port nor ports": {Host: "api.example.com"},
		"an empty ports list":    {Host: "api.example.com", Ports: []setecv1alpha1.NetworkAllowPort{}},
		"both port and ports": {
			Host: "api.example.com", Port: 443,
			Ports: []setecv1alpha1.NetworkAllowPort{{Port: 80}},
		},
		"a port above 65535": {Host: "api.example.com", Port: 70000},
		"a negative port":    {Host: "api.example.com", Port: -1},
		"a ports entry with port 0": {
			Host: "api.example.com", Ports: []setecv1alpha1.NetworkAllowPort{{Port: 0}},
		},
		"an endPort below the port": {
			Host: "api.example.com", Ports: []setecv1alpha1.NetworkAllowPort{{Port: 443, EndPort: new(int32(80))}},
		},
		"an endPort above 65535": {
			Host: "api.example.com", Ports: []setecv1alpha1.NetworkAllowPort{{Port: 1, EndPort: new(int32(70000))}},
		},
		"a protocol that is not TCP or UDP": {
			Host: "api.example.com", Ports: []setecv1alpha1.NetworkAllowPort{{Protocol: corev1.ProtocolSCTP, Port: 443}},
		},
		"one good entry and one bad entry": {
			Host: "api.example.com", Ports: []setecv1alpha1.NetworkAllowPort{{Port: 443}, {Port: 0}},
		},
	}

	for name, allow := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got, err := testCfg().Generate(t.Context(), sb(setecv1alpha1.NetworkModeEgressAllowList, allow))
			if !errors.Is(err, ErrInvalidPorts) {
				t.Fatalf("Generate() err = %v, want ErrInvalidPorts", err)
			}
			if got != nil {
				t.Fatalf("Generate() returned a policy with an error: %+v", got.Spec.Egress)
			}
		})
	}
}

// TestGenerate_AllowListDroppedEntryNamesEachPort proves the two drop
// annotations name each port of a dropped entry, in the same "host:port"
// form as the one-port entry.
func TestGenerate_AllowListDroppedEntryNamesEachPort(t *testing.T) {
	t.Parallel()

	got, err := testCfg().Generate(t.Context(), sb(setecv1alpha1.NetworkModeEgressAllowList,
		setecv1alpha1.NetworkAllow{
			Host: "daemon.platform.svc", CIDR: "10.96.0.0/12",
			Ports: []setecv1alpha1.NetworkAllowPort{
				{Port: 1, EndPort: new(int32(1024))},
				{Protocol: corev1.ProtocolUDP, Port: 53},
			},
		},
		setecv1alpha1.NetworkAllow{Host: "internal.example.com", CIDR: "10.96.0.0/12", Port: 50051},
	))
	if err != nil {
		t.Fatalf("Generate() err: %v", err)
	}

	if n := len(got.Spec.Egress); n != 1 {
		t.Fatalf("len(egress) = %d, want 1: a target in a reserved range gets no rule", n)
	}
	want := "daemon.platform.svc:1-1024, daemon.platform.svc:udp/53, internal.example.com:50051"
	if gotValue := got.Annotations[AnnotationSuppressed]; gotValue != want {
		t.Fatalf("%s = %q, want %q", AnnotationSuppressed, gotValue, want)
	}
}
