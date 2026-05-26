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

package class

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
)

// qty is a tiny helper to build a resource.Quantity in a table-driven style.
func qty(s string) resource.Quantity {
	return resource.MustParse(s)
}

func TestValidate(t *testing.T) {
	t.Parallel()

	// A reference "standard" class used by most cases; individual cases
	// override the fields they care about via a shallow copy.
	standardClass := func() *setecv1alpha1.SandboxClass {
		return &setecv1alpha1.SandboxClass{
			ObjectMeta: metav1.ObjectMeta{Name: "standard"},
			Spec: setecv1alpha1.SandboxClassSpec{
				VMM: setecv1alpha1.VMMFirecracker,
				MaxResources: &setecv1alpha1.Resources{
					VCPU:   4,
					Memory: qty("8Gi"),
				},
				AllowedNetworkModes: []setecv1alpha1.NetworkMode{
					setecv1alpha1.NetworkModeNone,
					setecv1alpha1.NetworkModeEgressAllowList,
				},
			},
		}
	}

	baseSandbox := func() *setecv1alpha1.Sandbox {
		return &setecv1alpha1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{Name: "sb", Namespace: "ns"},
			Spec: setecv1alpha1.SandboxSpec{
				Image:   "alpine:3.19",
				Command: []string{"sh", "-c", "echo hi"},
				Resources: setecv1alpha1.Resources{
					VCPU:   2,
					Memory: qty("2Gi"),
				},
				Network: &setecv1alpha1.Network{
					Mode: setecv1alpha1.NetworkModeNone,
				},
			},
		}
	}

	tests := []struct {
		name   string
		sb     *setecv1alpha1.Sandbox
		cls    *setecv1alpha1.SandboxClass
		expect []ConstraintViolation
	}{
		{
			name:   "happy path: sandbox within class",
			sb:     baseSandbox(),
			cls:    standardClass(),
			expect: nil,
		},
		{
			name: "vcpu exceeds max",
			sb: func() *setecv1alpha1.Sandbox {
				sb := baseSandbox()
				sb.Spec.Resources.VCPU = 8
				return sb
			}(),
			cls: standardClass(),
			expect: []ConstraintViolation{{
				Field:   "spec.resources.vcpu",
				Message: `Sandbox requests 8 vcpu but SandboxClass "standard" allows maximum 4 vcpu`,
			}},
		},
		{
			name: "memory exceeds max",
			sb: func() *setecv1alpha1.Sandbox {
				sb := baseSandbox()
				sb.Spec.Resources.Memory = qty("16Gi")
				return sb
			}(),
			cls: standardClass(),
			expect: []ConstraintViolation{{
				Field:   "spec.resources.memory",
				Message: `Sandbox requests 16Gi memory but SandboxClass "standard" allows maximum 8Gi`,
			}},
		},
		{
			name: "vcpu and memory both exceed max",
			sb: func() *setecv1alpha1.Sandbox {
				sb := baseSandbox()
				sb.Spec.Resources.VCPU = 8
				sb.Spec.Resources.Memory = qty("16Gi")
				return sb
			}(),
			cls: standardClass(),
			expect: []ConstraintViolation{
				{
					Field:   "spec.resources.vcpu",
					Message: `Sandbox requests 8 vcpu but SandboxClass "standard" allows maximum 4 vcpu`,
				},
				{
					Field:   "spec.resources.memory",
					Message: `Sandbox requests 16Gi memory but SandboxClass "standard" allows maximum 8Gi`,
				},
			},
		},
		{
			name: "network mode not allowed",
			sb: func() *setecv1alpha1.Sandbox {
				sb := baseSandbox()
				sb.Spec.Network = &setecv1alpha1.Network{Mode: setecv1alpha1.NetworkModeFull}
				return sb
			}(),
			cls: standardClass(),
			expect: []ConstraintViolation{{
				Field:   "spec.network.mode",
				Message: `Sandbox requests network.mode="full" but SandboxClass "standard" only allows modes [none egress-allow-list]`,
			}},
		},
		{
			name: "network mode check skipped when network nil",
			sb: func() *setecv1alpha1.Sandbox {
				sb := baseSandbox()
				sb.Spec.Network = nil
				return sb
			}(),
			cls:    standardClass(),
			expect: nil,
		},
		{
			name: "empty allowed modes means any mode permitted",
			sb: func() *setecv1alpha1.Sandbox {
				sb := baseSandbox()
				sb.Spec.Network = &setecv1alpha1.Network{Mode: setecv1alpha1.NetworkModeFull}
				return sb
			}(),
			cls: func() *setecv1alpha1.SandboxClass {
				c := standardClass()
				c.Spec.AllowedNetworkModes = nil
				return c
			}(),
			expect: nil,
		},
		{
			name: "class without MaxResources imposes no ceiling",
			sb: func() *setecv1alpha1.Sandbox {
				sb := baseSandbox()
				sb.Spec.Resources.VCPU = 32
				sb.Spec.Resources.Memory = qty("64Gi")
				return sb
			}(),
			cls: func() *setecv1alpha1.SandboxClass {
				c := standardClass()
				c.Spec.MaxResources = nil
				return c
			}(),
			expect: nil,
		},
		{
			name:   "nil class yields no violations",
			sb:     baseSandbox(),
			cls:    nil,
			expect: nil,
		},
		{
			name:   "nil sandbox yields a structural violation",
			sb:     nil,
			cls:    standardClass(),
			expect: []ConstraintViolation{{Field: "", Message: "sandbox is nil"}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := Validate(tc.sb, tc.cls)
			if diff := cmp.Diff(tc.expect, got); diff != "" {
				t.Fatalf("Validate() diff (-want +got):\n%s", diff)
			}
		})
	}
}

// TestConstraintViolation_String locks in the "<field>: <message>"
// rendering the webhook and controller both rely on.
func TestConstraintViolation_String(t *testing.T) {
	t.Parallel()
	v := ConstraintViolation{Field: "spec.resources.vcpu", Message: "too high"}
	if got, want := v.String(), "spec.resources.vcpu: too high"; got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
}
