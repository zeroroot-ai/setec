// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package webhook

import (
	"context"
	"strings"
	"testing"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
)

// TestSandboxClassWebhook_DefaultNetworkModeConsistency verifies the
// default-deny egress consistency rule (ADR-0052, setec#66): a class's
// defaultNetworkMode, when set alongside a restricted allowedNetworkModes
// list, must itself be an allowed mode.
func TestSandboxClassWebhook_DefaultNetworkModeConsistency(t *testing.T) {
	w := webhookWith(fakeClientWithNS(t), baseConfig())

	t.Run("default outside allowed set is rejected", func(t *testing.T) {
		cls := mkSandboxClass("bad", setecv1alpha1.VMMFirecracker, mkRuntime("kata-fc"))
		cls.Spec.AllowedNetworkModes = []setecv1alpha1.NetworkMode{setecv1alpha1.NetworkModeNone}
		cls.Spec.DefaultNetworkMode = setecv1alpha1.NetworkModeEgressAllowList
		_, err := w.ValidateCreate(context.Background(), cls)
		if err == nil || !strings.Contains(err.Error(), "defaultNetworkMode") {
			t.Fatalf("expected defaultNetworkMode consistency error, got %v", err)
		}
	})

	t.Run("default within allowed set passes", func(t *testing.T) {
		cls := mkSandboxClass("good", setecv1alpha1.VMMFirecracker, mkRuntime("kata-fc"))
		cls.Spec.AllowedNetworkModes = []setecv1alpha1.NetworkMode{
			setecv1alpha1.NetworkModeNone, setecv1alpha1.NetworkModeEgressAllowList,
		}
		cls.Spec.DefaultNetworkMode = setecv1alpha1.NetworkModeNone
		if _, err := w.ValidateCreate(context.Background(), cls); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("default-deny with no allowed-list restriction passes", func(t *testing.T) {
		cls := mkSandboxClass("open", setecv1alpha1.VMMFirecracker, mkRuntime("kata-fc"))
		cls.Spec.DefaultNetworkMode = setecv1alpha1.NetworkModeNone
		if _, err := w.ValidateCreate(context.Background(), cls); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("consistency holds even when Runtime is nil", func(t *testing.T) {
		cls := mkSandboxClass("noruntime", setecv1alpha1.VMMFirecracker, nil)
		cls.Spec.AllowedNetworkModes = []setecv1alpha1.NetworkMode{setecv1alpha1.NetworkModeNone}
		cls.Spec.DefaultNetworkMode = setecv1alpha1.NetworkModeEgressAllowList
		_, err := w.ValidateCreate(context.Background(), cls)
		if err == nil || !strings.Contains(err.Error(), "defaultNetworkMode") {
			t.Fatalf("expected consistency error with nil Runtime, got %v", err)
		}
	})
}
