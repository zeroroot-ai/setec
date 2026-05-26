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

// allCIDRPeer returns a 0.0.0.0/0 peer for egress rules.
func allCIDRPeer() networkingv1.NetworkPolicyPeer {
	return networkingv1.NetworkPolicyPeer{
		IPBlock: &networkingv1.IPBlock{CIDR: "0.0.0.0/0"},
	}
}

func TestGenerate_ModeFullReturnsNil(t *testing.T) {
	t.Parallel()

	got, err := Generate(sb(setecv1alpha1.NetworkModeFull))
	if err != nil {
		t.Fatalf("Generate() err: %v", err)
	}
	if got != nil {
		t.Fatalf("Generate() for mode=full should be nil, got %+v", got)
	}
}

func TestGenerate_NetworkAbsentReturnsNil(t *testing.T) {
	t.Parallel()

	// sb("") returns a Sandbox with Network == nil.
	got, err := Generate(sb(""))
	if err != nil {
		t.Fatalf("Generate() err: %v", err)
	}
	if got != nil {
		t.Fatalf("Generate() for absent network should be nil, got %+v", got)
	}
}

func TestGenerate_ModeNoneDeniesAll(t *testing.T) {
	t.Parallel()

	s := sb(setecv1alpha1.NetworkModeNone)
	got, err := Generate(s)
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

func TestGenerate_ModeEgressAllowListSinglePort(t *testing.T) {
	t.Parallel()

	s := sb(setecv1alpha1.NetworkModeEgressAllowList,
		setecv1alpha1.NetworkAllow{Host: "api.example.com", Port: 443})

	got, err := Generate(s)
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
				// Baseline DNS rule.
				{
					To:    []networkingv1.NetworkPolicyPeer{allCIDRPeer()},
					Ports: []networkingv1.NetworkPolicyPort{udpRule(53), portRule(53)},
				},
				// User-declared allow entry.
				{
					To:    []networkingv1.NetworkPolicyPeer{allCIDRPeer()},
					Ports: []networkingv1.NetworkPolicyPort{portRule(443)},
				},
			},
		},
	}

	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("Generate() diff (-want +got):\n%s", diff)
	}
}

func TestGenerate_ModeEgressAllowListMultiplePorts(t *testing.T) {
	t.Parallel()

	s := sb(setecv1alpha1.NetworkModeEgressAllowList,
		setecv1alpha1.NetworkAllow{Host: "api.example.com", Port: 443},
		setecv1alpha1.NetworkAllow{Host: "grpc.example.com", Port: 50051},
	)

	got, err := Generate(s)
	if err != nil {
		t.Fatalf("Generate() err: %v", err)
	}

	// Assert individual characteristics rather than the full policy;
	// the previous test already locked in full-shape rendering.
	if got == nil {
		t.Fatal("Generate() returned nil for egress-allow-list")
	}
	if want := "my-sb-netpol"; got.Name != want {
		t.Errorf("Name = %q, want %q", got.Name, want)
	}
	if got.Annotations["setec.zeroroot.ai/allow-443"] != "api.example.com" {
		t.Errorf("missing annotation for 443")
	}
	if got.Annotations["setec.zeroroot.ai/allow-50051"] != "grpc.example.com" {
		t.Errorf("missing annotation for 50051")
	}
	// 1 DNS baseline + 2 user rules = 3 egress rules.
	if want := 3; len(got.Spec.Egress) != want {
		t.Errorf("egress rules = %d, want %d", len(got.Spec.Egress), want)
	}
	// Expect both PolicyTypes set.
	if len(got.Spec.PolicyTypes) != 2 {
		t.Errorf("PolicyTypes = %v, want ingress+egress", got.Spec.PolicyTypes)
	}
	// Ingress rules must remain nil so "deny all ingress" applies.
	if got.Spec.Ingress != nil {
		t.Errorf("Ingress = %v, want nil", got.Spec.Ingress)
	}
}

func TestGenerate_ModeEgressAllowListDNSSoundingHost(t *testing.T) {
	t.Parallel()

	// DNS-sounding hosts are recorded as annotations but NOT resolved.
	// This test locks in the documented behaviour so a future change
	// that silently starts resolving hostnames is caught.
	s := sb(setecv1alpha1.NetworkModeEgressAllowList,
		setecv1alpha1.NetworkAllow{Host: "private.internal.corp", Port: 8080})

	got, err := Generate(s)
	if err != nil {
		t.Fatalf("Generate() err: %v", err)
	}

	if got.Annotations["setec.zeroroot.ai/allow-8080"] != "private.internal.corp" {
		t.Fatalf("DNS host should be recorded as annotation, got %v", got.Annotations)
	}
	// The egress rule should use 0.0.0.0/0 (not a resolved CIDR) because
	// hostname resolution is out of scope for the pure translator.
	// Skip the baseline DNS rule (index 0).
	userRule := got.Spec.Egress[1]
	if got := userRule.To[0].IPBlock.CIDR; got != "0.0.0.0/0" {
		t.Errorf("egress CIDR = %q, want 0.0.0.0/0", got)
	}
}

func TestGenerate_NilSandbox(t *testing.T) {
	t.Parallel()
	_, err := Generate(nil)
	if err == nil || !errors.Is(err, ErrNilSandbox) {
		t.Fatalf("expected ErrNilSandbox, got %v", err)
	}
}

func TestGenerate_UnknownMode(t *testing.T) {
	t.Parallel()
	s := sb(setecv1alpha1.NetworkMode("mystery"))
	_, err := Generate(s)
	if err == nil || !errors.Is(err, ErrUnknownMode) {
		t.Fatalf("expected ErrUnknownMode, got %v", err)
	}
}
