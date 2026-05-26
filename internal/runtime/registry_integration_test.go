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

package runtime

import (
	"errors"
	"testing"

	"github.com/zeroroot-ai/setec/api/v1alpha1"
)

// buildAllBackendsRegistry returns a Registry pre-populated with all four real
// Dispatcher implementations, each with a minimal BackendConfig.
func buildAllBackendsRegistry() *Registry {
	r := NewRegistry()
	r.Register(NewKataFCDispatcher(BackendConfig{RuntimeClassName: BackendKataFC}))
	r.Register(NewKataQEMUDispatcher(BackendConfig{RuntimeClassName: BackendKataQEMU}))
	r.Register(NewGVisorDispatcher(BackendConfig{RuntimeClassName: BackendGVisor}))
	r.Register(NewRuncDispatcher(BackendConfig{RuntimeClassName: BackendRunc}))
	return r
}

func TestRegistry_EnabledBackends_AllFour(t *testing.T) {
	t.Parallel()
	r := buildAllBackendsRegistry()
	got := r.EnabledBackends()
	// AllKnownBackends is already sorted alphabetically.
	if len(got) != len(AllKnownBackends) {
		t.Fatalf("EnabledBackends() = %v, want %v", got, AllKnownBackends)
	}
	for i, want := range AllKnownBackends {
		if got[i] != want {
			t.Errorf("[%d] = %q, want %q", i, got[i], want)
		}
	}
}

func TestRegistry_Select_RealDispatchers(t *testing.T) {
	t.Parallel()

	allCaps := AllKnownBackends // every node claims all backends

	tests := []struct {
		name            string
		class           *v1alpha1.SandboxClass
		cfg             *RuntimeConfig
		caps            []string
		wantBackend     string
		wantFellBack    bool
		wantFromBackend string
		wantErr         bool
		wantErrIs       error
	}{
		{
			name:        "kata-fc selected directly",
			class:       sandboxClassWith(BackendKataFC),
			cfg:         cfgWithDefaults(BackendKataFC),
			caps:        allCaps,
			wantBackend: BackendKataFC,
		},
		{
			name:        "kata-qemu selected directly",
			class:       sandboxClassWith(BackendKataQEMU),
			cfg:         cfgWithDefaults(BackendKataQEMU),
			caps:        allCaps,
			wantBackend: BackendKataQEMU,
		},
		{
			name:        "gvisor selected directly",
			class:       sandboxClassWith(BackendGVisor),
			cfg:         cfgWithDefaults(BackendGVisor),
			caps:        allCaps,
			wantBackend: BackendGVisor,
		},
		{
			name:        "runc selected directly",
			class:       sandboxClassWith(BackendRunc),
			cfg:         cfgWithDefaults(BackendRunc),
			caps:        allCaps,
			wantBackend: BackendRunc,
		},
		{
			// kata-fc not in node capabilities; fallback chain: kata-qemu → gvisor.
			name:            "kata-fc unavailable — falls back to kata-qemu",
			class:           sandboxClassWith(BackendKataFC, BackendKataQEMU, BackendGVisor),
			cfg:             cfgWithDefaults(BackendKataFC),
			caps:            []string{BackendKataQEMU, BackendGVisor, BackendRunc},
			wantBackend:     BackendKataQEMU,
			wantFellBack:    true,
			wantFromBackend: BackendKataFC,
		},
		{
			// Both kata backends absent from node; gvisor is last fallback.
			name:            "kata-fc and kata-qemu unavailable — falls back to gvisor",
			class:           sandboxClassWith(BackendKataFC, BackendKataQEMU, BackendGVisor),
			cfg:             cfgWithDefaults(BackendKataFC),
			caps:            []string{BackendGVisor, BackendRunc},
			wantBackend:     BackendGVisor,
			wantFellBack:    true,
			wantFromBackend: BackendKataFC,
		},
		{
			// Entire fallback chain missing from node capabilities.
			name:      "full fallback chain exhausted — ErrNoEligibleRuntime",
			class:     sandboxClassWith(BackendKataFC, BackendKataQEMU),
			cfg:       cfgWithDefaults(BackendKataFC),
			caps:      []string{BackendRunc}, // neither kata variant present
			wantErr:   true,
			wantErrIs: ErrNoEligibleRuntime,
		},
		{
			// cfg default used when class is nil; fallback via cfg.
			name:            "nil class — cfg default with fallback",
			class:           nil,
			cfg:             cfgWithDefaults(BackendKataFC, BackendGVisor),
			caps:            []string{BackendGVisor},
			wantBackend:     BackendGVisor,
			wantFellBack:    true,
			wantFromBackend: BackendKataFC,
		},
	}

	r := buildAllBackendsRegistry()

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			sel, err := r.Select(tc.class, tc.cfg, tc.caps)

			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				if tc.wantErrIs != nil && !errors.Is(err, tc.wantErrIs) {
					t.Fatalf("error %v does not wrap %v", err, tc.wantErrIs)
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if sel == nil {
				t.Fatal("Select returned nil Selection without error")
			}
			if sel.Backend != tc.wantBackend {
				t.Errorf("Backend = %q, want %q", sel.Backend, tc.wantBackend)
			}
			if sel.FellBack != tc.wantFellBack {
				t.Errorf("FellBack = %v, want %v", sel.FellBack, tc.wantFellBack)
			}
			if sel.FromBackend != tc.wantFromBackend {
				t.Errorf("FromBackend = %q, want %q", sel.FromBackend, tc.wantFromBackend)
			}
			// Verify the returned Dispatcher is the real implementation.
			if sel.Dispatcher == nil {
				t.Fatal("Dispatcher is nil")
			}
			if sel.Dispatcher.Name() != tc.wantBackend {
				t.Errorf("Dispatcher.Name() = %q, want %q", sel.Dispatcher.Name(), tc.wantBackend)
			}
			// RuntimeClassName must match what was registered.
			if sel.Dispatcher.RuntimeClassName() != tc.wantBackend {
				t.Errorf("Dispatcher.RuntimeClassName() = %q, want %q",
					sel.Dispatcher.RuntimeClassName(), tc.wantBackend)
			}
			// NodeAffinity must be non-nil for all real backends.
			if sel.Dispatcher.NodeAffinity() == nil {
				t.Errorf("Dispatcher.NodeAffinity() is nil for backend %q", tc.wantBackend)
			}
		})
	}
}
