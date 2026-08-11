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
	"testing"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
	"github.com/zeroroot-ai/setec/internal/uniquify"
)

// TestBootEntries_AllocateDistinctGuestCIDs is the ADR-0005
// invariant-2 construction guarantee: every pool entry boots with its
// own node-unique vsock CID, so two sandboxes warm-started from the
// same class pool can never collide.
func TestBootEntries_AllocateDistinctGuestCIDs(t *testing.T) {
	s := newFakeStorage()
	pre := &countingPrefetcher{}
	fc := &fakeFirecracker{}
	m := newTestManager(s, pre, fc, 4)
	m.CIDs = uniquify.NewCIDAllocator()

	cls := newClass("img:1", 3, 0)
	if err := m.ReconcilePools(context.Background(), []setecv1alpha1.SandboxClass{cls}); err != nil {
		t.Fatalf("ReconcilePools: %v", err)
	}

	entries := m.QueryAvailable("std", "")
	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(entries))
	}
	seen := map[uint32]bool{}
	for _, e := range entries {
		if e.GuestCID < uniquify.FirstGuestCID {
			t.Fatalf("entry %s has reserved/zero CID %d", e.ID, e.GuestCID)
		}
		if seen[e.GuestCID] {
			t.Fatalf("two pool entries share CID %d", e.GuestCID)
		}
		seen[e.GuestCID] = true
	}

	// The launcher must have been told the CID for every boot.
	fl := m.Launcher.(*fakeLauncher)
	fl.mu.Lock()
	defer fl.mu.Unlock()
	for _, o := range fl.opts {
		if o.GuestCID == 0 {
			t.Fatalf("launcher was not passed a guest CID: %+v", o)
		}
	}
}

// TestClaim_HandsCIDRegistrationToRestorePath pins the ownership
// handover: Claim frees the pool-side registration so the gRPC server
// can re-register the CID to the claiming sandbox; the allocator's
// monotonic policy prevents any other boot from grabbing it.
func TestClaim_HandsCIDRegistrationToRestorePath(t *testing.T) {
	s := newFakeStorage()
	pre := &countingPrefetcher{}
	fc := &fakeFirecracker{}
	m := newTestManager(s, pre, fc, 4)
	m.CIDs = uniquify.NewCIDAllocator()

	cls := newClass("img:1", 1, 0)
	if err := m.ReconcilePools(context.Background(), []setecv1alpha1.SandboxClass{cls}); err != nil {
		t.Fatalf("ReconcilePools: %v", err)
	}

	entry, ok, err := m.Claim(context.Background(), "std", "")
	if err != nil || !ok {
		t.Fatalf("Claim: ok=%v err=%v", ok, err)
	}
	if entry.GuestCID == 0 {
		t.Fatal("claimed entry lost its guest CID")
	}
	if _, held := m.CIDs.Owner(entry.GuestCID); held {
		t.Fatal("Claim must release the pool-side CID registration for the restore path to adopt")
	}
	// The restore path re-registers it to the sandbox.
	if err := m.CIDs.Observe(entry.GuestCID, "ns/sb-1"); err != nil {
		t.Fatalf("restore path could not adopt the CID: %v", err)
	}
	// ReleaseClaimed (post-restore teardown of the entry state) must
	// NOT free the sandbox's registration.
	if err := m.ReleaseClaimed(context.Background(), entry); err != nil {
		t.Fatalf("ReleaseClaimed: %v", err)
	}
	if owner, held := m.CIDs.Owner(entry.GuestCID); !held || owner != "ns/sb-1" {
		t.Fatalf("ReleaseClaimed must not free the restored sandbox's CID (owner=%q held=%v)", owner, held)
	}
}

// TestRelease_FreesUnclaimedEntryCID: entries torn down while still
// pool-owned (scale-down, TTL, drain) release their registration.
func TestRelease_FreesUnclaimedEntryCID(t *testing.T) {
	s := newFakeStorage()
	pre := &countingPrefetcher{}
	fc := &fakeFirecracker{}
	m := newTestManager(s, pre, fc, 4)
	m.CIDs = uniquify.NewCIDAllocator()

	cls := newClass("img:1", 1, 0)
	if err := m.ReconcilePools(context.Background(), []setecv1alpha1.SandboxClass{cls}); err != nil {
		t.Fatalf("ReconcilePools: %v", err)
	}
	entries := m.QueryAvailable("std", "")
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	cid := entries[0].GuestCID
	if err := m.Release(context.Background(), entries[0].ID); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if _, held := m.CIDs.Owner(cid); held {
		t.Fatal("Release must free an unclaimed entry's CID registration")
	}
}
