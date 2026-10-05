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

// TestSandboxClassWebhook_ValidateScratch covers the two scratch values of a
// class (ADR-0146, setec#172).
func TestSandboxClassWebhook_ValidateScratch(t *testing.T) {
	t.Parallel()
	mk := func(def, max *resource.Quantity) *setecv1alpha1.SandboxClass {
		cls := mkSandboxClass("scratch", "", mkRuntime(setecruntime.BackendGVisor))
		if def != nil {
			cls.Spec.DefaultResources = &setecv1alpha1.Resources{VCPU: 1, Memory: resource.MustParse("1Gi"), Scratch: def}
		}
		if max != nil {
			cls.Spec.MaxResources = &setecv1alpha1.Resources{VCPU: 4, Memory: resource.MustParse("8Gi"), Scratch: max}
		}
		return cls
	}
	for _, tc := range []struct {
		name    string
		class   *setecv1alpha1.SandboxClass
		wantMsg string
	}{
		{name: "no scratch values → accept", class: mk(nil, nil)},
		{name: "default under the 10Gi default ceiling → accept", class: mk(quantityPtr("5Gi"), nil)},
		{name: "raised ceiling and default under it → accept", class: mk(quantityPtr("20Gi"), quantityPtr("40Gi"))},
		{name: "default above the 10Gi default ceiling → reject", class: mk(quantityPtr("11Gi"), nil),
			wantMsg: "exceeds the scratch ceiling of the class (10Gi)"},
		{name: "default above the class ceiling → reject", class: mk(quantityPtr("3Gi"), quantityPtr("2Gi")),
			wantMsg: "spec.defaultResources.scratch"},
		{name: "zero ceiling → reject", class: mk(nil, quantityPtr("0")),
			wantMsg: "spec.maxResources.scratch"},
		{name: "negative default → reject", class: mk(quantityPtr("-1Gi"), nil),
			wantMsg: "spec.defaultResources.scratch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := webhookWith(fakeClientWithNS(t, gateNamespaceUnlabelled()), baseConfig())
			_, err := w.ValidateCreate(context.Background(), tc.class)
			switch {
			case tc.wantMsg == "" && err != nil:
				t.Fatalf("expected admit, got %v", err)
			case tc.wantMsg != "" && err == nil:
				t.Fatal("expected rejection, got admit")
			case tc.wantMsg != "" && !strings.Contains(err.Error(), tc.wantMsg):
				t.Fatalf("error %q does not contain %q", err.Error(), tc.wantMsg)
			}
		})
	}
}
