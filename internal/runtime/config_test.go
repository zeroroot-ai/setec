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
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// minimalValid returns a RuntimeConfig that passes Validate with only kata-fc
// enabled.
func minimalValid() *RuntimeConfig {
	return &RuntimeConfig{
		Runtimes: map[string]BackendConfig{
			BackendKataFC: {Enabled: true, RuntimeClassName: "kata-fc"},
		},
		Defaults: DefaultsConfig{
			Runtime: RuntimeDefaults{
				Backend: BackendKataFC,
			},
		},
	}
}

func TestValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		cfg         *RuntimeConfig
		wantErr     bool
		errContains string
	}{
		{
			name:        "empty config — no runtimes map",
			cfg:         &RuntimeConfig{},
			wantErr:     true,
			errContains: "at least one runtime",
		},
		{
			name: "all runtimes disabled",
			cfg: &RuntimeConfig{
				Runtimes: map[string]BackendConfig{
					BackendKataFC:   {Enabled: false},
					BackendKataQEMU: {Enabled: false},
					BackendGVisor:   {Enabled: false},
					BackendRunc:     {Enabled: false},
				},
				Defaults: DefaultsConfig{
					Runtime: RuntimeDefaults{Backend: BackendKataFC},
				},
			},
			wantErr:     true,
			errContains: "at least one runtime",
		},
		{
			name: "missing default backend (empty string)",
			cfg: &RuntimeConfig{
				Runtimes: map[string]BackendConfig{
					BackendKataFC: {Enabled: true, RuntimeClassName: "kata-fc"},
				},
				Defaults: DefaultsConfig{
					Runtime: RuntimeDefaults{Backend: ""},
				},
			},
			wantErr:     true,
			errContains: "defaults.runtime.backend",
		},
		{
			name: "default backend not in enabled set",
			cfg: &RuntimeConfig{
				Runtimes: map[string]BackendConfig{
					BackendKataFC: {Enabled: true, RuntimeClassName: "kata-fc"},
				},
				Defaults: DefaultsConfig{
					Runtime: RuntimeDefaults{Backend: BackendGVisor},
				},
			},
			wantErr:     true,
			errContains: "defaults.runtime.backend",
		},
		{
			name: "fallback entry disabled",
			cfg: &RuntimeConfig{
				Runtimes: map[string]BackendConfig{
					BackendKataFC: {Enabled: true, RuntimeClassName: "kata-fc"},
					BackendGVisor: {Enabled: false},
				},
				Defaults: DefaultsConfig{
					Runtime: RuntimeDefaults{
						Backend:  BackendKataFC,
						Fallback: []string{BackendGVisor},
					},
				},
			},
			wantErr:     true,
			errContains: "defaults.runtime.fallback[0]",
		},
		{
			name: "fallback entry unknown backend",
			cfg: &RuntimeConfig{
				Runtimes: map[string]BackendConfig{
					BackendKataFC: {Enabled: true, RuntimeClassName: "kata-fc"},
				},
				Defaults: DefaultsConfig{
					Runtime: RuntimeDefaults{
						Backend:  BackendKataFC,
						Fallback: []string{"unknown-backend"},
					},
				},
			},
			wantErr:     true,
			errContains: "defaults.runtime.fallback[0]",
		},
		{
			name: "invalid nodeCapabilitiesMode",
			cfg: &RuntimeConfig{
				Runtimes: map[string]BackendConfig{
					BackendKataFC: {Enabled: true, RuntimeClassName: "kata-fc"},
				},
				Defaults: DefaultsConfig{
					Runtime: RuntimeDefaults{
						Backend:              BackendKataFC,
						NodeCapabilitiesMode: "auto",
					},
				},
			},
			wantErr:     true,
			errContains: "defaults.runtime.nodeCapabilitiesMode",
		},
		{
			name:    "valid minimal config — only kata-fc enabled",
			cfg:     minimalValid(),
			wantErr: false,
		},
		{
			name: "valid config — multiple enabled backends with fallback",
			cfg: &RuntimeConfig{
				Runtimes: map[string]BackendConfig{
					BackendKataFC: {Enabled: true, RuntimeClassName: "kata-fc"},
					BackendGVisor: {Enabled: true, RuntimeClassName: "gvisor"},
				},
				Defaults: DefaultsConfig{
					Runtime: RuntimeDefaults{
						Backend:  BackendKataFC,
						Fallback: []string{BackendGVisor},
					},
				},
			},
			wantErr: false,
		},
		{
			name: "invalid config — nodeCapabilitiesMode=static is rejected (mode removed)",
			cfg: &RuntimeConfig{
				Runtimes: map[string]BackendConfig{
					BackendKataFC: {Enabled: true, RuntimeClassName: "kata-fc"},
				},
				Defaults: DefaultsConfig{
					Runtime: RuntimeDefaults{
						Backend:              BackendKataFC,
						NodeCapabilitiesMode: "static",
					},
				},
			},
			wantErr: true,
		},
		{
			name: "valid config — nodeCapabilitiesMode=probe explicit",
			cfg: &RuntimeConfig{
				Runtimes: map[string]BackendConfig{
					BackendKataFC: {Enabled: true, RuntimeClassName: "kata-fc"},
				},
				Defaults: DefaultsConfig{
					Runtime: RuntimeDefaults{
						Backend:              BackendKataFC,
						NodeCapabilitiesMode: "probe",
					},
				},
			},
			wantErr: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.cfg.Validate()
			if tc.wantErr && err == nil {
				t.Fatal("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected no error, got: %v", err)
			}
			if tc.wantErr && tc.errContains != "" {
				if !strings.Contains(err.Error(), tc.errContains) {
					t.Fatalf("error %q does not contain expected substring %q", err.Error(), tc.errContains)
				}
			}
		})
	}
}

func TestEnabledBackends(t *testing.T) {
	t.Parallel()

	cfg := &RuntimeConfig{
		Runtimes: map[string]BackendConfig{
			BackendKataFC:   {Enabled: true},
			BackendKataQEMU: {Enabled: false},
			BackendGVisor:   {Enabled: true},
			BackendRunc:     {Enabled: false},
		},
	}

	got := cfg.EnabledBackends()
	want := []string{BackendGVisor, BackendKataFC} // alphabetical

	if len(got) != len(want) {
		t.Fatalf("EnabledBackends() = %v, want %v", got, want)
	}
	for i, v := range want {
		if got[i] != v {
			t.Errorf("EnabledBackends()[%d] = %q, want %q", i, got[i], v)
		}
	}
}

func TestLoadFromFile_HappyPath(t *testing.T) {
	t.Parallel()

	yaml := `
runtimes:
  kata-fc:
    enabled: true
    runtimeClassName: kata-fc
  gvisor:
    enabled: false
    runtimeClassName: gvisor
defaults:
  runtime:
    backend: kata-fc
    nodeCapabilitiesMode: probe
`
	path := writeTemp(t, yaml)
	cfg, err := LoadFromFile(path)
	if err != nil {
		t.Fatalf("LoadFromFile() unexpected error: %v", err)
	}
	if !cfg.Runtimes[BackendKataFC].Enabled {
		t.Error("expected kata-fc enabled=true")
	}
	if cfg.Runtimes[BackendGVisor].Enabled {
		t.Error("expected gvisor enabled=false")
	}
	if cfg.Defaults.Runtime.Backend != BackendKataFC {
		t.Errorf("defaults.runtime.backend = %q, want %q", cfg.Defaults.Runtime.Backend, BackendKataFC)
	}
}

func TestLoadFromFile_MalformedYAML(t *testing.T) {
	t.Parallel()

	path := writeTemp(t, "runtimes: [this is not a map")
	_, err := LoadFromFile(path)
	if err == nil {
		t.Fatal("expected error for malformed YAML, got nil")
	}
	if !strings.Contains(err.Error(), "parsing runtimes config") {
		t.Errorf("error message %q does not mention parsing", err.Error())
	}
}

func TestLoadFromFile_NotFound(t *testing.T) {
	t.Parallel()

	_, err := LoadFromFile("/nonexistent/path/to/runtimes.yaml")
	if err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
	if !strings.Contains(err.Error(), "reading runtimes config") {
		t.Errorf("error message %q does not mention reading", err.Error())
	}
}

func TestLoadFromFile_ValidationError(t *testing.T) {
	t.Parallel()

	// Valid YAML but semantically invalid: all backends disabled.
	yaml := `
runtimes:
  kata-fc:
    enabled: false
    runtimeClassName: kata-fc
defaults:
  runtime:
    backend: kata-fc
`
	path := writeTemp(t, yaml)
	_, err := LoadFromFile(path)
	if err == nil {
		t.Fatal("expected validation error, got nil")
	}
}

// writeTemp writes content to a temp file and returns its path.  It registers
// a cleanup function to remove the file when the test ends.
func writeTemp(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "runtimes.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing temp file: %v", err)
	}
	return path
}
