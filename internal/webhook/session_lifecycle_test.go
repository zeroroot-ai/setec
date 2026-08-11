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

	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/controller-runtime/pkg/client"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
	"github.com/zeroroot-ai/setec/internal/class"
)

// lifecycleValidator returns a validator wired against a default class so
// the class-resolution branch never interferes with lifecycle assertions.
func lifecycleValidator(t *testing.T) *SandboxValidator {
	t.Helper()
	seed := []client.Object{mkClass("standard", true, "8Gi")}
	return &SandboxValidator{Resolver: class.NewResolver(newFakeClient(t, seed...))}
}

// withMode stamps a lifecycle mode onto a valid Sandbox.
func withMode(sb *setecv1alpha1.Sandbox, mode setecv1alpha1.LifecycleMode) *setecv1alpha1.Sandbox {
	out := sb.DeepCopy()
	if out.Spec.Lifecycle == nil {
		out.Spec.Lifecycle = &setecv1alpha1.Lifecycle{}
	}
	out.Spec.Lifecycle.Mode = mode
	return out
}

// TestValidateUpdate_LifecycleModeImmutable asserts every effective mode
// change is rejected while no-op re-spellings are admitted.
func TestValidateUpdate_LifecycleModeImmutable(t *testing.T) {
	t.Parallel()
	v := lifecycleValidator(t)
	base := mkSandbox("standard", 2, "2Gi", "")

	cases := []struct {
		name     string
		oldSB    *setecv1alpha1.Sandbox
		newSB    *setecv1alpha1.Sandbox
		wantDeny bool
	}{
		{"implicit ephemeral to session denied", base, withMode(base, setecv1alpha1.LifecycleModeSession), true},
		{"explicit ephemeral to session denied", withMode(base, setecv1alpha1.LifecycleModeEphemeral), withMode(base, setecv1alpha1.LifecycleModeSession), true},
		{"session to ephemeral denied", withMode(base, setecv1alpha1.LifecycleModeSession), withMode(base, setecv1alpha1.LifecycleModeEphemeral), true},
		{"session to implicit ephemeral denied", withMode(base, setecv1alpha1.LifecycleModeSession), base, true},
		{"implicit to explicit ephemeral allowed", base, withMode(base, setecv1alpha1.LifecycleModeEphemeral), false},
		{"session stays session allowed", withMode(base, setecv1alpha1.LifecycleModeSession), withMode(base, setecv1alpha1.LifecycleModeSession), false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := v.ValidateUpdate(context.Background(), tc.oldSB, tc.newSB)
			if tc.wantDeny {
				if err == nil {
					t.Fatalf("expected lifecycle-mode mutation to be rejected, got nil error")
				}
				if !strings.Contains(err.Error(), "immutable") {
					t.Fatalf("rejection should name immutability, got: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected rejection: %v", err)
			}
		})
	}
}

// TestValidateCreate_Workspace asserts the structural workspace rules:
// a workspace block requires session mode, and a declared size must be
// positive.
func TestValidateCreate_Workspace(t *testing.T) {
	t.Parallel()
	v := lifecycleValidator(t)

	mkWithWorkspace := func(mode setecv1alpha1.LifecycleMode, size string) *setecv1alpha1.Sandbox {
		sb := mkSandbox("standard", 2, "2Gi", "")
		lc := &setecv1alpha1.Lifecycle{Mode: mode, Workspace: &setecv1alpha1.WorkspaceSpec{}}
		if size != "" {
			q := resource.MustParse(size)
			lc.Workspace.Size = &q
		}
		sb.Spec.Lifecycle = lc
		return sb
	}

	cases := []struct {
		name     string
		sb       *setecv1alpha1.Sandbox
		wantDeny bool
	}{
		{"workspace on ephemeral denied", mkWithWorkspace(setecv1alpha1.LifecycleModeEphemeral, "10Gi"), true},
		{"workspace with no mode denied", mkWithWorkspace("", "10Gi"), true},
		{"zero size denied", mkWithWorkspace(setecv1alpha1.LifecycleModeSession, "0"), true},
		{"session workspace admitted", mkWithWorkspace(setecv1alpha1.LifecycleModeSession, "20Gi"), false},
		{"session workspace default size admitted", mkWithWorkspace(setecv1alpha1.LifecycleModeSession, ""), false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := v.ValidateCreate(context.Background(), tc.sb)
			if tc.wantDeny && err == nil {
				t.Fatalf("expected rejection, got nil error")
			}
			if !tc.wantDeny && err != nil {
				t.Fatalf("unexpected rejection: %v", err)
			}
		})
	}
}
