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

package installer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeRunner records every command and answers from a script keyed on
// "name arg1 arg2...". Unscripted commands succeed with empty output.
type fakeRunner struct {
	t        *testing.T
	calls    []string
	respond  map[string]fakeResponse
	failures map[string]error
}

type fakeResponse struct {
	out string
	err error
}

func newFakeRunner(t *testing.T) *fakeRunner {
	return &fakeRunner{t: t, respond: map[string]fakeResponse{}, failures: map[string]error{}}
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	key := strings.TrimSpace(name + " " + strings.Join(args, " "))
	f.calls = append(f.calls, key)
	if r, ok := f.respond[key]; ok {
		return []byte(r.out), r.err
	}
	return nil, nil
}

func (f *fakeRunner) called(prefix string) int {
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

// hostFixture builds a fake host root with KVM, host tools, and the
// requested runtime flavor, plus a payload directory.
type hostFixture struct {
	root    string
	payload string
}

func newHostFixture(t *testing.T, flavor string) hostFixture {
	t.Helper()
	root := t.TempDir()
	payload := t.TempDir()

	// KVM device + module.
	mustWrite(t, filepath.Join(root, "dev/kvm"), "")
	mustMkdir(t, filepath.Join(root, "sys/module/kvm_intel"))

	// Host tools the preflight looks for.
	for _, tool := range []string{"sh", "dmsetup", "blockdev", "losetup", "truncate"} {
		mustExecutable(t, filepath.Join(root, "usr/bin", tool))
	}

	switch flavor {
	case "containerd":
		mustMkdir(t, filepath.Join(root, "etc/containerd"))
		mustWrite(t, filepath.Join(root, "etc/systemd/system/containerd.service"), "[Unit]\n")
	case "k3s":
		mustMkdir(t, filepath.Join(root, "var/lib/rancher/k3s/agent/etc/containerd"))
		mustWrite(t, filepath.Join(root, "etc/systemd/system/k3s.service"), "[Unit]\n")
	case "none":
		// no runtime markers at all
	default:
		t.Fatalf("unknown fixture flavor %q", flavor)
	}

	// Kata payload tree.
	mustWrite(t, filepath.Join(payload, "VERSION"), "3.28.0\n")
	for _, rel := range []string{
		"bin/containerd-shim-kata-v2",
		"bin/firecracker",
		"bin/jailer",
		"bin/kata-runtime",
	} {
		mustExecutable(t, filepath.Join(payload, rel))
	}
	mustWrite(t, filepath.Join(payload, "share/defaults/kata-containers/configuration-fc.toml"), "# fc config\n")

	return hostFixture{root: root, payload: payload}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustExecutable(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

func newTestInstaller(t *testing.T, fx hostFixture, runner Runner) *Installer {
	t.Helper()
	inst, err := New(Config{
		HostRoot:   fx.root,
		PayloadDir: fx.payload,
		Runner:     runner,
	}, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	return inst
}

func TestConvergeFreshStockContainerdNode(t *testing.T) {
	fx := newHostFixture(t, "containerd")
	runner := newFakeRunner(t)
	runner.respond["containerd config default"] = fakeResponse{out: "version = 2\n"}
	runner.respond["systemctl is-active containerd.service"] = fakeResponse{out: "active\n"}
	inst := newTestInstaller(t, fx, runner)

	res, err := inst.Converge(context.Background())
	if err != nil {
		t.Fatalf("Converge: %v", err)
	}
	if res.Outcome != OutcomeConverged {
		t.Fatalf("outcome = %s, want %s", res.Outcome, OutcomeConverged)
	}
	if !res.Changed || !res.RuntimeRestarted {
		t.Fatalf("fresh node: Changed=%t RuntimeRestarted=%t, want both true", res.Changed, res.RuntimeRestarted)
	}
	if res.Flavor != "containerd" {
		t.Fatalf("flavor = %q, want containerd", res.Flavor)
	}

	// Payload landed with shim symlink.
	for _, p := range []string{
		"opt/kata/bin/firecracker",
		"opt/kata/bin/jailer",
		"opt/kata/share/defaults/kata-containers/configuration-fc.toml",
	} {
		if _, err := os.Stat(filepath.Join(fx.root, p)); err != nil {
			t.Errorf("missing %s after converge: %v", p, err)
		}
	}
	link, err := os.Readlink(filepath.Join(fx.root, "usr/local/bin/containerd-shim-kata-fc-v2"))
	if err != nil || link != "/opt/kata/bin/containerd-shim-kata-v2" {
		t.Errorf("shim symlink = %q, %v", link, err)
	}

	// Thin-pool assets + ordering drop-in.
	for _, p := range []string{
		"usr/local/sbin/setec-thinpool.sh",
		"etc/setec/thinpool.env",
		"etc/systemd/system/setec-thinpool.service",
		"etc/systemd/system/containerd.service.d/10-setec-thinpool.conf",
	} {
		if _, err := os.Stat(filepath.Join(fx.root, p)); err != nil {
			t.Errorf("missing %s after converge: %v", p, err)
		}
	}

	// Containerd drop-in registered with version-2 table names, and the
	// main config imports the drop-in dir.
	dropin := readFile(t, filepath.Join(fx.root, "etc/containerd/config.d/99-setec-kata-fc.toml"))
	if !strings.Contains(dropin, `plugins."io.containerd.grpc.v1.cri".containerd.runtimes.kata-fc`) {
		t.Errorf("drop-in missing v2 runtime table:\n%s", dropin)
	}
	if !strings.Contains(dropin, `pool_name = "setec-thinpool"`) {
		t.Errorf("drop-in missing pool name:\n%s", dropin)
	}
	mainCfg := readFile(t, filepath.Join(fx.root, "etc/containerd/config.toml"))
	if !strings.Contains(mainCfg, `imports = ["/etc/containerd/config.d/*.toml"]`) {
		t.Errorf("main config missing imports line:\n%s", mainCfg)
	}

	// The runtime was restarted exactly once, after the thin-pool was
	// provisioned.
	if n := runner.called("systemctl restart containerd.service"); n != 1 {
		t.Errorf("containerd restarts = %d, want 1", n)
	}
	if n := runner.called("systemctl start setec-thinpool.service") + runner.called("systemctl restart setec-thinpool.service"); n != 1 {
		t.Errorf("thin-pool provisioning invocations = %d, want exactly 1", n)
	}
}

func TestConvergeIsIdempotent(t *testing.T) {
	fx := newHostFixture(t, "containerd")
	runner := newFakeRunner(t)
	runner.respond["containerd config default"] = fakeResponse{out: "version = 2\n"}
	runner.respond["systemctl is-active containerd.service"] = fakeResponse{out: "active\n"}
	inst := newTestInstaller(t, fx, runner)

	if _, err := inst.Converge(context.Background()); err != nil {
		t.Fatalf("first Converge: %v", err)
	}
	restartsAfterFirst := runner.called("systemctl restart containerd.service")

	res, err := inst.Converge(context.Background())
	if err != nil {
		t.Fatalf("second Converge: %v", err)
	}
	if res.Outcome != OutcomeConverged {
		t.Fatalf("outcome = %s, want %s", res.Outcome, OutcomeConverged)
	}
	if res.Changed {
		t.Error("second Converge reported Changed=true, want false (zero writes)")
	}
	if res.RuntimeRestarted {
		t.Error("second Converge restarted the runtime — re-runs must not bounce containerd")
	}
	if n := runner.called("systemctl restart containerd.service"); n != restartsAfterFirst {
		t.Errorf("containerd restarted again on re-run (%d -> %d)", restartsAfterFirst, n)
	}
}

func TestConvergeNoKVMIdles(t *testing.T) {
	fx := newHostFixture(t, "containerd")
	if err := os.Remove(filepath.Join(fx.root, "dev/kvm")); err != nil {
		t.Fatal(err)
	}
	runner := newFakeRunner(t)
	inst := newTestInstaller(t, fx, runner)

	res, err := inst.Converge(context.Background())
	if err != nil {
		t.Fatalf("Converge: %v", err)
	}
	if res.Outcome != OutcomeIdleNoKVM {
		t.Fatalf("outcome = %s, want %s", res.Outcome, OutcomeIdleNoKVM)
	}
	if res.Changed {
		t.Error("no-KVM node was mutated")
	}
	if len(runner.calls) != 0 {
		t.Errorf("no-KVM node ran host commands: %v", runner.calls)
	}
	if _, err := os.Stat(filepath.Join(fx.root, "opt/kata")); !os.IsNotExist(err) {
		t.Error("no-KVM node received the kata payload")
	}
}

func TestConvergeForeignKataFCStandsDown(t *testing.T) {
	fx := newHostFixture(t, "containerd")
	// kata-deploy-style registration in the admin's own config.
	mustWrite(t, filepath.Join(fx.root, "etc/containerd/config.toml"),
		"version = 2\n[plugins.\"io.containerd.grpc.v1.cri\".containerd.runtimes.kata-fc]\n  runtime_type = \"io.containerd.kata-fc.v2\"\n")
	runner := newFakeRunner(t)
	inst := newTestInstaller(t, fx, runner)

	res, err := inst.Converge(context.Background())
	if err != nil {
		t.Fatalf("Converge: %v", err)
	}
	if res.Outcome != OutcomeIdleForeignOwner {
		t.Fatalf("outcome = %s, want %s", res.Outcome, OutcomeIdleForeignOwner)
	}
	if res.Changed {
		t.Error("foreign-owned node was mutated")
	}
	if got := readFile(t, filepath.Join(fx.root, "etc/containerd/config.toml")); !strings.Contains(got, "kata-deploy") && strings.Contains(got, "setec") {
		t.Errorf("foreign config was rewritten:\n%s", got)
	}
}

func TestConvergeK3sFreshNode(t *testing.T) {
	fx := newHostFixture(t, "k3s")
	// k3s renders a version-3 config (containerd 2.x).
	mustWrite(t, filepath.Join(fx.root, "var/lib/rancher/k3s/agent/etc/containerd/config.toml"),
		"version = 3\n")
	runner := newFakeRunner(t)
	runner.respond["systemctl is-active k3s.service"] = fakeResponse{out: "active\n"}
	inst := newTestInstaller(t, fx, runner)

	res, err := inst.Converge(context.Background())
	if err != nil {
		t.Fatalf("Converge: %v", err)
	}
	if res.Outcome != OutcomeConverged || res.Flavor != "k3s" {
		t.Fatalf("outcome=%s flavor=%s, want converged/k3s", res.Outcome, res.Flavor)
	}

	tmpl := readFile(t, filepath.Join(fx.root, "var/lib/rancher/k3s/agent/etc/containerd/config-v3.toml.tmpl"))
	if !strings.Contains(tmpl, `{{ template "base" . }}`) {
		t.Errorf("k3s template does not render the base config:\n%s", tmpl)
	}
	if !strings.Contains(tmpl, `plugins."io.containerd.cri.v1.runtime".containerd.runtimes.kata-fc`) {
		t.Errorf("k3s template missing v3 runtime table:\n%s", tmpl)
	}
	if !strings.Contains(tmpl, beginMarker) || !strings.Contains(tmpl, endMarker) {
		t.Errorf("k3s template missing managed-block markers:\n%s", tmpl)
	}
	// k3s (not containerd) is what gets restarted, and boot ordering
	// targets the k3s unit.
	if n := runner.called("systemctl restart k3s.service"); n != 1 {
		t.Errorf("k3s restarts = %d, want 1", n)
	}
	if _, err := os.Stat(filepath.Join(fx.root, "etc/systemd/system/k3s.service.d/10-setec-thinpool.conf")); err != nil {
		t.Errorf("k3s ordering drop-in missing: %v", err)
	}

	// Second run: no rewrite, no restart.
	res2, err := inst.Converge(context.Background())
	if err != nil {
		t.Fatalf("second Converge: %v", err)
	}
	if res2.Changed || res2.RuntimeRestarted {
		t.Errorf("k3s re-run not idempotent: Changed=%t RuntimeRestarted=%t", res2.Changed, res2.RuntimeRestarted)
	}
}

func TestConvergeK3sPreservesExistingTemplate(t *testing.T) {
	fx := newHostFixture(t, "k3s")
	mustWrite(t, filepath.Join(fx.root, "var/lib/rancher/k3s/agent/etc/containerd/config.toml"), "version = 3\n")
	adminContent := "# admin-owned template\n{{ template \"base\" . }}\n# custom registry tweak\n"
	mustWrite(t, filepath.Join(fx.root, "var/lib/rancher/k3s/agent/etc/containerd/config-v3.toml.tmpl"), adminContent)
	runner := newFakeRunner(t)
	runner.respond["systemctl is-active k3s.service"] = fakeResponse{out: "active\n"}
	inst := newTestInstaller(t, fx, runner)

	if _, err := inst.Converge(context.Background()); err != nil {
		t.Fatalf("Converge: %v", err)
	}
	tmpl := readFile(t, filepath.Join(fx.root, "var/lib/rancher/k3s/agent/etc/containerd/config-v3.toml.tmpl"))
	if !strings.Contains(tmpl, "# custom registry tweak") {
		t.Errorf("admin template content lost:\n%s", tmpl)
	}
	if !strings.Contains(tmpl, beginMarker) {
		t.Errorf("managed block not appended:\n%s", tmpl)
	}
	// Pristine original backed up.
	backup := readFile(t, filepath.Join(fx.root, "var/lib/rancher/k3s/agent/etc/containerd/config-v3.toml.tmpl"+backupSuffix))
	if backup != adminContent {
		t.Errorf("backup is not the pristine original:\n%s", backup)
	}
}

func TestConvergePreservesExistingImports(t *testing.T) {
	fx := newHostFixture(t, "containerd")
	mustWrite(t, filepath.Join(fx.root, "etc/containerd/config.toml"),
		"version = 2\nimports = [\"/etc/containerd/extra.toml\"]\n[metrics]\n  address = \"127.0.0.1:1338\"\n")
	runner := newFakeRunner(t)
	runner.respond["containerd config default"] = fakeResponse{out: "version = 2\n"}
	runner.respond["systemctl is-active containerd.service"] = fakeResponse{out: "active\n"}
	inst := newTestInstaller(t, fx, runner)

	if _, err := inst.Converge(context.Background()); err != nil {
		t.Fatalf("Converge: %v", err)
	}
	cfg := readFile(t, filepath.Join(fx.root, "etc/containerd/config.toml"))
	if !strings.Contains(cfg, `"/etc/containerd/extra.toml", "/etc/containerd/config.d/*.toml"`) {
		t.Errorf("existing imports entry lost or ours missing:\n%s", cfg)
	}
	if !strings.Contains(cfg, "[metrics]") {
		t.Errorf("unrelated config lost:\n%s", cfg)
	}
	// Backup of the pristine original exists.
	if _, err := os.Stat(filepath.Join(fx.root, "etc/containerd/config.toml"+backupSuffix)); err != nil {
		t.Errorf("no pristine backup: %v", err)
	}
}

func TestConvergeMissingHostToolsFailsLoudly(t *testing.T) {
	fx := newHostFixture(t, "containerd")
	if err := os.Remove(filepath.Join(fx.root, "usr/bin/dmsetup")); err != nil {
		t.Fatal(err)
	}
	runner := newFakeRunner(t)
	inst := newTestInstaller(t, fx, runner)

	_, err := inst.Converge(context.Background())
	if err == nil || !strings.Contains(err.Error(), "dmsetup") {
		t.Fatalf("want missing-dmsetup error, got %v", err)
	}
	// Nothing was written before the preflight failure.
	if _, statErr := os.Stat(filepath.Join(fx.root, "opt/kata")); !os.IsNotExist(statErr) {
		t.Error("payload written despite failed preflight")
	}
}

func TestConvergeNoSupportedRuntime(t *testing.T) {
	fx := newHostFixture(t, "none")
	runner := newFakeRunner(t)
	inst := newTestInstaller(t, fx, runner)

	_, err := inst.Converge(context.Background())
	if err == nil || !strings.Contains(err.Error(), "no supported container runtime") {
		t.Fatalf("want no-supported-runtime error, got %v", err)
	}
}

func TestConvergeRepairsDamagedPayload(t *testing.T) {
	fx := newHostFixture(t, "containerd")
	runner := newFakeRunner(t)
	runner.respond["containerd config default"] = fakeResponse{out: "version = 2\n"}
	runner.respond["systemctl is-active containerd.service"] = fakeResponse{out: "active\n"}
	inst := newTestInstaller(t, fx, runner)

	if _, err := inst.Converge(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Simulate a partially deleted /opt/kata with a surviving VERSION.
	if err := os.Remove(filepath.Join(fx.root, "opt/kata/bin/firecracker")); err != nil {
		t.Fatal(err)
	}
	res, err := inst.Converge(context.Background())
	if err != nil {
		t.Fatalf("repair Converge: %v", err)
	}
	if !res.Changed {
		t.Error("damaged payload not repaired (Changed=false)")
	}
	if _, err := os.Stat(filepath.Join(fx.root, "opt/kata/bin/firecracker")); err != nil {
		t.Errorf("firecracker not restored: %v", err)
	}
}

func TestThinpoolEnvRendering(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  Config
		want []string
		omit []string
	}{
		{
			name: "loop mode",
			cfg:  Config{ThinpoolMode: ThinpoolModeLoop, LoopDataGB: 100, LoopMetaGB: 4},
			want: []string{
				"SETEC_THINPOOL_MODE=loop",
				"SETEC_THINPOOL_NAME=setec-thinpool",
				"SETEC_THINPOOL_DATA_GB=100",
				"SETEC_THINPOOL_META_GB=4",
				"SETEC_THINPOOL_DIR=/var/lib/setec/thinpool",
			},
			omit: []string{"SETEC_THINPOOL_DATA_DEVICE"},
		},
		{
			name: "device mode",
			cfg: Config{
				ThinpoolMode: ThinpoolModeDevice,
				DataDevice:   "/dev/nvme1n1", MetadataDevice: "/dev/nvme2n1",
			},
			want: []string{
				"SETEC_THINPOOL_MODE=device",
				"SETEC_THINPOOL_DATA_DEVICE=/dev/nvme1n1",
				"SETEC_THINPOOL_META_DEVICE=/dev/nvme2n1",
			},
			omit: []string{"SETEC_THINPOOL_DATA_GB"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := tc.cfg
			cfg.HostRoot = t.TempDir()
			cfg.Runner = newFakeRunner(t)
			inst, err := New(cfg, nil)
			if err != nil {
				t.Fatal(err)
			}
			env := string(inst.thinpoolEnv())
			for _, w := range tc.want {
				if !strings.Contains(env, w) {
					t.Errorf("env missing %q:\n%s", w, env)
				}
			}
			for _, o := range tc.omit {
				if strings.Contains(env, o) {
					t.Errorf("env unexpectedly contains %q:\n%s", o, env)
				}
			}
		})
	}
}

func TestConfigValidate(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{"device mode without devices", func(c *Config) { c.ThinpoolMode = ThinpoolModeDevice }, "requires both"},
		{"unknown mode", func(c *Config) { c.ThinpoolMode = "zfs" }, "unknown thinpool mode"},
		{"loop mode ok", func(c *Config) {}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{Runner: newFakeRunner(t)}
			cfg.ApplyDefaults()
			tc.mutate(&cfg)
			err := cfg.Validate()
			if tc.wantErr == "" && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestEnvChangeRestartsThinpoolUnit(t *testing.T) {
	fx := newHostFixture(t, "containerd")
	runner := newFakeRunner(t)
	runner.respond["containerd config default"] = fakeResponse{out: "version = 2\n"}
	runner.respond["systemctl is-active containerd.service"] = fakeResponse{out: "active\n"}
	inst := newTestInstaller(t, fx, runner)
	if _, err := inst.Converge(context.Background()); err != nil {
		t.Fatal(err)
	}
	baseline := runner.called("systemctl restart setec-thinpool.service")

	// Same host, bigger pool: the env changes, so the oneshot must be
	// restarted (a plain start is a no-op with RemainAfterExit).
	inst2, err := New(Config{
		HostRoot:   fx.root,
		PayloadDir: fx.payload,
		LoopDataGB: 200,
		Runner:     runner,
	}, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := inst2.Converge(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := runner.called("systemctl restart setec-thinpool.service") - baseline; n != 1 {
		t.Errorf("thin-pool unit restarts after env change = %d, want 1", n)
	}
}

func TestLookPathIn(t *testing.T) {
	root := t.TempDir()
	mustExecutable(t, filepath.Join(root, "usr/sbin/dmsetup"))
	got, err := lookPathIn(root, "dmsetup")
	if err != nil || got != "/usr/sbin/dmsetup" {
		t.Fatalf("lookPathIn = %q, %v", got, err)
	}
	if _, err := lookPathIn(root, "nonexistent-tool"); err == nil {
		t.Fatal("want error for missing tool")
	}
	// Absolute path passthrough.
	if got, err := lookPathIn(root, "/usr/sbin/dmsetup"); err != nil || got != "/usr/sbin/dmsetup" {
		t.Fatalf("absolute lookPathIn = %q, %v", got, err)
	}
}

func TestRestartFailureSurfacesUnitName(t *testing.T) {
	fx := newHostFixture(t, "containerd")
	runner := newFakeRunner(t)
	runner.respond["containerd config default"] = fakeResponse{out: "version = 2\n"}
	runner.respond["systemctl is-active containerd.service"] = fakeResponse{out: "activating\n"}
	inst, err := New(Config{
		HostRoot:       fx.root,
		PayloadDir:     fx.payload,
		RestartTimeout: 10 * 1e6, // 10ms — fail fast in tests
		Runner:         runner,
	}, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	_, err = inst.Converge(context.Background())
	if err == nil || !strings.Contains(err.Error(), "containerd.service") || !strings.Contains(err.Error(), backupSuffix) {
		t.Fatalf("want restart-timeout error naming the unit and backup suffix, got %v", err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// verify() must answer questions about the tree the installer wrote, not about
// the machine running the tests (setec#220).
//
// kataShimLink is a symlink to a host-absolute target, so the original
// os.Stat followed it out of HostRoot and onto the real filesystem: green on a
// workstation that happens to have /opt/kata, red on every clean CI runner.
// The assertions below fail on that implementation regardless of which kind of
// machine runs them.
func TestVerifyChecksTheInstalledTreeNotTheHost(t *testing.T) {
	fx := newHostFixture(t, "containerd")
	runner := newFakeRunner(t)
	runner.respond["containerd config default"] = fakeResponse{out: "version = 2\n"}
	runner.respond["systemctl is-active containerd.service"] = fakeResponse{out: "active\n"}
	inst := newTestInstaller(t, fx, runner)
	if _, err := inst.Converge(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Deleting the shim binary INSIDE the fake root must fail verification.
	// os.Stat on the link would still succeed on any host with a real
	// /opt/kata, so this is the assertion that pins root-relative checking.
	if err := os.Remove(filepath.Join(fx.root, kataShimBin)); err != nil {
		t.Fatalf("remove root-relative shim target: %v", err)
	}
	flavor, err := detectFlavor(inst.cfg)
	if err != nil {
		t.Fatalf("detect flavor: %v", err)
	}
	if err := inst.verify(context.Background(), flavor); err == nil {
		t.Fatal("verify passed with the shim target absent from the installed tree — " +
			"it is resolving the symlink against the real filesystem instead of HostRoot")
	}
}
