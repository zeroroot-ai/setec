// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package webhook

import (
	"context"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
	setecruntime "github.com/zeroroot-ai/setec/internal/runtime"
)

func quantityPtr(s string) *resource.Quantity {
	q := resource.MustParse(s)
	return &q
}

// TestSandboxClassWebhook_ValidateRequests covers the scheduler
// reservation rules (setec#372): each set value must be positive, and
// when the class states a ceiling the reservation must fit under it.
func TestSandboxClassWebhook_ValidateRequests(t *testing.T) {
	t.Parallel()

	mk := func(req *setecv1alpha1.ResourceRequests, max *setecv1alpha1.Resources) *setecv1alpha1.SandboxClass {
		cls := mkSandboxClass("req", "", mkRuntime(setecruntime.BackendGVisor))
		cls.Spec.Requests = req
		cls.Spec.MaxResources = max
		return cls
	}
	ceiling := &setecv1alpha1.Resources{VCPU: 2, Memory: resource.MustParse("4Gi")}

	tests := []struct {
		name    string
		class   *setecv1alpha1.SandboxClass
		wantErr bool
		wantMsg string
	}{
		{
			name:  "unset → accept (requests equal limits)",
			class: mk(nil, ceiling),
		},
		{
			name:  "idle member shape under the ceiling → accept",
			class: mk(&setecv1alpha1.ResourceRequests{CPU: quantityPtr("250m"), Memory: quantityPtr("768Mi")}, ceiling),
		},
		{
			name:  "only cpu set → accept",
			class: mk(&setecv1alpha1.ResourceRequests{CPU: quantityPtr("1")}, nil),
		},
		{
			name:  "equal to the ceiling → accept",
			class: mk(&setecv1alpha1.ResourceRequests{CPU: quantityPtr("2"), Memory: quantityPtr("4Gi")}, ceiling),
		},
		{
			name:    "zero cpu → reject",
			class:   mk(&setecv1alpha1.ResourceRequests{CPU: quantityPtr("0")}, nil),
			wantErr: true,
			wantMsg: "spec.requests.cpu",
		},
		{
			name:    "negative memory → reject",
			class:   mk(&setecv1alpha1.ResourceRequests{Memory: quantityPtr("-1Mi")}, nil),
			wantErr: true,
			wantMsg: "spec.requests.memory",
		},
		{
			name:    "cpu above maxResources.vcpu → reject",
			class:   mk(&setecv1alpha1.ResourceRequests{CPU: quantityPtr("2500m")}, ceiling),
			wantErr: true,
			wantMsg: "exceeds spec.maxResources.vcpu (2)",
		},
		{
			name:    "memory above maxResources.memory → reject",
			class:   mk(&setecv1alpha1.ResourceRequests{Memory: quantityPtr("5Gi")}, ceiling),
			wantErr: true,
			wantMsg: "exceeds spec.maxResources.memory (4Gi)",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := webhookWith(fakeClientWithNS(t, gateNamespaceUnlabelled()), baseConfig())
			_, err := w.ValidateCreate(context.Background(), tc.class)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected rejection, got admit")
				}
				if !strings.Contains(err.Error(), tc.wantMsg) {
					t.Fatalf("error %q does not contain %q", err.Error(), tc.wantMsg)
				}
				return
			}
			if err != nil {
				t.Fatalf("expected admit, got %v", err)
			}
		})
	}
}
