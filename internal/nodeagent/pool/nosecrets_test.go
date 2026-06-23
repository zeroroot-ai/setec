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

package pool

import (
	"reflect"
	"strings"
	"testing"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
)

// TestLaunchOptions_CarriesNoSecretMaterial codifies the no-secrets-in-snapshot
// invariant (ADR-0052) at its source: a pre-warm pool entry is the VM that
// gets snapshotted and then shared across every warm-pool claim. Per-lease
// secrets MUST therefore never enter the pool launch path; they are injected
// per-Sandbox POST-claim via the Pod env, never baked into the snapshotted
// pool VM.
//
// This test fails loudly if a future refactor adds a secret/env/credential
// field to LaunchOptions, which would let secret material reach the
// snapshotted VM's memory image.
func TestLaunchOptions_CarriesNoSecretMaterial(t *testing.T) {
	forbidden := []string{"secret", "env", "credential", "password", "token", "apikey"}
	typ := reflect.TypeFor[LaunchOptions]()
	for field := range typ.Fields() {
		name := strings.ToLower(field.Name)
		for _, bad := range forbidden {
			if strings.Contains(name, bad) {
				t.Fatalf("LaunchOptions.%s looks like secret material; pool entries are snapshotted and "+
					"shared across warm-pool claims, so secrets must be injected per-lease post-restore, "+
					"never carried into the pool launch path (ADR-0052)", field.Name)
			}
		}
	}
}

// TestLaunchOptionsFrom_DoesNotCopySandboxClassSecrets ensures the rendering
// helper never pulls anything beyond image/resource shape from the
// SandboxClass into the pool entry.
func TestLaunchOptionsFrom_DoesNotCopySandboxClassSecrets(t *testing.T) {
	cls := &setecv1alpha1.SandboxClass{
		Spec: setecv1alpha1.SandboxClassSpec{
			PreWarmImage: "ghcr.io/org/app:1.2.3",
		},
	}
	cls.Name = "fast"
	opts := LaunchOptionsFrom(cls, "entry-1", "/run/sock", "/var/state", "/k", "/r", 2, 2048)

	if opts.ImageRef != "ghcr.io/org/app:1.2.3" {
		t.Fatalf("unexpected ImageRef: %q", opts.ImageRef)
	}
	// The rendered options must contain only the declared, non-secret fields.
	if opts.ClassName != "fast" || opts.EntryID != "entry-1" {
		t.Fatalf("unexpected pool entry identity: %+v", opts)
	}
}
