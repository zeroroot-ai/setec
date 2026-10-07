// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package guestagent

import (
	"os"
	"path/filepath"
	"testing"
)

// TestResolve_FollowsLinksInsideTheRoot proves that a command on the PATH
// resolves through an absolute symbolic link of the image, as /bin/sh ->
// /bin/busybox in alpine, and through a relative one. A link that points
// at nothing in the root does not resolve.
func TestResolve_FollowsLinksInsideTheRoot(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for _, d := range []string{"bin", "usr/bin"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "bin", "busybox"), []byte("#!"), 0o755); err != nil {
		t.Fatal(err)
	}
	links := map[string]string{"bin/sh": "/bin/busybox", "usr/bin/env": "../../bin/busybox", "bin/gone": "/bin/nothing"}
	for link, target := range links {
		if err := os.Symlink(target, filepath.Join(root, link)); err != nil {
			t.Fatal(err)
		}
	}
	s := &Supervisor{Root: root}
	env := []string{"PATH=/usr/bin:/bin"}
	for name, want := range map[string]string{"sh": "/bin/sh", "env": "/usr/bin/env"} {
		got, err := s.resolve(name, env)
		if err != nil || got != want {
			t.Errorf("resolve(%s) = %q, %v; want %q", name, got, err, want)
		}
	}
	if got, err := s.resolve("gone", env); err == nil {
		t.Errorf("resolve(gone) = %q; want an error for a broken link", got)
	}
}
