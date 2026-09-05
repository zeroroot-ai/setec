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
