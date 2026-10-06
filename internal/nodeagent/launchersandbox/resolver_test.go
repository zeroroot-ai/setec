// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package launchersandbox

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/zeroroot-ai/setec/internal/podspec"
)

func TestResolve_FindsTheWorkVolumeOfALauncherPod(t *testing.T) {
	t.Parallel()
	pods := t.TempDir()
	const uid = "0a1b2c3d-0000-4000-8000-000000000001"
	r := Resolver{PodsDir: pods}
	if _, err := r.Resolve(t.Context(), uid); !errors.Is(err, ErrNotFound) {
		t.Fatalf("no launcher yet: err = %v, want ErrNotFound", err)
	}
	work := filepath.Join(pods, uid, "volumes", "kubernetes.io~empty-dir", "work")
	if err := os.MkdirAll(filepath.Join(work, "vm"), 0o700); err != nil {
		t.Fatal(err)
	}
	p, err := r.Resolve(t.Context(), uid)
	if err != nil {
		t.Fatal(err)
	}
	if p.APISocket != filepath.Join(work, "vm", "api.sock") || p.HybridVsock != filepath.Join(work, "vm", "v.sock") {
		t.Fatalf("paths = %+v", p)
	}
	// Firecracker sees the work volume at /work, so a file the node agent
	// writes under it has the Pod path in the Firecracker API.
	got, err := p.FCPath(HostPath(p, podspec.LauncherRestoreState))
	if err != nil || got != "/work/vm/restore/state.bin" {
		t.Fatalf("FCPath = %q, %v", got, err)
	}
	if _, err := p.FCPath("/etc/passwd"); err == nil {
		t.Fatal("FCPath accepted a path outside the work volume")
	}
}

func TestResolve_RefusesAPathInsteadOfAUID(t *testing.T) {
	t.Parallel()
	if _, err := (Resolver{PodsDir: t.TempDir()}).Resolve(t.Context(), "../../etc"); err == nil {
		t.Fatal("Resolve accepted a path")
	}
}
