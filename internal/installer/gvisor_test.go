// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package installer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The installer laid kata only, so a plain `helm install` produced a node whose
// runtime agent reported runtime.gvisor=false and no Sandbox ever scheduled —
// on the profile where gvisor is the ONE backend (setec#89).

func TestConverge_LaysGvisorPayloadAndLinks(t *testing.T) {
	fx := newHostFixture(t, flavorContainerd)
	runner := newFakeRunner(t)
	runner.respond["containerd config default"] = fakeResponse{out: "version = 2\n"}
	runner.respond["systemctl is-active containerd.service"] = fakeResponse{out: "active\n"}
	inst := newTestInstaller(t, fx, runner)

	if _, err := inst.Converge(context.Background()); err != nil {
		t.Fatalf("Converge: %v", err)
	}

	// The sentry matters as much as runsc: without it runsc refuses to create a
	// sandbox under the default sidecar policy, so a node missing it is prepared
	// in name only.
	for _, want := range []string{
		"opt/gvisor/runsc",
		"opt/gvisor/containerd-shim-runsc-v1",
		"opt/gvisor/gvisor-bin/gvisor_sentry",
		"opt/gvisor/VERSION",
	} {
		if _, err := os.Stat(filepath.Join(fx.root, want)); err != nil {
			t.Errorf("%s was not laid onto the host: %v", want, err)
		}
	}

	// containerd resolves the shim by name, and the shim resolves runsc by name.
	for link, target := range map[string]string{
		"usr/local/bin/runsc":                    gvisorRunscBin,
		"usr/local/bin/containerd-shim-runsc-v1": gvisorShimBin,
	} {
		got, err := os.Readlink(filepath.Join(fx.root, link))
		if err != nil {
			t.Errorf("%s is not a symlink: %v", link, err)
			continue
		}
		if got != target {
			t.Errorf("%s -> %q, want %q", link, got, target)
		}
	}
}

// The runsc handler must be registered under the containerd 2.x table on a
// version-3 config. The 1.x table is SILENTLY IGNORED by a 2.x containerd: the
// stanza is present, containerd starts clean, and kubelet then fails the pod
// with "no runtime for runsc is configured".
func TestRegistrationTOML_GvisorTableFollowsConfigVersion(t *testing.T) {
	fx := newHostFixture(t, flavorContainerd)
	inst := newTestInstaller(t, fx, newFakeRunner(t))

	v3 := inst.registrationTOML(3, modeFull)
	if !strings.Contains(v3, `[plugins."io.containerd.cri.v1.runtime".containerd.runtimes.runsc]`) {
		t.Errorf("version 3 config did not register runsc under the 2.x CRI table:\n%s", v3)
	}
	if strings.Contains(v3, `io.containerd.grpc.v1.cri".containerd.runtimes.runsc`) {
		t.Error("version 3 config used the 1.x table, which a 2.x containerd silently ignores")
	}

	v2 := inst.registrationTOML(2, modeFull)
	if !strings.Contains(v2, `[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runsc]`) {
		t.Errorf("version 2 config did not register runsc under the 1.x CRI table:\n%s", v2)
	}

	// Both versions must name the shim's runtime_type, or containerd has nothing
	// to resolve on PATH.
	for name, toml := range map[string]string{"v2": v2, "v3": v3} {
		if !strings.Contains(toml, `runtime_type = "io.containerd.runsc.v1"`) {
			t.Errorf("%s config did not set runsc's runtime_type", name)
		}
	}
}

// /etc/containerd at 0644 has no execute bit, so a non-root process cannot
// traverse into it. The runtime agent then reported runtime.gvisor=false with
// "no containerd configuration is readable on this node" — on a node whose
// containerd was correctly configured. Nothing failed loudly.
func TestConverge_MakesContainerdConfigDirTraversable(t *testing.T) {
	fx := newHostFixture(t, flavorContainerd)
	dir := filepath.Join(fx.root, "etc/containerd")
	if err := os.Chmod(dir, 0o644); err != nil {
		t.Fatal(err)
	}

	runner := newFakeRunner(t)
	runner.respond["containerd config default"] = fakeResponse{out: "version = 2\n"}
	runner.respond["systemctl is-active containerd.service"] = fakeResponse{out: "active\n"}
	inst := newTestInstaller(t, fx, runner)

	if _, err := inst.Converge(context.Background()); err != nil {
		t.Fatalf("Converge: %v", err)
	}

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o755 {
		t.Errorf("/etc/containerd is %#o, want 0755 so the non-root runtime agent can traverse it", perm)
	}
}

// A payload missing the sentry is an image defect and must fail before any host
// write, so a bad image cannot half-prepare a node.
func TestConverge_RefusesAGvisorPayloadMissingTheSentry(t *testing.T) {
	fx := newHostFixture(t, flavorContainerd)
	if err := os.Remove(filepath.Join(fx.gvisorPayload, "gvisor-bin/gvisor_sentry")); err != nil {
		t.Fatal(err)
	}

	runner := newFakeRunner(t)
	runner.respond["containerd config default"] = fakeResponse{out: "version = 2\n"}
	runner.respond["systemctl is-active containerd.service"] = fakeResponse{out: "active\n"}
	inst := newTestInstaller(t, fx, runner)

	_, err := inst.Converge(context.Background())
	if err == nil {
		t.Fatal("Converge accepted a gvisor payload with no sentry; runsc cannot create a sandbox without it")
	}
	if !strings.Contains(err.Error(), "gvisor_sentry") {
		t.Errorf("error does not name the missing artifact: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(fx.root, "opt/gvisor/runsc")); statErr == nil {
		t.Error("the installer wrote part of the payload before failing")
	}
}

// A converged node performs zero writes on a re-run, including for gvisor.
func TestConverge_GvisorIsIdempotent(t *testing.T) {
	fx := newHostFixture(t, flavorContainerd)
	runner := newFakeRunner(t)
	runner.respond["containerd config default"] = fakeResponse{out: "version = 2\n"}
	runner.respond["systemctl is-active containerd.service"] = fakeResponse{out: "active\n"}

	inst := newTestInstaller(t, fx, runner)
	if _, err := inst.Converge(context.Background()); err != nil {
		t.Fatalf("first Converge: %v", err)
	}

	changed, err := inst.ensureGvisorPayload()
	if err != nil {
		t.Fatalf("second ensureGvisorPayload: %v", err)
	}
	if changed {
		t.Error("a converged node reported a gvisor change on the second pass")
	}
}
