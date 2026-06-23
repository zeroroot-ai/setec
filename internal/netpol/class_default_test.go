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
// policy for a Sandbox that declares no network block — egress is
// default-deny per class, not unrestricted.
func TestGenerateForClass_DefaultDenyWhenSandboxSilent(t *testing.T) {
	t.Parallel()
	s := sb("") // nil Network
	cls := classWithDefault(setecv1alpha1.NetworkModeNone)

	got, err := GenerateForClass(s, cls)
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
		setecv1alpha1.NetworkAllow{Host: "mirror.internal", Port: 443})

	got, err := GenerateForClass(s, cls)
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
	if got.Annotations["setec.zeroroot.ai/allow-443"] != "mirror.internal" {
		t.Fatalf("class allow host not recorded: %v", got.Annotations)
	}
}

// TestGenerateForClass_ExplicitSandboxNetworkWins ensures a Sandbox that
// declares its own network is authoritative and the class default is not
// applied over it.
func TestGenerateForClass_ExplicitSandboxNetworkWins(t *testing.T) {
	t.Parallel()
	s := sb(setecv1alpha1.NetworkModeFull) // explicit full
	cls := classWithDefault(setecv1alpha1.NetworkModeNone)

	got, err := GenerateForClass(s, cls)
	if err != nil {
		t.Fatalf("GenerateForClass: %v", err)
	}
	if got != nil {
		t.Fatalf("explicit mode=full must win over class default-deny; expected nil policy, got %+v", got.Spec)
	}
}

// TestGenerateForClass_NoClassDefaultPreservesBackCompat ensures a class with
// no default posture (or a nil class) keeps the historical mode=full →
// no-policy behaviour.
func TestGenerateForClass_NoClassDefaultPreservesBackCompat(t *testing.T) {
	t.Parallel()
	s := sb("")
	for name, cls := range map[string]*setecv1alpha1.SandboxClass{
		"nil-class":     nil,
		"empty-default": classWithDefault(""),
		"full-default":  classWithDefault(setecv1alpha1.NetworkModeFull),
	} {
		t.Run(name, func(t *testing.T) {
			got, err := GenerateForClass(s, cls)
			if err != nil {
				t.Fatalf("GenerateForClass: %v", err)
			}
			if got != nil {
				t.Fatalf("expected nil policy (back-compat), got %+v", got.Spec)
			}
		})
	}
}

// TestGenerateForClass_DoesNotMutateSandbox guards that synthesising the
// effective network block never writes back onto the caller's Sandbox.
func TestGenerateForClass_DoesNotMutateSandbox(t *testing.T) {
	t.Parallel()
	s := sb("")
	cls := classWithDefault(setecv1alpha1.NetworkModeNone)

	if _, err := GenerateForClass(s, cls); err != nil {
		t.Fatalf("GenerateForClass: %v", err)
	}
	if s.Spec.Network != nil {
		t.Fatalf("GenerateForClass mutated the caller's Sandbox: %+v", s.Spec.Network)
	}
}

func TestGenerateForClass_NilSandbox(t *testing.T) {
	t.Parallel()
	if _, err := GenerateForClass(nil, classWithDefault(setecv1alpha1.NetworkModeNone)); err == nil {
		t.Fatal("expected error on nil sandbox")
	}
}
