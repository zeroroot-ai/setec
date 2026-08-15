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

package probe

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stockConfigRelPath is where the scan expects a stock containerd node's
// rendered config, relative to the probe FSRoot.
const stockConfigRelPath = "etc/containerd/config.toml"

// writeFile writes content at rel under root, creating parent directories.
func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	full := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir %q: %v", filepath.Dir(full), err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatalf("write %q: %v", full, err)
	}
}

// containerdConfigWith renders a minimal containerd v2-schema config that
// registers the named CRI runtime handlers.
func containerdConfigWith(handlers ...string) string {
	var b strings.Builder
	b.WriteString("version = 2\n\n")
	for _, h := range handlers {
		b.WriteString("[plugins.\"io.containerd.grpc.v1.cri\".containerd.runtimes." + h + "]\n")
		b.WriteString("  runtime_type = \"io.containerd." + h + ".v2\"\n")
		b.WriteString("[plugins.\"io.containerd.grpc.v1.cri\".containerd.runtimes." + h + ".options]\n")
		b.WriteString("  ConfigPath = \"/opt/kata/share/defaults/kata-containers/configuration.toml\"\n\n")
	}
	return b.String()
}

// writeStockContainerdConfig lays down /etc/containerd/config.toml under root
// registering the given handlers.
func writeStockContainerdConfig(t *testing.T, root string, handlers ...string) {
	t.Helper()
	writeFile(t, root, stockConfigRelPath, containerdConfigWith(handlers...))
}

// TestConfiguredRuntimeHandlers covers the containerd config schemas the scan
// has to understand and the files it must decline to read meaning into.
func TestConfiguredRuntimeHandlers(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		files       map[string]string // relative path -> content
		wantHandler []string          // handlers that must be present
		wantAbsent  []string          // handlers that must NOT be present
		wantScanned int
	}{
		{
			name: "stock v2 config with kata handlers",
			files: map[string]string{
				stockConfigRelPath: containerdConfigWith("runc", "kata-fc", "kata-qemu"),
			},
			wantHandler: []string{"runc", "kata-fc", "kata-qemu"},
			wantScanned: 1,
		},
		{
			name: "v3 schema, single-quoted plugin name, quoted handler",
			files: map[string]string{
				stockConfigRelPath: "version = 3\n" +
					"[plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.\"kata-qemu\"]\n" +
					"  runtime_type = 'io.containerd.kata-qemu.v2'\n",
			},
			wantHandler: []string{"kata-qemu"},
			wantAbsent:  []string{"kata-fc"},
			wantScanned: 1,
		},
		{
			name: "drop-in under config.d is scanned alongside the main config",
			files: map[string]string{
				stockConfigRelPath: "version = 2\nimports = [\"/etc/containerd/config.d/*.toml\"]\n",
				"etc/containerd/config.d/99-setec-kata-fc.toml": containerdConfigWith("kata-fc"),
			},
			wantHandler: []string{"kata-fc"},
			wantScanned: 2,
		},
		{
			name: "k3s config template is scanned",
			files: map[string]string{
				"var/lib/rancher/k3s/agent/etc/containerd/config.toml.tmpl": containerdConfigWith("kata-qemu"),
			},
			wantHandler: []string{"kata-qemu"},
			wantScanned: 1,
		},
		{
			name: "k3s config.toml.d drop-in is scanned",
			files: map[string]string{
				"var/lib/rancher/k3s/agent/etc/containerd/config.toml":                    "version = 2\n",
				"var/lib/rancher/k3s/agent/etc/containerd/config.toml.d/kata-deploy.toml": containerdConfigWith("kata-fc"),
			},
			wantHandler: []string{"kata-fc"},
			wantScanned: 2,
		},
		{
			name: "a comment mentioning the table is not a registration",
			files: map[string]string{
				stockConfigRelPath: "version = 2\n" +
					"# add [plugins.\"io.containerd.grpc.v1.cri\".containerd.runtimes.kata-fc] to enable kata\n",
			},
			wantAbsent:  []string{"kata-fc"},
			wantScanned: 1,
		},
		{
			name: "non-toml files in the config dir are ignored",
			files: map[string]string{
				"etc/containerd/certs.d/README": containerdConfigWith("kata-fc"),
				"etc/containerd/notes.txt":      containerdConfigWith("kata-qemu"),
			},
			wantAbsent:  []string{"kata-fc", "kata-qemu"},
			wantScanned: 0,
		},
		{
			name:        "no containerd config at all",
			files:       nil,
			wantAbsent:  []string{"kata-fc", "kata-qemu", "runc"},
			wantScanned: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			root := t.TempDir()
			for rel, content := range tc.files {
				writeFile(t, root, rel, content)
			}

			handlers, scanned := ConfiguredRuntimeHandlers(root)

			if len(scanned) != tc.wantScanned {
				t.Errorf("scanned %d file(s) (%v), want %d", len(scanned), scanned, tc.wantScanned)
			}
			for _, h := range tc.wantHandler {
				if _, ok := handlers[h]; !ok {
					t.Errorf("handler %q missing; got %v", h, handlers)
				}
			}
			for _, h := range tc.wantAbsent {
				if _, ok := handlers[h]; ok {
					t.Errorf("handler %q unexpectedly present; got %v", h, handlers)
				}
			}
		})
	}
}

// TestCheckContainerdHandler pins the three outcomes the kata probes key on,
// with "cannot read the config" kept distinct from "the handler is not there"
// while both deny the capability.
func TestCheckContainerdHandler(t *testing.T) {
	t.Parallel()

	t.Run("handler registered", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		writeStockContainerdConfig(t, root, "runc", "kata-qemu")

		got := checkContainerdHandler(root, "kata-qemu")
		if !got.Configured || got.State != handlerConfigured {
			t.Fatalf("got %+v, want Configured=true state=%q", got, handlerConfigured)
		}
	})

	t.Run("config readable but handler absent", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		writeStockContainerdConfig(t, root, "runc")

		got := checkContainerdHandler(root, "kata-qemu")
		if got.Configured {
			t.Fatalf("got Configured=true, want false: %+v", got)
		}
		if got.State != handlerAbsent {
			t.Errorf("State = %q, want %q", got.State, handlerAbsent)
		}
		if !strings.Contains(got.Reason, `no runtime handler "kata-qemu"`) {
			t.Errorf("Reason = %q, want it to name the missing handler", got.Reason)
		}
	})

	t.Run("no config readable", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()

		got := checkContainerdHandler(root, "kata-qemu")
		if got.Configured {
			t.Fatalf("got Configured=true, want false: %+v", got)
		}
		if got.State != handlerUnverifiable {
			t.Errorf("State = %q, want %q", got.State, handlerUnverifiable)
		}
		if !strings.Contains(got.Reason, "no containerd configuration is readable") {
			t.Errorf("Reason = %q, want it to say the config was unreadable", got.Reason)
		}
	})
}
