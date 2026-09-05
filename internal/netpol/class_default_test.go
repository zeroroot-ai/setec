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
	"testing"

	networkingv1 "k8s.io/api/networking/v1"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
)

func classWithDefault(mode setecv1alpha1.NetworkMode, allow ...setecv1alpha1.NetworkAllow) *setecv1alpha1.SandboxClass {
	c := &setecv1alpha1.SandboxClass{
		Spec: setecv1alpha1.SandboxClassSpec{
			DefaultNetworkMode: mode,
			DefaultEgressAllow: allow,
		},
	}
	c.Name = "hardened"
	return c
}

// TestGenerateForClass_DefaultDenyWhenSandboxSilent is the core security
// assertion: a class with defaultNetworkMode=none must produce a deny-all
// policy for a Sandbox that declares no network block.
func TestGenerateForClass_DefaultDenyWhenSandboxSilent(t *testing.T) {
	t.Parallel()
	s := sb("") // nil Network
	cls := classWithDefault(setecv1alpha1.NetworkModeNone)

	got, err := testCfg().GenerateForClass(t.Context(), s, cls)
	if err != nil {
		t.Fatalf("GenerateForClass: %v", err)
	}
	if got == nil {
		t.Fatal("expected a deny-all NetworkPolicy, got nil (unrestricted egress)")
	}
	if len(got.Spec.Egress) != 0 {
		t.Fatalf("default-deny class must yield zero egress rules, got %v", got.Spec.Egress)
	}
	wantTypes := []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress}
	if len(got.Spec.PolicyTypes) != len(wantTypes) {
		t.Fatalf("policy types = %v, want %v", got.Spec.PolicyTypes, wantTypes)
	}
}

// TestGenerateForClass_DefaultEgressAllowList applies a class-level allowlist
// when the Sandbox is silent.
func TestGenerateForClass_DefaultEgressAllowList(t *testing.T) {
	t.Parallel()
	s := sb("")
	cls := classWithDefault(setecv1alpha1.NetworkModeEgressAllowList,
		setecv1alpha1.NetworkAllow{Host: "mirror.example.com", Port: 443})

	got, err := testCfg().GenerateForClass(t.Context(), s, cls)
	if err != nil {
		t.Fatalf("GenerateForClass: %v", err)
	}
	if got == nil {
		t.Fatal("expected an egress-allow-list policy, got nil")
	}
	// DNS rule + one allow rule.
	if len(got.Spec.Egress) != 2 {
		t.Fatalf("expected DNS + 1 allow rule, got %d rules", len(got.Spec.Egress))
	}
	if got.Annotations["setec.zeroroot.ai/allow-443"] != "mirror.example.com" {
		t.Fatalf("class allow host not recorded: %v", got.Annotations)
	}
}

// TestGenerateForClass_DefaultExternalOnly is the tool posture arriving
// through the class rather than through the Sandbox. This is the path a
// scanning workload actually takes: it says nothing about networking and
// inherits external-only from its class.
func TestGenerateForClass_DefaultExternalOnly(t *testing.T) {
	t.Parallel()
	s := sb("")
	cls := classWithDefault(setecv1alpha1.NetworkModeExternalOnly)

	got, err := testCfg().GenerateForClass(t.Context(), s, cls)
	if err != nil {
		t.Fatalf("GenerateForClass: %v", err)
	}
	// DNS + the public-egress rule.
	if len(got.Spec.Egress) != 2 {
		t.Fatalf("expected DNS + public egress, got %d rules", len(got.Spec.Egress))
	}
	pub := got.Spec.Egress[1]
	if pub.To[0].IPBlock.CIDR != AllCIDR {
		t.Errorf("public rule CIDR = %q, want %q", pub.To[0].IPBlock.CIDR, AllCIDR)
	}
	if len(pub.To[0].IPBlock.Except) != len(testReserved) {
		t.Errorf("except = %v, want all %d reserved ranges", pub.To[0].IPBlock.Except, len(testReserved))
	}
}

// TestGenerateForClass_ExplicitSandboxNetworkWins ensures a Sandbox that
// declares its own network is authoritative and the class default is not
// applied over it.
func TestGenerateForClass_ExplicitSandboxNetworkWins(t *testing.T) {
	t.Parallel()
	s := sb(setecv1alpha1.NetworkModeExternalOnly)
	cls := classWithDefault(setecv1alpha1.NetworkModeNone)

	got, err := testCfg().GenerateForClass(t.Context(), s, cls)
	if err != nil {
		t.Fatalf("GenerateForClass: %v", err)
	}
	if len(got.Spec.Egress) == 0 {
		t.Fatal("explicit external-only must win over the class default-deny")
	}
}

// TestGenerateForClass_UnstatedPostureIsDenyAll replaces the old
// back-compat case. A nil class, or a class that declares no default,
// used to mean "emit no policy". It now means deny-all: the absence of a
// stated posture is not a licence to skip the policy.
func TestGenerateForClass_UnstatedPostureIsDenyAll(t *testing.T) {
	t.Parallel()
	s := sb("")
	for name, cls := range map[string]*setecv1alpha1.SandboxClass{
		"nil-class":     nil,
		"empty-default": classWithDefault(""),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := testCfg().GenerateForClass(t.Context(), s, cls)
			if err != nil {
				t.Fatalf("GenerateForClass: %v", err)
			}
			if got == nil {
				t.Fatal("expected a deny-all policy, got nil (unrestricted egress)")
			}
			if len(got.Spec.Egress) != 0 || len(got.Spec.Ingress) != 0 {
				t.Fatalf("expected deny-all, got egress=%v ingress=%v", got.Spec.Egress, got.Spec.Ingress)
			}
		})
	}
}

// TestGenerateForClass_DoesNotMutateSandbox guards that resolving the
// effective posture never writes back onto the caller's Sandbox.
func TestGenerateForClass_DoesNotMutateSandbox(t *testing.T) {
	t.Parallel()
	s := sb("")
	cls := classWithDefault(setecv1alpha1.NetworkModeNone)

	if _, err := testCfg().GenerateForClass(t.Context(), s, cls); err != nil {
		t.Fatalf("GenerateForClass: %v", err)
	}
	if s.Spec.Network != nil {
		t.Fatalf("GenerateForClass mutated the caller's Sandbox: %+v", s.Spec.Network)
	}
}

func TestGenerateForClass_NilSandbox(t *testing.T) {
	t.Parallel()
	if _, err := testCfg().GenerateForClass(t.Context(), nil, classWithDefault(setecv1alpha1.NetworkModeNone)); err == nil {
		t.Fatal("expected error on nil sandbox")
	}
}

// TestEffectiveMode pins the resolution order the admission webhook
// depends on: Sandbox first, then class default, then none.
func TestEffectiveMode(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		sandbox *setecv1alpha1.Sandbox
		class   *setecv1alpha1.SandboxClass
		want    setecv1alpha1.NetworkMode
	}{
		"sandbox wins over class": {
			sb(setecv1alpha1.NetworkModeExternalOnly),
			classWithDefault(setecv1alpha1.NetworkModeNone),
			setecv1alpha1.NetworkModeExternalOnly,
		},
		"class default fills the gap": {
			sb(""),
			classWithDefault(setecv1alpha1.NetworkModeExternalOnly),
			setecv1alpha1.NetworkModeExternalOnly,
		},
		"nothing stated is none": {
			sb(""), nil, setecv1alpha1.NetworkModeNone,
		},
		"empty class default is none": {
			sb(""), classWithDefault(""), setecv1alpha1.NetworkModeNone,
		},
		"nil sandbox is none": {
			nil, classWithDefault(setecv1alpha1.NetworkModeExternalOnly), setecv1alpha1.NetworkModeNone,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := EffectiveMode(tc.sandbox, tc.class); got != tc.want {
				t.Fatalf("EffectiveMode() = %q, want %q", got, tc.want)
			}
		})
	}
}
