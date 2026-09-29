// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package katasandbox

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
)

type fakeLookup map[string]string

// resolveIn resolves testPodUID against a fake lookup under root.
func resolveIn(root string) (Paths, error) {
	r := Resolver{Lookup: fakeLookup{testPodUID: testSandboxID}, VCRoot: root, RootOf: jailedRoot}
	return r.Resolve(context.Background(), testPodUID)
}

func (f fakeLookup) SandboxID(_ context.Context, podUID string) (string, error) {
	id, ok := f[podUID]
	if !ok {
		return "", ErrNotFound
	}
	return id, nil
}

const (
	testPodUID = "05c716c8-eae5-4540-9daf-5c0bb659810a"
	// A CRI sandbox id is 64 hex characters; kata keeps the first 32.
	testSandboxID = "9f3c2a1b7e6d5c4b3a291807f6e5d4c3b2a19087f6e5d4c3b2a1908f7e6d5c4b"
)

// makeKataSandbox lays out the kata Go runtime's files for the test
// sandbox under root, as fc.go setPaths does, and returns its VM root.
func makeKataSandbox(t *testing.T, root, hypervisor string) string {
	t.Helper()
	vmRoot := filepath.Join(root, hypervisor, testSandboxID[:kataIDLen], "root")
	if err := os.MkdirAll(filepath.Join(vmRoot, "run"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(vmRoot, "run", "firecracker.socket"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return vmRoot
}

// TestResolve_KataGoRuntimeLayout asserts the paths the kata 4.2.0 Go
// runtime uses: the socket under the truncated CRI sandbox id, never
// under the Pod UID (setec#19: /run/kata-containers/<pod-uid>/... was
// ENOENT on every pause and snapshot).
func TestResolve_KataGoRuntimeLayout(t *testing.T) {
	root := t.TempDir()
	vmRoot := makeKataSandbox(t, root, "firecracker")

	got, err := resolveIn(root)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if want := filepath.Join(vmRoot, "run", "firecracker.socket"); got.APISocket != want {
		t.Errorf("APISocket = %q, want %q", got.APISocket, want)
	}
	if want := filepath.Join(vmRoot, "kata.hvsock"); got.HybridVsock != want {
		t.Errorf("HybridVsock = %q, want %q", got.HybridVsock, want)
	}
}

// TestResolve_AnyHypervisorBinaryName asserts that the hypervisor
// directory follows the configured binary's name.
func TestResolve_AnyHypervisorBinaryName(t *testing.T) {
	root := t.TempDir()
	vmRoot := makeKataSandbox(t, root, "firecracker-v1.12")
	got, err := resolveIn(root)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if want := filepath.Join(vmRoot, "run", "firecracker.socket"); got.APISocket != want {
		t.Errorf("APISocket = %q, want %q", got.APISocket, want)
	}
}

// TestResolve_NoSocketIsNotFound asserts that a sandbox id with no
// socket on disk is ErrNotFound, not a guessed path.
func TestResolve_NoSocketIsNotFound(t *testing.T) {
	_, err := resolveIn(t.TempDir())
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Resolve with no socket = %v, want ErrNotFound", err)
	}
}

// TestResolve_UnknownPodIsNotFound asserts that the lookup's
// ErrNotFound comes through.
func TestResolve_UnknownPodIsNotFound(t *testing.T) {
	_, err := Resolver{Lookup: fakeLookup{}, VCRoot: t.TempDir(), RootOf: jailedRoot}.Resolve(context.Background(), testPodUID)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Resolve for an unknown pod = %v, want ErrNotFound", err)
	}
}

// TestResolve_TwoSocketsIsAnError asserts that an ambiguous layout is
// refused rather than resolved to one of them.
func TestResolve_TwoSocketsIsAnError(t *testing.T) {
	root := t.TempDir()
	makeKataSandbox(t, root, "firecracker")
	makeKataSandbox(t, root, "firecracker-other")
	_, err := resolveIn(root)
	if err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("Resolve with two sockets = %v, want an ambiguity error", err)
	}
}

// TestResolve_RefusesNonUID asserts that a value that is not a UID
// never reaches the containerd filter or the glob.
func TestResolve_RefusesNonUID(t *testing.T) {
	for _, bad := range []string{"", "../etc", `x",labels."a"==b`, "*"} {
		if _, err := (Resolver{Lookup: fakeLookup{}, VCRoot: t.TempDir()}).Resolve(context.Background(), bad); err == nil {
			t.Errorf("Resolve(%q) = nil error, want a refusal", bad)
		}
	}
}

// jailedRoot stands in for firecrackerRoot on a layout with no live
// socket: a jailed Firecracker's root is the directory above run/.
func jailedRoot(_ context.Context, hostSocket string) (string, error) {
	return filepath.Dir(filepath.Dir(hostSocket)), nil
}

// TestResolve_ReportsTheFirecrackerRoot asserts that Paths carries the
// chroot Firecracker resolves file paths in.
func TestResolve_ReportsTheFirecrackerRoot(t *testing.T) {
	root := t.TempDir()
	vmRoot := makeKataSandbox(t, root, "firecracker")
	got, err := resolveIn(root)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.FCRoot != vmRoot {
		t.Fatalf("FCRoot = %q, want the jailer root %q", got.FCRoot, vmRoot)
	}
}

// TestFCRootFrom covers the jailed and unjailed socket layouts.
func TestFCRootFrom(t *testing.T) {
	const host = "/run/vc/firecracker/abc/root/run/firecracker.socket"
	cases := []struct {
		name, boundAs, want string
		wantErr             bool
	}{
		{"jailed", "/run/firecracker.socket", "/run/vc/firecracker/abc/root", false},
		{"unjailed", host, "/", false},
		{"no address", "", "/", false},
		{"not a suffix", "/other/firecracker.socket", "", true},
		{"relative", "run/firecracker.socket", "", true},
	}
	for _, c := range cases {
		got, err := fcRootFrom(host, c.boundAs)
		if (err != nil) != c.wantErr || got != c.want {
			t.Errorf("%s: fcRootFrom = (%q, %v), want (%q, error=%t)", c.name, got, err, c.want, c.wantErr)
		}
	}
}

// TestFCPath maps host paths into Firecracker's view and refuses a
// path outside its root.
func TestFCPath(t *testing.T) {
	p := Paths{FCRoot: "/run/vc/firecracker/abc/root"}
	got, err := p.FCPath("/run/vc/firecracker/abc/root/setec-snapshots/s1/state.bin")
	if err != nil || got != "/setec-snapshots/s1/state.bin" {
		t.Fatalf("FCPath = (%q, %v), want /setec-snapshots/s1/state.bin", got, err)
	}
	if _, err := p.FCPath("/var/lib/setec/tmp/state.bin"); err == nil {
		t.Fatal("FCPath outside the root = nil error, want a refusal")
	}
	if got, err := (Paths{FCRoot: "/"}).FCPath("/var/lib/x"); err != nil || got != "/var/lib/x" {
		t.Fatalf("unjailed FCPath = (%q, %v), want /var/lib/x", got, err)
	}
}

// TestFirecrackerRoot_UnjailedSocket dials a real socket bound by its
// full path, as an unjailed Firecracker binds it: the root is "/".
func TestFirecrackerRoot_UnjailedSocket(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "fc.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		if c, err := ln.Accept(); err == nil {
			_ = c.Close()
		}
	}()
	got, err := firecrackerRoot(context.Background(), sock)
	if err != nil || got != "/" {
		t.Fatalf("firecrackerRoot = (%q, %v), want /", got, err)
	}
}
