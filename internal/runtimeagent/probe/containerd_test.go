// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

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

// TestScanContainerdConfig covers the containerd config schemas the scan has to
// understand, the files it must decline to read meaning into, and the `imports`
// chains it must follow to find a drop-in nobody enumerated (setec#281).
func TestScanContainerdConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		files          map[string]string // relative path -> content
		wantHandler    []string          // handlers that must be present
		wantAbsent     []string          // handlers that must NOT be present
		wantScanned    int
		wantUnreadable []string // host paths the scan must report as unreachable
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
		{
			// THE setec#281 SHAPE. kata-deploy 3.28 writes its drop-in outside
			// every directory this package used to know about and registers it
			// through `imports`. Before the fix the scan read config.toml, saw
			// only runc, and reported kata "absent" on a node running kata.
			name: "absolute imports glob outside the seed dirs is followed",
			files: map[string]string{
				stockConfigRelPath: "version = 2\n" +
					"imports = [\"/opt/kata/containerd/config.d/*.toml\"]\n" +
					"[plugins.\"io.containerd.grpc.v1.cri\".containerd.runtimes.runc]\n",
				"opt/kata/containerd/config.d/kata-deploy.toml": containerdConfigWith("kata-fc", "kata-qemu"),
			},
			wantHandler: []string{"runc", "kata-fc", "kata-qemu"},
			wantScanned: 2,
		},
		{
			// containerd resolves a relative entry against the directory of the
			// file that declared it, NOT the process working directory. A join
			// against FSRoot instead would silently find nothing.
			name: "relative imports entry resolves against the declaring file's directory",
			files: map[string]string{
				stockConfigRelPath:                    "version = 2\nimports = [\"extra/kata.toml\"]\n",
				"etc/containerd/extra/kata.toml":      containerdConfigWith("kata-fc"),
				"opt/kata/containerd/extra/kata.toml": containerdConfigWith("gvisor-decoy"),
			},
			wantHandler: []string{"kata-fc"},
			wantAbsent:  []string{"gvisor-decoy"},
			wantScanned: 2,
		},
		{
			// Both forms in one array, which is what a node carrying an
			// installer drop-in AND a kata-deploy drop-in actually looks like.
			name: "a mixed glob + relative imports array is followed whole",
			files: map[string]string{
				stockConfigRelPath: "version = 2\n" +
					"imports = [\n" +
					"  \"/opt/kata/containerd/config.d/*.toml\",\n" +
					"  'config.d/99-setec-kata-fc.toml',\n" +
					"]\n",
				"opt/kata/containerd/config.d/kata-deploy.toml": containerdConfigWith("kata-qemu"),
				"etc/containerd/config.d/99-setec-kata-fc.toml": containerdConfigWith("kata-fc"),
			},
			wantHandler: []string{"kata-fc", "kata-qemu"},
			wantScanned: 3,
		},
		{
			// An import chain: config.toml -> a.toml -> b.toml.
			name: "imports are followed transitively",
			files: map[string]string{
				stockConfigRelPath:        "version = 2\nimports = [\"/etc/containerd/a/a.toml\"]\n",
				"etc/containerd/a/a.toml": "imports = [\"/etc/containerd/b/b.toml\"]\n",
				"etc/containerd/b/b.toml": containerdConfigWith("kata-fc"),
			},
			wantHandler: []string{"kata-fc"},
			wantScanned: 3,
		},
		{
			// A cycle must terminate and must not double-count.
			name: "an imports cycle terminates",
			files: map[string]string{
				stockConfigRelPath:        "version = 2\nimports = [\"/etc/containerd/a/a.toml\"]\n",
				"etc/containerd/a/a.toml": "imports = [\"" + "/" + stockConfigRelPath + "\"]\n" + containerdConfigWith("kata-fc"),
			},
			wantHandler: []string{"kata-fc"},
			wantScanned: 2,
		},
		{
			// The diagnostic that would have made setec#281 a five-minute
			// read: the config names a path, the agent cannot see it, and the
			// scan says so instead of concluding the handler is absent.
			name: "an import pointing at an unmounted path is reported, not ignored",
			files: map[string]string{
				stockConfigRelPath: "version = 2\n" +
					"imports = [\"/opt/kata/containerd/config.d/*.toml\"]\n" +
					"[plugins.\"io.containerd.grpc.v1.cri\".containerd.runtimes.runc]\n",
			},
			wantHandler:    []string{"runc"},
			wantAbsent:     []string{"kata-fc"},
			wantScanned:    1,
			wantUnreadable: []string{"/opt/kata/containerd/config.d/*.toml"},
		},
		{
			// A mounted-but-empty drop-in directory is a COMPLETE answer, so it
			// must not be reported as unreadable — otherwise every stock node
			// with an empty config.d degrades to "unverifiable" forever.
			name: "a mounted but empty imports directory is not reported as unreadable",
			files: map[string]string{
				stockConfigRelPath: "version = 2\n" +
					"imports = [\"/etc/containerd/config.d/*.toml\"]\n" +
					"[plugins.\"io.containerd.grpc.v1.cri\".containerd.runtimes.runc]\n",
				"etc/containerd/config.d/.keep": "",
			},
			wantHandler: []string{"runc"},
			wantAbsent:  []string{"kata-fc"},
			wantScanned: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			root := t.TempDir()
			for rel, content := range tc.files {
				writeFile(t, root, rel, content)
			}

			scan := ScanContainerdConfig(root)

			if len(scan.Scanned) != tc.wantScanned {
				t.Errorf("scanned %d file(s) (%v), want %d", len(scan.Scanned), scan.Scanned, tc.wantScanned)
			}
			for _, h := range tc.wantHandler {
				if _, ok := scan.Handlers[h]; !ok {
					t.Errorf("handler %q missing; got %v", h, scan.Handlers)
				}
			}
			for _, h := range tc.wantAbsent {
				if _, ok := scan.Handlers[h]; ok {
					t.Errorf("handler %q unexpectedly present; got %v", h, scan.Handlers)
				}
			}
			if got, want := strings.Join(scan.Unreadable, ","), strings.Join(tc.wantUnreadable, ","); got != want {
				t.Errorf("unreadable imports = %q, want %q", got, want)
			}
		})
	}
}

// kataDeployFCConfig is the kata-deploy 3.28 shape for the fc shim: the
// handler names the devmapper snapshotter (the chart default for fc,
// "requires pre-configuration on the user side") and nothing configures it.
const kataDeployFCConfig = "version = 2\n" +
	"[plugins.\"io.containerd.grpc.v1.cri\".containerd.runtimes.kata-fc]\n" +
	"  runtime_type = \"io.containerd.kata-fc.v2\"\n" +
	"  privileged_without_host_devices = true\n" +
	"  snapshotter = \"devmapper\"\n" +
	"[plugins.\"io.containerd.grpc.v1.cri\".containerd.runtimes.kata-fc.options]\n" +
	"  ConfigPath = \"/opt/kata/share/defaults/kata-containers/configuration-fc.toml\"\n"

// devmapperPluginTable is the snapshotter plugin table that makes containerd
// load devmapper at all.
const devmapperPluginTable = "[plugins.\"io.containerd.snapshotter.v1.devmapper\"]\n" +
	"  root_path = \"/var/lib/containerd/io.containerd.snapshotter.v1.devmapper\"\n" +
	"  pool_name = \"setec-thinpool\"\n"

// TestScanContainerdConfigSnapshotters covers the two facts the installer
// needs beside the handler set (setec#9): which snapshotter a handler asks
// for, and which snapshotter plugins the configuration actually carries.
func TestScanContainerdConfigSnapshotters(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		files            map[string]string
		wantHandlerSnaps map[string]string
		wantSnapshotters []string
		wantNoSnapshot   []string
	}{
		{
			// THE setec#9 SHAPE: the handler wants devmapper, nothing
			// provides it. Every kata-fc pod fails in containerd with
			// "snapshotter must be provided to unpack".
			name: "kata-deploy fc handler names devmapper that nothing configures",
			files: map[string]string{
				stockConfigRelPath: "version = 2\nimports = [\"/opt/kata/containerd/config.d/*.toml\"]\n",
				"opt/kata/containerd/config.d/kata-deploy.toml": kataDeployFCConfig,
			},
			wantHandlerSnaps: map[string]string{"kata-fc": "devmapper"},
			wantNoSnapshot:   []string{"devmapper"},
		},
		{
			// The packer AMI shape: handler and snapshotter in one drop-in.
			name: "handler and its snapshotter plugin both configured",
			files: map[string]string{
				stockConfigRelPath: "version = 2\nimports = [\"/etc/containerd/config.d/*.toml\"]\n",
				"etc/containerd/config.d/99-setec-kata-fc.toml": devmapperPluginTable + kataDeployFCConfig,
			},
			wantHandlerSnaps: map[string]string{"kata-fc": "devmapper"},
			wantSnapshotters: []string{"devmapper"},
		},
		{
			name: "a handler with no snapshotter key is not reported",
			files: map[string]string{
				stockConfigRelPath: containerdConfigWith("kata-qemu"),
			},
			wantHandlerSnaps: map[string]string{},
		},
		{
			// A snapshotter key under the .options sub-table is not the
			// handler's, and a single-quoted v3 table is read like a v2 one.
			name: "v3 schema, key under the handler table only",
			files: map[string]string{
				stockConfigRelPath: "version = 3\n" +
					"[plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.\"kata-fc\"]\n" +
					"  snapshotter = 'devmapper'\n" +
					"[plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.\"kata-qemu\".options]\n" +
					"  snapshotter = 'overlayfs'\n" +
					"[plugins.'io.containerd.snapshotter.v1.erofs']\n",
			},
			wantHandlerSnaps: map[string]string{"kata-fc": "devmapper"},
			wantSnapshotters: []string{"erofs"},
			wantNoSnapshot:   []string{"devmapper"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			for rel, content := range tc.files {
				writeFile(t, root, rel, content)
			}
			scan := ScanContainerdConfig(root)
			if len(scan.HandlerSnapshotters) != len(tc.wantHandlerSnaps) {
				t.Errorf("handler snapshotters = %v, want %v", scan.HandlerSnapshotters, tc.wantHandlerSnaps)
			}
			for h, want := range tc.wantHandlerSnaps {
				if got := scan.HandlerSnapshotters[h]; got != want {
					t.Errorf("handler %q snapshotter = %q, want %q", h, got, want)
				}
			}
			for _, s := range tc.wantSnapshotters {
				if _, ok := scan.Snapshotters[s]; !ok {
					t.Errorf("snapshotter plugin %q missing; got %v", s, scan.Snapshotters)
				}
			}
			for _, s := range tc.wantNoSnapshot {
				if _, ok := scan.Snapshotters[s]; ok {
					t.Errorf("snapshotter plugin %q unexpectedly present; got %v", s, scan.Snapshotters)
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
