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

	corev1 "k8s.io/api/core/v1"

	"github.com/zeroroot-ai/setec/api/v1alpha1"
)

// fakeDispatcher is a minimal Dispatcher implementation for unit testing.
type fakeDispatcher struct {
	name string
}

func (f *fakeDispatcher) Name() string                                       { return f.name }
func (f *fakeDispatcher) RuntimeClassName() string                           { return f.name }
func (f *fakeDispatcher) NodeAffinity() *corev1.NodeAffinity                 { return nil }
func (f *fakeDispatcher) Overhead() corev1.ResourceList                      { return nil }
func (f *fakeDispatcher) MutatePod(_ *corev1.Pod, _ map[string]string) error { return nil }

// newTestRegistry builds a Registry pre-populated with fakeDispatchers for
// each name in names.
func newTestRegistry(names ...string) *Registry {
	r := NewRegistry()
	for _, n := range names {
		r.Register(&fakeDispatcher{name: n})
	}
	return r
}

// cfgWithDefaults returns a RuntimeConfig whose defaults select the given
// primary backend and optional fallback chain.  All named backends are marked
// enabled so Validate would pass, but the registry is separate.
func cfgWithDefaults(primary string, fallback ...string) *RuntimeConfig {
	cfg := &RuntimeConfig{
		Runtimes: map[string]BackendConfig{},
		Defaults: DefaultsConfig{
			Runtime: RuntimeDefaults{
				Backend:  primary,
				Fallback: fallback,
			},
		},
	}
	cfg.Runtimes[primary] = BackendConfig{Enabled: true}
	for _, fb := range fallback {
		cfg.Runtimes[fb] = BackendConfig{Enabled: true}
	}
	return cfg
}

// sandboxClassWith returns a SandboxClass whose Runtime.Backend is set to
// backend and Runtime.Fallback to fallback.
func sandboxClassWith(backend string, fallback ...string) *v1alpha1.SandboxClass {
	return &v1alpha1.SandboxClass{
		Spec: v1alpha1.SandboxClassSpec{
			Runtime: &v1alpha1.SandboxClassRuntime{
				Backend:  backend,
				Fallback: fallback,
			},
		},
	}
}

func TestRegistry_Select(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		registeredNames  []string // dispatchers in registry
		class            *v1alpha1.SandboxClass
		cfg              *RuntimeConfig
		nodeCapabilities []string
		wantBackend      string
		wantFellBack     bool
		wantFromBackend  string
		wantErr          bool
		wantErrIs        error
	}{
		{
			name:             "default backend selected — class is nil",
			registeredNames:  []string{BackendKataFC},
			class:            nil,
			cfg:              cfgWithDefaults(BackendKataFC),
			nodeCapabilities: []string{BackendKataFC},
			wantBackend:      BackendKataFC,
			wantFellBack:     false,
		},
		{
			name:             "explicit backend from SandboxClass",
			registeredNames:  []string{BackendKataFC, BackendGVisor},
			class:            sandboxClassWith(BackendGVisor),
			cfg:              cfgWithDefaults(BackendKataFC),
			nodeCapabilities: []string{BackendKataFC, BackendGVisor},
			wantBackend:      BackendGVisor,
			wantFellBack:     false,
		},
		{
			name:             "class runtime nil — falls through to cfg default",
			registeredNames:  []string{BackendKataFC},
			class:            &v1alpha1.SandboxClass{Spec: v1alpha1.SandboxClassSpec{}},
			cfg:              cfgWithDefaults(BackendKataFC),
			nodeCapabilities: []string{BackendKataFC},
			wantBackend:      BackendKataFC,
			wantFellBack:     false,
		},
		{
			name:             "primary unavailable — first fallback selected",
			registeredNames:  []string{BackendKataFC, BackendGVisor, BackendRunc},
			class:            sandboxClassWith(BackendKataFC, BackendGVisor, BackendRunc),
			cfg:              cfgWithDefaults(BackendKataFC),
			nodeCapabilities: []string{BackendGVisor, BackendRunc},
			wantBackend:      BackendGVisor,
			wantFellBack:     true,
			wantFromBackend:  BackendKataFC,
		},
		{
			name:             "primary and first fallback unavailable — second fallback selected",
			registeredNames:  []string{BackendKataFC, BackendGVisor, BackendRunc},
			class:            sandboxClassWith(BackendKataFC, BackendGVisor, BackendRunc),
			cfg:              cfgWithDefaults(BackendKataFC),
			nodeCapabilities: []string{BackendRunc},
			wantBackend:      BackendRunc,
			wantFellBack:     true,
			wantFromBackend:  BackendKataFC,
		},
		{
			name:             "fallback chain exhausted — ErrNoEligibleRuntime",
			registeredNames:  []string{BackendKataFC, BackendGVisor},
			class:            sandboxClassWith(BackendKataFC, BackendGVisor),
			cfg:              cfgWithDefaults(BackendKataFC),
			nodeCapabilities: []string{BackendRunc}, // none of the above
			wantErr:          true,
			wantErrIs:        ErrNoEligibleRuntime,
		},
		{
			name:             "primary registered but node lacks capability — fallback used",
			registeredNames:  []string{BackendKataFC, BackendKataQEMU},
			class:            sandboxClassWith(BackendKataFC, BackendKataQEMU),
			cfg:              cfgWithDefaults(BackendKataFC),
			nodeCapabilities: []string{BackendKataQEMU},
			wantBackend:      BackendKataQEMU,
			wantFellBack:     true,
			wantFromBackend:  BackendKataFC,
		},
		{
			name:             "primary not registered — fallback registered and capable",
			registeredNames:  []string{BackendGVisor}, // kata-fc dispatcher absent
			class:            sandboxClassWith(BackendKataFC, BackendGVisor),
			cfg:              cfgWithDefaults(BackendKataFC),
			nodeCapabilities: []string{BackendKataFC, BackendGVisor},
			wantBackend:      BackendGVisor,
			wantFellBack:     true,
			wantFromBackend:  BackendKataFC,
		},
		{
			name:             "empty node capabilities — no eligible runtime",
			registeredNames:  []string{BackendKataFC},
			class:            sandboxClassWith(BackendKataFC),
			cfg:              cfgWithDefaults(BackendKataFC),
			nodeCapabilities: []string{},
			wantErr:          true,
			wantErrIs:        ErrNoEligibleRuntime,
		},
		{
			name:             "nil node capabilities — no eligible runtime",
			registeredNames:  []string{BackendKataFC},
			class:            sandboxClassWith(BackendKataFC),
			cfg:              cfgWithDefaults(BackendKataFC),
			nodeCapabilities: nil,
			wantErr:          true,
			wantErrIs:        ErrNoEligibleRuntime,
		},
		{
			name:             "cfg fallback used when class is nil",
			registeredNames:  []string{BackendKataFC, BackendGVisor},
			class:            nil,
			cfg:              cfgWithDefaults(BackendKataFC, BackendGVisor),
			nodeCapabilities: []string{BackendGVisor},
			wantBackend:      BackendGVisor,
			wantFellBack:     true,
			wantFromBackend:  BackendKataFC,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			reg := newTestRegistry(tc.registeredNames...)
			got, err := reg.Select(tc.class, tc.cfg, tc.nodeCapabilities)

			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				if tc.wantErrIs != nil && !errors.Is(err, tc.wantErrIs) {
					t.Fatalf("error %v does not wrap expected sentinel %v", err, tc.wantErrIs)
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got == nil {
				t.Fatal("Select returned nil Selection without error")
			}
			if got.Backend != tc.wantBackend {
				t.Errorf("Backend = %q, want %q", got.Backend, tc.wantBackend)
			}
			if got.FellBack != tc.wantFellBack {
				t.Errorf("FellBack = %v, want %v", got.FellBack, tc.wantFellBack)
			}
			if got.FromBackend != tc.wantFromBackend {
				t.Errorf("FromBackend = %q, want %q", got.FromBackend, tc.wantFromBackend)
			}
			if got.Dispatcher == nil {
				t.Error("Dispatcher is nil")
			} else if got.Dispatcher.Name() != tc.wantBackend {
				t.Errorf("Dispatcher.Name() = %q, want %q", got.Dispatcher.Name(), tc.wantBackend)
			}
		})
	}
}

func TestRegistry_EnabledBackends(t *testing.T) {
	t.Parallel()

	r := newTestRegistry(BackendRunc, BackendKataFC, BackendGVisor)
	got := r.EnabledBackends()
	want := []string{BackendGVisor, BackendKataFC, BackendRunc} // alphabetical

	if len(got) != len(want) {
		t.Fatalf("EnabledBackends() = %v, want %v", got, want)
	}
	for i, v := range want {
		if got[i] != v {
			t.Errorf("[%d] = %q, want %q", i, got[i], v)
		}
	}
}

func TestRegistry_SelectDoesNotMutateInputs(t *testing.T) {
	t.Parallel()

	originalFallback := []string{BackendGVisor}
	class := sandboxClassWith(BackendKataFC, originalFallback...)
	cfg := cfgWithDefaults(BackendKataFC, BackendGVisor)
	caps := []string{BackendGVisor}

	reg := newTestRegistry(BackendKataFC, BackendGVisor)
	_, err := reg.Select(class, cfg, caps)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify inputs unchanged.
	if class.Spec.Runtime.Fallback[0] != BackendGVisor {
		t.Error("class.Spec.Runtime.Fallback was mutated")
	}
	if cfg.Defaults.Runtime.Backend != BackendKataFC {
		t.Error("cfg.Defaults.Runtime.Backend was mutated")
	}
	if caps[0] != BackendGVisor {
		t.Error("nodeCapabilities slice was mutated")
	}
}
