// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package webhook

import (
	"context"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
	setecruntime "github.com/zeroroot-ai/setec/internal/runtime"
)

// TestSandboxClassWebhook_ValidatePreWarm covers the ADR-0004
// declarative pre-warm pool surface: pool size, image, and TTL are one
// coherent knob, and an active pool requires the kata-fc backend
// (pool restore drives the Kata VM's Firecracker socket).
func TestSandboxClassWebhook_ValidatePreWarm(t *testing.T) {
	t.Parallel()

	mk := func(size int32, image string, ttl *metav1.Duration, backend string) *setecv1alpha1.SandboxClass {
		cls := mkSandboxClass("pw", "", mkRuntime(backend))
		cls.Spec.PreWarmPoolSize = size
		cls.Spec.PreWarmImage = image
		cls.Spec.PreWarmTTL = ttl
		return cls
	}

	tests := []struct {
		name    string
		class   *setecv1alpha1.SandboxClass
		wantErr bool
		wantMsg string
	}{
		{
			name:  "pool disabled → accept",
			class: mk(0, "", nil, setecruntime.BackendKataFC),
		},
		{
			name:  "full trio on kata-fc → accept",
			class: mk(3, "ghcr.io/org/tools:v1", &metav1.Duration{Duration: time.Hour}, setecruntime.BackendKataFC),
		},
		{
			name:  "pool without TTL → accept (node-agent defaults 24h)",
			class: mk(1, "ghcr.io/org/tools:v1", nil, setecruntime.BackendKataFC),
		},
		{
			name:    "pool size without image → reject",
			class:   mk(2, "", nil, setecruntime.BackendKataFC),
			wantErr: true,
			wantMsg: "requires preWarmImage",
		},
		{
			name:    "zero TTL → reject",
			class:   mk(1, "ghcr.io/org/tools:v1", &metav1.Duration{}, setecruntime.BackendKataFC),
			wantErr: true,
			wantMsg: "positive duration",
		},
		{
			name:    "negative TTL → reject",
			class:   mk(1, "ghcr.io/org/tools:v1", &metav1.Duration{Duration: -time.Minute}, setecruntime.BackendKataFC),
			wantErr: true,
			wantMsg: "positive duration",
		},
		{
			name:    "TTL invalid even when pool disabled → reject",
			class:   mk(0, "", &metav1.Duration{Duration: -time.Second}, setecruntime.BackendKataFC),
			wantErr: true,
			wantMsg: "positive duration",
		},
		{
			name:  "image without pool size → accept (lease pool template only)",
			class: mk(0, "ghcr.io/org/tools:v1", nil, setecruntime.BackendKataFC),
		},
		{
			name:    "pool on gvisor → reject",
			class:   mk(1, "ghcr.io/org/tools:v1", nil, setecruntime.BackendGVisor),
			wantErr: true,
			wantMsg: "require the \"kata-fc\" backend",
		},
		{
			name:    "pool on kata-qemu → reject",
			class:   mk(1, "ghcr.io/org/tools:v1", nil, setecruntime.BackendKataQEMU),
			wantErr: true,
			wantMsg: "require the \"kata-fc\" backend",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := webhookWith(fakeClientWithNS(t, gateNamespaceUnlabelled()), baseConfig())
			_, err := w.ValidateCreate(context.Background(), tc.class)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected a validation error, got nil")
				}
				if !strings.Contains(err.Error(), tc.wantMsg) {
					t.Fatalf("error %q does not contain %q", err.Error(), tc.wantMsg)
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
	cls := mkSandboxClass("pw-nil", "", nil)
	cls.Spec.PreWarmPoolSize = 2 // no image

	w := webhookWith(fakeClientWithNS(t, gateNamespaceUnlabelled()), baseConfig())
	_, err := w.ValidateCreate(context.Background(), cls)
	if err == nil || !strings.Contains(err.Error(), "requires preWarmImage") {
		t.Fatalf("expected preWarmImage pairing error with nil Runtime, got: %v", err)
	}
}
