package frontend

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	corev1 "k8s.io/api/core/v1"

	setecv1grpc "github.com/zeroroot-ai/setec/api/grpc/v1"
	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
)

// TestNetworkAllowFromProto proves a Launch request carries each field of
// an allow-list entry into the Sandbox: the address block, and each port
// with its protocol and its range.
func TestNetworkAllowFromProto(t *testing.T) {
	t.Parallel()

	end := int32(65535)
	cases := map[string]struct {
		in   *setecv1grpc.NetworkAllow
		want setecv1alpha1.NetworkAllow
	}{
		"one TCP port": {
			in:   &setecv1grpc.NetworkAllow{Host: "api.example.com", Port: 443},
			want: setecv1alpha1.NetworkAllow{Host: "api.example.com", Port: 443},
		},
		"an address block": {
			in:   &setecv1grpc.NetworkAllow{Host: "lab", Port: 22, Cidr: "198.51.100.0/24"},
			want: setecv1alpha1.NetworkAllow{Host: "lab", Port: 22, CIDR: "198.51.100.0/24"},
		},
		"a ports list": {
			in: &setecv1grpc.NetworkAllow{
				Host: "198.51.100.7",
				Ports: []*setecv1grpc.NetworkAllowPort{
					{Port: 1, EndPort: 65535},
					{Protocol: "UDP", Port: 161},
				},
			},
			want: setecv1alpha1.NetworkAllow{
				Host: "198.51.100.7",
				Ports: []setecv1alpha1.NetworkAllowPort{
					{Port: 1, EndPort: &end},
					{Protocol: corev1.ProtocolUDP, Port: 161},
				},
			},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if diff := cmp.Diff(tc.want, networkAllowFromProto(tc.in)); diff != "" {
				t.Fatalf("networkAllowFromProto() diff (-want +got):\n%s", diff)
			}
		})
	}
}
