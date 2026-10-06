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

// TestSandboxClassWebhook_ValidatePreWarm covers the warm pool of a class
// (setec#103): an active pool needs an image with a digest and the default
// resources of the class.
func TestSandboxClassWebhook_ValidatePreWarm(t *testing.T) {
	t.Parallel()
	digest := "ghcr.io/org/tools@sha256:" + strings.Repeat("a", 64)
	mk := func(size int32, image string) *setecv1alpha1.SandboxClass {
		cls := mkSandboxClass("pw", mkRuntime(setecruntime.BackendLauncher))
		cls.Spec.PreWarmPoolSize = size
		cls.Spec.PreWarmImage = image
		return cls
	}
	tests := []struct {
		name    string
		class   *setecv1alpha1.SandboxClass
		wantErr bool
		wantMsg string
	}{
		{name: "no pool", class: mk(0, "")},
		{name: "a pool with a digest and a size", class: withDefaultResources(mk(2, digest))},
		{name: "a pool with no image", class: withDefaultResources(mk(2, "")), wantErr: true, wantMsg: "requires preWarmImage"},
		{name: "a pool with a tag", class: withDefaultResources(mk(2, "ghcr.io/org/tools:v1")), wantErr: true, wantMsg: "digest"},
		{name: "a pool with no size", class: mk(2, digest), wantErr: true, wantMsg: "defaultResources"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := classWebhook(t).ValidateCreate(context.Background(), tc.class)
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), tc.wantMsg) {
					t.Fatalf("error = %v, want %q", err, tc.wantMsg)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected validation error: %v", err)
			}
		})
	}
}

// TestSandboxClassWebhook_ValidatePreWarm_NilRuntime pins that the
// trio rules still apply when Runtime is nil (webhook defaulting
// bypassed, e.g. --dry-run): the pairing errors must surface even
// without a backend to check.
func TestSandboxClassWebhook_ValidatePreWarm_NilRuntime(t *testing.T) {
	t.Parallel()
	cls := mkSandboxClass("pw-nil", nil)
	cls.Spec.PreWarmPoolSize = 2 // no image

	w := classWebhook(t)
	_, err := w.ValidateCreate(context.Background(), cls)
	if err == nil || !strings.Contains(err.Error(), "requires preWarmImage") {
		t.Fatalf("expected preWarmImage pairing error with nil Runtime, got: %v", err)
	}
}

func withDefaultResources(cls *setecv1alpha1.SandboxClass) *setecv1alpha1.SandboxClass {
	cls.Spec.DefaultResources = &setecv1alpha1.Resources{VCPU: 1, Memory: resource.MustParse("1Gi")}
	return cls
}
