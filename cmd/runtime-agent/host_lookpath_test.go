// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// hostTree builds a fake host root. files maps a host-absolute path to a mode,
// links maps a host-absolute link path to its target exactly as the host
// stores it.
func hostTree(t *testing.T, files map[string]os.FileMode, links map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for p, mode := range files {
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatal(err)
		}
		//nolint:gosec // G306: an executable fixture
		if err := os.WriteFile(full, []byte("#!/bin/true\n"), mode); err != nil {
			t.Fatal(err)
		}
	}
	for p, target := range links {
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, full); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// Node preparation lays /usr/local/bin/runsc as a symlink with a host-absolute
// target. Before statUnderRoot, the lookup followed that target against the
// test machine's own filesystem and reported "not found", so a node that was
// just prepared advertised runtime.gvisor=false.
func TestHostLookPath_FollowsAnAbsoluteSymlinkInsideTheHostRoot(t *testing.T) {
	root := hostTree(t,
		map[string]os.FileMode{"/opt/gvisor/runsc": 0o755},
		map[string]string{"/usr/local/bin/runsc": "/opt/gvisor/runsc"})

	got, err := hostLookPath(root)("runsc")
	if err != nil {
		t.Fatalf("runsc laid by node preparation was not found: %v", err)
	}
	if want := filepath.Join(root, "/usr/local/bin/runsc"); got != want {
		t.Fatalf("path = %q, want %q", got, want)
	}
}

func TestHostLookPath(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]os.FileMode
		links map[string]string
		found bool
	}{
		{name: "a regular executable is found",
			files: map[string]os.FileMode{"/usr/bin/runsc": 0o755}, found: true},
		{name: "a relative symlink is found",
			files: map[string]os.FileMode{"/usr/lib/gvisor/runsc": 0o755},
			links: map[string]string{"/usr/bin/runsc": "../lib/gvisor/runsc"}, found: true},
		{name: "a chain of two absolute symlinks is found",
			files: map[string]os.FileMode{"/opt/gvisor/v1/runsc": 0o755},
			links: map[string]string{
				"/usr/local/bin/runsc": "/opt/gvisor/runsc",
				"/opt/gvisor/runsc":    "/opt/gvisor/v1/runsc",
			}, found: true},
		// The target is not under the mounted tree. The probe cannot see the
		// binary, so it must not vouch for it.
		{name: "a symlink to a target that is not in the host tree is not found",
			links: map[string]string{"/usr/local/bin/runsc": "/opt/gvisor/runsc"}},
		{name: "a symlink to a file that is not executable is not found",
			files: map[string]os.FileMode{"/opt/gvisor/runsc": 0o644},
			links: map[string]string{"/usr/local/bin/runsc": "/opt/gvisor/runsc"}},
		{name: "a symlink to a directory is not found",
			files: map[string]os.FileMode{"/opt/gvisor/runsc/keep": 0o755},
			links: map[string]string{"/usr/local/bin/runsc": "/opt/gvisor/runsc"}},
		{name: "a symlink loop is not found",
			links: map[string]string{
				"/usr/local/bin/runsc":  "/usr/local/bin/runsc2",
				"/usr/local/bin/runsc2": "/usr/local/bin/runsc",
			}},
		{name: "a missing binary is not found"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := hostTree(t, tc.files, tc.links)
			got, err := hostLookPath(root)("runsc")
			if tc.found && err != nil {
				t.Fatalf("not found: %v", err)
			}
			if !tc.found && err == nil {
				t.Fatalf("found at %q, want not found", got)
			}
			if tc.found && !strings.HasPrefix(got, root) {
				t.Fatalf("path %q is outside the host root %q", got, root)
			}
		})
	}
}
