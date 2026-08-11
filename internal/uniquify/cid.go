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

package uniquify

import (
	"fmt"
	"math"
	"sync"
)

// FirstGuestCID is the lowest allocatable guest vsock context id.
// CIDs 0-2 are reserved (hypervisor / loopback / host).
const FirstGuestCID uint32 = 3

// CIDAllocator is the node-local vsock context-id authority (ADR-0005
// invariant 2: "a unique vsock CID per restore"). It serves two
// cooperating callers on one node-agent:
//
//   - the pre-warm pool Manager ALLOCATES a fresh CID for every pool
//     VM it boots, so any two pool entries — and therefore any two
//     sandboxes warm-started from the same class pool — carry
//     distinct CIDs by construction;
//   - the gRPC restore path OBSERVES the CID a restored guest reports
//     and registers it to the owning sandbox, failing when another
//     live owner already holds it. This is what catches two restores
//     of the same named Snapshot, whose snapshotted CID is identical
//     in both clones.
//
// Allocation is monotonic and never reuses a released value within
// the allocator's lifetime: a CID freed by a torn-down pool entry is
// not handed out again, so a stale registration can never mask a real
// collision. The 32-bit CID space makes exhaustion a non-issue for a
// node-agent process lifetime.
type CIDAllocator struct {
	mu    sync.Mutex
	next  uint32
	inUse map[uint32]string // cid -> owner
}

// NewCIDAllocator returns an allocator starting at FirstGuestCID.
func NewCIDAllocator() *CIDAllocator {
	return &CIDAllocator{
		next:  FirstGuestCID,
		inUse: map[uint32]string{},
	}
}

// Allocate reserves the next free CID for owner and returns it.
func (a *CIDAllocator) Allocate(owner string) (uint32, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for a.next != math.MaxUint32 {
		cid := a.next
		a.next++
		if _, taken := a.inUse[cid]; taken {
			continue
		}
		a.inUse[cid] = owner
		return cid, nil
	}
	return 0, fmt.Errorf("uniquify: node vsock CID space exhausted")
}

// Observe registers an externally-assigned CID (reported by a
// restored guest) to owner. It fails when the CID is invalid or
// already held by a DIFFERENT live owner — the fail-closed signal
// that two restores would share a CID on this node. Re-observing the
// same cid/owner pair is idempotent.
func (a *CIDAllocator) Observe(cid uint32, owner string) error {
	if cid < FirstGuestCID {
		return fmt.Errorf("uniquify: guest CID %d is in the reserved range", cid)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if holder, taken := a.inUse[cid]; taken && holder != owner {
		return fmt.Errorf("uniquify: vsock CID %d is already active on this node (held by %q)", cid, holder)
	}
	a.inUse[cid] = owner
	return nil
}

// Release frees a CID registration. Releasing an unknown CID is a
// no-op. Note that Allocate never re-issues a released value, so
// Release only affects Observe collision checks.
func (a *CIDAllocator) Release(cid uint32) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.inUse, cid)
}

// Owner reports the current holder of cid, if any.
func (a *CIDAllocator) Owner(cid uint32) (string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	owner, ok := a.inUse[cid]
	return owner, ok
}

// Active returns the number of live registrations (metrics/tests).
func (a *CIDAllocator) Active() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.inUse)
}
