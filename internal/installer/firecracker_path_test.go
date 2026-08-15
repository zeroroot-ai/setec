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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zeroroot-ai/setec/internal/firecracker"
)

// TestFirecrackerPathMatchesLauncherDefault is the regression guard for
// setec#287: the pool-VM launcher's --firecracker-binary default and the path
// this installer actually produces must be the same path.
//
// They were independent string literals and had drifted — the launcher
// defaulted to /usr/local/bin/firecracker ("the standard path kata-deploy lays
// down") while this installer only ever links the shim and leaves Firecracker
// at /opt/kata/bin/firecracker. On any node prepared by setec's own installer
// DaemonSet (the default node-prep path, ADR-0003) the launcher's default
// therefore named a file that does not exist, and nothing failed until a pool
// VM tried to spawn on a real node.
//
// The two now share firecracker.HostBinaryPath. This test asserts the
// installer side still routes through it: reverting kataFCBin to a literal
// that disagrees fails here.
func TestFirecrackerPathMatchesLauncherDefault(t *testing.T) {
	if kataFCBin != firecracker.HostBinaryPath {
		t.Fatalf("installer places firecracker at %q but the shared constant "+
			"(and therefore the pool-VM launcher default) is %q; the two must "+
			"not drift — see setec#287",
			kataFCBin, firecracker.HostBinaryPath)
	}

	// The shared path must live under the kata payload root, because that is
	// the tree this installer lays down and the tree configuration-fc.toml
	// resolves against. A path outside it cannot be produced by the installer.
	if !strings.HasPrefix(firecracker.HostBinaryPath, kataHostDir+"/") {
		t.Fatalf("shared firecracker path %q is outside the kata payload root %q; "+
			"the installer only ever creates files under that root",
			firecracker.HostBinaryPath, kataHostDir)
	}

	// It must also be one of the artifacts the installer verifies on-host,
	// otherwise a missing binary would not be caught at install time.
	found := false
	for _, a := range requiredKataArtifacts {
		if a == firecracker.HostBinaryPath {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("firecracker path %q is not in requiredKataArtifacts, so the "+
			"installer would not notice it missing on the host",
			firecracker.HostBinaryPath)
	}
}

// TestPayloadContainsFirecrackerAtSharedPath proves the claim end-to-end
// against a real payload tree: ensureKataPayload copies the payload to the
// host and then verifies every required artifact, so a payload that does not
// carry firecracker at the shared path fails the install rather than silently
// producing a node the launcher cannot use.
func TestPayloadContainsFirecrackerAtSharedPath(t *testing.T) {
	payload := t.TempDir()
	hostRoot := t.TempDir()

	// Build a minimal payload with every required artifact present.
	if err := os.WriteFile(filepath.Join(payload, "VERSION"), []byte("test-1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, artifact := range requiredKataArtifacts {
		rel := strings.TrimPrefix(artifact, kataHostDir+"/")
		p := filepath.Join(payload, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("stub"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	in, err := New(Config{PayloadDir: payload, HostRoot: hostRoot}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := in.ensureKataPayload(); err != nil {
		t.Fatalf("ensureKataPayload with a complete payload: %v", err)
	}

	// The launcher's default must now resolve to a real file on this host.
	if _, err := os.Stat(in.hostPath(firecracker.HostBinaryPath)); err != nil {
		t.Fatalf("after install, the pool-VM launcher default %q does not exist on the host: %v",
			firecracker.HostBinaryPath, err)
	}

	// And the reverse: a payload missing firecracker must fail the install,
	// not leave a node the launcher will fail on later.
	broken := t.TempDir()
	if err := os.WriteFile(filepath.Join(broken, "VERSION"), []byte("test-2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, artifact := range requiredKataArtifacts {
		if artifact == firecracker.HostBinaryPath {
			continue // deliberately absent
		}
		rel := strings.TrimPrefix(artifact, kataHostDir+"/")
		p := filepath.Join(broken, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("stub"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	brokenIn, err := New(Config{PayloadDir: broken, HostRoot: t.TempDir()}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := brokenIn.ensureKataPayload(); err == nil {
		t.Fatal("a payload with no firecracker binary must fail the install")
	}
}
