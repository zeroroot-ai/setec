// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package webhook

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
	setecruntime "github.com/zeroroot-ai/setec/internal/runtime"
)

// SandboxClassSpec.KernelImage and SandboxClassSpec.RootfsImage are served CRD
// fields that no component reads (setec#126). A class naming a hardened or
// digest-pinned kernel was admitted, the Sandbox started, and it booted the
// operator-wide default while reporting nothing.
//
// The microVM is the isolation boundary and the kernel and rootfs are its
// contents, so that substitution silently changed the boundary. These tests pin
// the refusal shut, and the last one pins the part that matters most: a cluster
// that never set either field must be untouched.

func TestSandboxClassWebhook_GuestImageIsRefused(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*setecv1alpha1.SandboxClass)
		wantErr bool
		wantMsg []string
	}{
		{
			name:   "neither field set, a stock class, accept",
			mutate: func(*setecv1alpha1.SandboxClass) {},
		},
		{
			name: "kernelImage set, reject and name the field",
			mutate: func(c *setecv1alpha1.SandboxClass) {
				c.Spec.KernelImage = "ghcr.io/org/hardened-kernel@sha256:" + strings.Repeat("a", 64)
			},
			wantErr: true,
			wantMsg: []string{"spec.kernelImage", "no component reads this field", "setec#126"},
		},
		{
			name: "rootfsImage set, reject and name the field",
			mutate: func(c *setecv1alpha1.SandboxClass) {
				c.Spec.RootfsImage = "ghcr.io/org/minimal-rootfs:v3"
			},
			wantErr: true,
			wantMsg: []string{"spec.rootfsImage", "no component reads this field", "setec#126"},
		},
		{
			// Both at once must report both. Stopping at the first would send an
			// operator round the loop twice for one mistake.
			name: "both set, both reported",
			mutate: func(c *setecv1alpha1.SandboxClass) {
				c.Spec.KernelImage = "ghcr.io/org/k:v1"
				c.Spec.RootfsImage = "ghcr.io/org/r:v1"
			},
			wantErr: true,
			wantMsg: []string{"spec.kernelImage", "spec.rootfsImage"},
		},
		{
			// The empty string is not "set". A class that carries the key with no
			// value is what a templated chart produces, and it must not break.
			name: "explicit empty strings, accept",
			mutate: func(c *setecv1alpha1.SandboxClass) {
				c.Spec.KernelImage = ""
				c.Spec.RootfsImage = ""
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cls := mkSandboxClass("gi", "", mkRuntime(setecruntime.BackendKataFC))
			tc.mutate(cls)

			w := webhookWith(fakeClientWithNS(t, gateNamespaceUnlabelled()), baseConfig())
			_, err := w.ValidateCreate(context.Background(), cls)
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("unexpected validation error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("a class naming a guest image it cannot get was admitted")
			}
			for _, want := range tc.wantMsg {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err.Error(), want)
				}
			}
		})
	}
}

// TestSandboxClassWebhook_GuestImageRefusedOnUpdateToo. Admission on create
// alone would let an operator add the field to a live class and still believe
// the pin took, which is the exact state this change exists to end.
func TestSandboxClassWebhook_GuestImageRefusedOnUpdateToo(t *testing.T) {
	t.Parallel()

	old := mkSandboxClass("gi-upd", "", mkRuntime(setecruntime.BackendKataFC))
	updated := mkSandboxClass("gi-upd", "", mkRuntime(setecruntime.BackendKataFC))
	updated.Spec.KernelImage = "ghcr.io/org/hardened-kernel:v2"

	w := webhookWith(fakeClientWithNS(t, gateNamespaceUnlabelled()), baseConfig())
	if _, err := w.ValidateUpdate(context.Background(), old, updated); err == nil {
		t.Fatal("a live class was updated to name a guest image it cannot get")
	}
}

// TestSandboxClassWebhook_GuestImageRefusedWithNilRuntime. The check must sit
// above the Runtime nil-return, which short-circuits validate() when the
// defaulting webhook has not run (kubectl --dry-run, webhooks bypassed).
// Several earlier rules had to be moved for exactly that reason.
func TestSandboxClassWebhook_GuestImageRefusedWithNilRuntime(t *testing.T) {
	t.Parallel()

	cls := mkSandboxClass("gi-nil", "", nil)
	cls.Spec.RootfsImage = "ghcr.io/org/minimal-rootfs:v3"

	w := webhookWith(fakeClientWithNS(t, gateNamespaceUnlabelled()), baseConfig())
	_, err := w.ValidateCreate(context.Background(), cls)
	if err == nil {
		t.Fatal("a class with no Runtime block smuggled a guest image through")
	}
	if !strings.Contains(err.Error(), "spec.rootfsImage") {
		t.Errorf("error %q does not name the field", err.Error())
	}
}

// TestGuestImageFieldsHaveNoReader is the premise, asserted rather than
// believed. If someone wires either field into a controller or the podspec
// builder, this test fails and tells them to delete the refusal above instead
// of leaving two contradictory behaviours in the tree.
func TestGuestImageFieldsHaveNoReader(t *testing.T) {
	t.Parallel()

	readers := goFilesReading(t, "../..", "KernelImage", "RootfsImage")
	// The declaration, the generated deepcopy, the refusal and this test are
	// the only places the names may appear.
	allowed := map[string]bool{
		"api/v1alpha1/sandboxclass_types.go":               true,
		"api/v1alpha1/zz_generated.deepcopy.go":            true,
		"internal/webhook/sandboxclass_webhook.go":         true,
		"internal/webhook/sandboxclass_guestimage_test.go": true,
	}
	for _, f := range readers {
		if !allowed[f] {
			t.Errorf("%s reads KernelImage or RootfsImage. If it now honors the field, "+
				"delete validateGuestImages and its tests; a field that is both honored "+
				"and refused is worse than either", f)
		}
	}
	// A floor: the declaration and the deepcopy must always be found, or the
	// walk measured nothing and this test would pass by looking at no files.
	if len(readers) < 2 {
		t.Fatalf("found %d file(s) naming the fields; the walk is not reaching the tree", len(readers))
	}
}

// goFilesReading walks root and returns the repo-relative path of every .go
// file naming any of the given identifiers. Vendor, bin and the worktree
// directory are skipped; a sibling worktree is another checkout of this repo and
// counting it would make the result depend on what else is checked out.
func goFilesReading(t *testing.T, root string, names ...string) []string {
	t.Helper()

	abs, err := filepath.Abs(root)
	if err != nil {
		t.Fatalf("resolve %s: %v", root, err)
	}
	var out []string
	err = filepath.WalkDir(abs, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", ".worktrees", "bin", "vendor", "node_modules", "testdata":
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".go") {
			return nil
		}
		// #nosec G304 -- the path comes from walking this repository's own tree
		// from a path fixed in the caller; there is no external input.
		b, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		for _, n := range names {
			if strings.Contains(string(b), n) {
				rel, relErr := filepath.Rel(abs, path)
				if relErr != nil {
					return relErr
				}
				out = append(out, filepath.ToSlash(rel))
				return nil
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", abs, err)
	}
	return out
}
