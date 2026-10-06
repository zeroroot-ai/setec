// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

//go:build linux

package guestagent

import (
	"os"
	"path/filepath"
	"testing"
)

// TestClaimWorkspace_GivesANewWorkspaceToTheUser needs files owned by root,
// for example under unshare -r. A new workspace (root-owned, lost+found
// only) goes to the image user; a workspace with data stays as it is.
func TestClaimWorkspace_GivesANewWorkspaceToTheUser(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root; run under unshare -r")
	}
	root := t.TempDir()
	_ = os.MkdirAll(filepath.Join(root, "etc"), 0o755)
	_ = os.WriteFile(filepath.Join(root, "etc", "passwd"), []byte("runner:x:999:999::/home/runner:/bin/sh\n"), 0o644)
	ws := filepath.Join(root, WorkspaceDir)
	_ = os.MkdirAll(filepath.Join(ws, "lost+found"), 0o700)
	var owned []string
	sup := &Supervisor{Root: root, chown: func(p string, uid, gid int) error {
		if uid != 999 || gid != 999 {
			t.Fatalf("chown %s to %d:%d, want 999:999", p, uid, gid)
		}
		owned = append(owned, p)
		return nil
	}}
	if err := sup.ClaimWorkspace("runner"); err != nil {
		t.Fatalf("ClaimWorkspace: %v", err)
	}
	if len(owned) != 1 || owned[0] != ws {
		t.Fatalf("chowned %v, want the new workspace", owned)
	}

	// A workspace with data is never taken over.
	_ = os.WriteFile(filepath.Join(ws, "data"), []byte("x"), 0o600)
	if err := sup.ClaimWorkspace("runner"); err != nil {
		t.Fatal(err)
	}
	if len(owned) != 1 {
		t.Fatal("a workspace with data changed its owner")
	}
}
