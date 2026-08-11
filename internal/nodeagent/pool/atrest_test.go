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

package pool

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
	"github.com/zeroroot-ai/setec/internal/nodeagent/poolentry"
)

// TestLaunchOptions_CarriesNoSnapshotSource codifies ADR-0005
// invariant 4 at its source, in the style of
// TestLaunchOptions_CarriesNoSecretMaterial: the pool launch surface
// must never grow a field that could point the builder at an existing
// snapshot, template, or used sandbox. A pool entry is built from the
// class-image boot path (kernel/rootfs/image) and nothing else.
func TestLaunchOptions_CarriesNoSnapshotSource(t *testing.T) {
	forbidden := []string{"snapshot", "restore", "template", "clone"}
	typ := reflect.TypeFor[LaunchOptions]()
	for field := range typ.Fields() {
		name := strings.ToLower(field.Name)
		for _, bad := range forbidden {
			if strings.Contains(name, bad) {
				t.Fatalf("LaunchOptions.%s looks like a snapshot/template source; the pool builder "+
					"only ever cold-boots from the class image (ADR-0005 invariant 4)", field.Name)
			}
		}
	}
}

// TestClaim_RefusesEntryWithoutProvenance codifies ADR-0005 invariant 4
// at the hand-over boundary: a pool entry whose on-disk provenance
// record is missing is never handed out — it is torn down and the
// claim falls through to "no entry".
func TestClaim_RefusesEntryWithoutProvenance(t *testing.T) {
	s := newFakeStorage()
	pre := &countingPrefetcher{}
	fc := &fakeFirecracker{}
	m := newTestManager(s, pre, fc, 4)

	cls := newClass("img:v1", 1, 0)
	if err := m.ReconcilePools(context.Background(), []setecv1alpha1.SandboxClass{cls}); err != nil {
		t.Fatalf("ReconcilePools: %v", err)
	}

	// Strip the provenance record, simulating an artifact of unknown
	// origin sitting in the pool directory.
	entries := m.QueryAvailable("std", "")
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	dir := entries[0].StorageRef
	if err := os.Remove(filepath.Join(dir, poolentry.ProvenanceFile)); err != nil {
		t.Fatalf("remove provenance: %v", err)
	}

	_, ok, err := m.Claim(context.Background(), "std", "img:v1")
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if ok {
		t.Fatal("Claim must refuse an entry without a provenance record")
	}
	// The refused entry must be destroyed, not returned to the pool.
	if m.CountClass("std") != 0 {
		t.Fatalf("refused entry left in pool: count=%d", m.CountClass("std"))
	}
	if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
		t.Fatalf("refused entry dir should be removed: %v", statErr)
	}
}

// TestClaim_RefusesForeignSourceProvenance: a provenance record that
// names any source other than the class-image boot path (e.g. a used
// sandbox promoted into the pool directory) is refused.
func TestClaim_RefusesForeignSourceProvenance(t *testing.T) {
	s := newFakeStorage()
	pre := &countingPrefetcher{}
	fc := &fakeFirecracker{}
	m := newTestManager(s, pre, fc, 4)

	cls := newClass("img:v1", 1, 0)
	if err := m.ReconcilePools(context.Background(), []setecv1alpha1.SandboxClass{cls}); err != nil {
		t.Fatalf("ReconcilePools: %v", err)
	}
	entries := m.QueryAvailable("std", "")
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	dir := entries[0].StorageRef
	if err := poolentry.WriteProvenance(dir, poolentry.Provenance{
		Source:   "used-sandbox",
		ImageRef: "img:v1",
	}); err != nil {
		t.Fatalf("write forged provenance: %v", err)
	}

	_, ok, err := m.Claim(context.Background(), "std", "img:v1")
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if ok {
		t.Fatal("Claim must refuse a non-class-image-boot provenance source")
	}
	if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
		t.Fatalf("refused entry dir should be removed: %v", statErr)
	}
}

// TestRelease_DestroysSealedDEK asserts the crypto-erase side of
// invariant 5: releasing an entry removes its sealed DEK together with
// the state files.
func TestRelease_DestroysSealedDEK(t *testing.T) {
	s := newFakeStorage()
	pre := &countingPrefetcher{}
	fc := &fakeFirecracker{}
	m := newTestManager(s, pre, fc, 4)

	cls := newClass("img:v1", 1, 0)
	if err := m.ReconcilePools(context.Background(), []setecv1alpha1.SandboxClass{cls}); err != nil {
		t.Fatalf("ReconcilePools: %v", err)
	}
	entries := m.QueryAvailable("std", "")
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	e := entries[0]
	dekPath := filepath.Join(e.StorageRef, poolentry.DEKFile)
	if _, err := os.Stat(dekPath); err != nil {
		t.Fatalf("pre-release sealed DEK should exist: %v", err)
	}

	if err := m.Release(context.Background(), e.ID); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if _, err := os.Stat(dekPath); !os.IsNotExist(err) {
		t.Fatalf("sealed DEK must be destroyed on release: %v", err)
	}
	if _, err := os.Stat(e.StorageRef); !os.IsNotExist(err) {
		t.Fatalf("entry dir must be removed on release: %v", err)
	}
}
