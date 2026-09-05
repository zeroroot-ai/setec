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

// Package pool maintains the per-node pre-warmed pool of paused
// Firecracker microVMs that power Phase 3's sub-100ms cold starts.
//
// The Manager lives entirely on the node-agent side: it talks to the
// local Firecracker sockets and the snapshot storage backend but
// never to the Kubernetes API server directly. The operator observes
// pool availability via the QueryPool gRPC call and the
// setec_prewarm_pool_entries metric.
package pool

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
	"github.com/zeroroot-ai/setec/internal/firecracker"
	"github.com/zeroroot-ai/setec/internal/nodeagent/poolentry"
	"github.com/zeroroot-ai/setec/internal/snapshot/atrest"
	"github.com/zeroroot-ai/setec/internal/snapshot/storage"
	"github.com/zeroroot-ai/setec/internal/uniquify"
)

// ImagePrefetcher is the narrow hook the pool uses to ensure the
// pre-warm image is present on the node before booting a pool entry.
// The node-agent wires this to the existing imagecache.ImageCache.
type ImagePrefetcher interface {
	Prefetch(ctx context.Context, refs []string) error
}

// FirecrackerFactory constructs a Firecracker client for the given
// socket path. In production this is firecracker.NewClientFromSocket;
// tests inject a mock.
type FirecrackerFactory func(socketPath string) firecracker.Client

// Entry is a single paused microVM the node holds ready for on-demand
// restore.
type Entry struct {
	// ID is the locally-unique identifier of the entry (UUID).
	ID string
	// ClassName is the SandboxClass this entry was provisioned for.
	ClassName string
	// ImageRef is the OCI image baked into the entry.
	ImageRef string
	// KataSocket is the Firecracker API socket path for the entry.
	// Populated by whoever booted the entry; kept for the eventual
	// restore-or-tear-down call.
	KataSocket string
	// StorageRef is the snapshot backend reference for the state
	// files the entry was produced from. Populated after a pool boot
	// that issued CreateSnapshot; empty for purely-paused entries
	// that never hit disk.
	StorageRef string
	// PausedAt is when the entry entered the paused state; the TTL
	// recycler uses this.
	PausedAt time.Time
	// GuestCID is the vsock context id the entry's guest was booted
	// with, allocated node-locally so any two entries differ
	// (ADR-0005 invariant 2). Zero when the Manager runs without a
	// CIDAllocator (tests, legacy wiring).
	GuestCID uint32
}

// Manager maintains the pool state for one node. The struct is safe
// for concurrent use: ReconcilePools / Claim / Release can run from
// different goroutines (the gRPC server and the reconciliation tick)
// without external synchronization.
type Manager struct {
	// Storage is the snapshot storage backend shared with the
	// Coordinator's RPCs. The pool Manager does not write pool-entry
	// state through this backend (pool entries live under
	// PoolStorageRoot); Storage is retained here so future phases that
	// promote pool entries into first-class Snapshots have the hook
	// available.
	Storage storage.StorageBackend

	// ImageCache prefetches pre-warm images so pool boots do not
	// stampede the registry.
	ImageCache ImagePrefetcher

	// FirecrackerFactory builds Firecracker clients per entry.
	FirecrackerFactory FirecrackerFactory

	// Launcher boots individual pool entries. In production this is
	// pool.DefaultExecLauncher() which shells out to setec-pool-vm;
	// tests inject a fake. A nil Launcher falls back to a no-op that
	// errors — tests must set one explicitly.
	Launcher Launcher

	// NodeName is the node this Manager runs on. Used for metric
	// labels and for the pool-entry naming convention.
	NodeName string

	// MaxConcurrentBoots bounds concurrent pre-warm VM boots to avoid
	// overwhelming the node when a large pool is first created.
	// Defaults to 4 when zero.
	MaxConcurrentBoots int

	// SocketPattern renders an API-socket path for a given entry ID.
	// Defaults to "/run/kata-containers/pool-%s/firecracker.socket".
	SocketPattern string

	// PoolStorageRoot is the on-node directory under which the
	// launcher writes paused-VM state. Defaults to
	// "/var/lib/setec/pool".
	PoolStorageRoot string

	// KernelPath and RootfsPath are forwarded to the launcher as
	// sensible node-wide defaults; per-class overrides are a future
	// enhancement.
	KernelPath string
	RootfsPath string

	// DefaultVCPUs and DefaultMemoryMiB are the resource sizes used
	// for pool entries when a SandboxClass does not override.
	DefaultVCPUs     int
	DefaultMemoryMiB int

	// CIDs is the node-local vsock CID authority shared with the gRPC
	// server. When set, every pool entry boots with a freshly
	// allocated guest CID so no two entries (and no two sandboxes
	// warm-started from them) can collide (ADR-0005 invariant 2). A
	// claimed entry's CID registration is released at Claim time; the
	// restore path re-registers it to the owning sandbox after the
	// guest confirms it. nil disables allocation (launcher default
	// CID applies).
	CIDs *uniquify.CIDAllocator

	// clockFn returns the current time. Exposed for tests so they
	// can drive TTL recycling deterministically.
	clockFn func() time.Time

	mu    sync.Mutex
	state map[string][]*Entry // class name -> entries
}

// New returns a Manager with sensible defaults. The returned Manager
// has no Launcher set; callers MUST set one (DefaultExecLauncher in
// production, a fake in tests) before ReconcilePools is invoked.
func New(storageBackend storage.StorageBackend, cache ImagePrefetcher, ff FirecrackerFactory, nodeName string) *Manager {
	return &Manager{
		Storage:            storageBackend,
		ImageCache:         cache,
		FirecrackerFactory: ff,
		NodeName:           nodeName,
		MaxConcurrentBoots: 4,
		SocketPattern:      "/run/kata-containers/pool-%s/firecracker.socket",
		PoolStorageRoot:    "/var/lib/setec/pool",
		DefaultVCPUs:       1,
		DefaultMemoryMiB:   512,
		clockFn:            time.Now,
		state:              map[string][]*Entry{},
	}
}

// SetClock overrides the clock used for TTL calculations (used by
// tests).
func (m *Manager) SetClock(fn func() time.Time) {
	m.clockFn = fn
}

// QueryAvailable returns a copy of the entries for the given class
// that still match the optional image filter. Used by the gRPC
// QueryPool handler.
func (m *Manager) QueryAvailable(className, imageRef string) []Entry {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Entry
	for _, e := range m.state[className] {
		if imageRef != "" && e.ImageRef != imageRef {
			continue
		}
		out = append(out, *e)
	}
	return out
}

// ReconcilePools ensures the pool state matches the declared
// PreWarmPoolSize for each class:
//   - classes with pool size > 0 get topped up to N entries
//   - classes with pool size < current entries get pruned
//   - entries older than PreWarmTTL are recycled (Released and
//     replaced on the next cycle)
//
// The function is resilient: failures on individual entries are
// logged-via-error-return but do not block reconciliation of other
// classes.
func (m *Manager) ReconcilePools(ctx context.Context, classes []setecv1alpha1.SandboxClass) error {
	var firstErr error

	for i := range classes {
		cls := &classes[i]
		if err := m.reconcileClass(ctx, cls); err != nil && firstErr == nil {
			firstErr = err
		}
	}

	// Any class that used to have entries but is no longer in the
	// input list (or whose pool was disabled) must be fully pruned.
	m.mu.Lock()
	known := make(map[string]struct{}, len(classes))
	for i := range classes {
		known[classes[i].Name] = struct{}{}
	}
	var toDrain []string
	for name := range m.state {
		if _, ok := known[name]; !ok {
			toDrain = append(toDrain, name)
		}
	}
	m.mu.Unlock()

	for _, name := range toDrain {
		entries := m.drainClass(name)
		for _, e := range entries {
			if err := m.releaseEntry(ctx, e, true); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}

	return firstErr
}

// reconcileClass drives a single class to its target pool size,
// recycling entries older than PreWarmTTL along the way.
func (m *Manager) reconcileClass(ctx context.Context, cls *setecv1alpha1.SandboxClass) error {
	target := max(int(cls.Spec.PreWarmPoolSize), 0)
	if target > 0 && cls.Spec.PreWarmImage == "" {
		return fmt.Errorf("pool: class %q has PreWarmPoolSize=%d but no PreWarmImage", cls.Name, target)
	}

	// Step 1: recycle expired entries. An entry older than the
	// configured TTL is released; it will be reprovisioned later in
	// this same call.
	expired := m.popExpired(cls)
	for _, e := range expired {
		if err := m.releaseEntry(ctx, e, true); err != nil {
			return err
		}
	}

	// Step 2: scale down.
	current := m.countClass(cls.Name)
	for current > target {
		e := m.popOne(cls.Name)
		if e == nil {
			break
		}
		if err := m.releaseEntry(ctx, e, true); err != nil {
			return err
		}
		current--
	}

	// Step 3: scale up under a concurrency cap.
	if current < target {
		if err := m.ImageCache.Prefetch(ctx, []string{cls.Spec.PreWarmImage}); err != nil {
			return fmt.Errorf("pool: prefetch %q: %w", cls.Spec.PreWarmImage, err)
		}
		missing := target - current
		if err := m.bootEntries(ctx, cls, missing); err != nil {
			return err
		}
	}

	return nil
}

// bootEntries concurrently boots `count` pool entries for cls under
// the MaxConcurrentBoots cap. Individual boot failures are collected
// but not retried here — the next ReconcilePools tick will try again.
func (m *Manager) bootEntries(ctx context.Context, cls *setecv1alpha1.SandboxClass, count int) error {
	cap := m.MaxConcurrentBoots
	if cap <= 0 {
		cap = 4
	}
	sem := make(chan struct{}, cap)
	var wg sync.WaitGroup
	errs := make(chan error, count)

	for range count {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			if err := m.bootOne(ctx, cls); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

// bootOne creates a single pool entry. It generates a stable ID,
// renders the socket path, delegates the Firecracker spawn + pause +
// snapshot sequence to the configured Launcher (typically
// ExecLauncher running setec-pool-vm), and records the resulting
// entry in memory.
//
// The Launcher is expected to leave the Firecracker process alive,
// paused, and reachable at the socket path rendered from
// m.SocketPattern; the grandchild process outlives the brief
// setec-pool-vm run.
func (m *Manager) bootOne(ctx context.Context, cls *setecv1alpha1.SandboxClass) error {
	if m.Launcher == nil {
		return fmt.Errorf("pool: no Launcher configured")
	}

	id := uuid.New().String()
	sock := fmt.Sprintf(m.SocketPattern, id)
	entryDir := filepath.Join(m.PoolStorageRoot, id)

	vcpus := m.DefaultVCPUs
	if vcpus <= 0 {
		vcpus = 1
	}
	mem := m.DefaultMemoryMiB
	if mem <= 0 {
		mem = 512
	}

	// Allocate a node-unique guest vsock CID for the entry (ADR-0005
	// invariant 2): any two pool entries — and any two sandboxes
	// restored from them — differ by construction.
	var guestCID uint32
	if m.CIDs != nil {
		cid, cidErr := m.CIDs.Allocate("pool/" + id)
		if cidErr != nil {
			return fmt.Errorf("pool: allocate guest CID for %s/%s: %w", cls.Name, id, cidErr)
		}
		guestCID = cid
	}

	opts := LaunchOptionsFrom(cls, id, sock, m.PoolStorageRoot, m.KernelPath, m.RootfsPath, vcpus, mem, guestCID)
	if err := m.Launcher.Launch(ctx, opts); err != nil {
		// The launcher is responsible for its own cleanup. Drop the
		// partial entry directory defensively in case the launcher
		// was killed before its deferred cleanup ran.
		_ = os.RemoveAll(entryDir)
		if m.CIDs != nil && guestCID != 0 {
			m.CIDs.Release(guestCID)
		}
		return fmt.Errorf("pool: launch %s/%s: %w", cls.Name, id, err)
	}

	entry := &Entry{
		ID:         id,
		ClassName:  cls.Name,
		ImageRef:   cls.Spec.PreWarmImage,
		KataSocket: sock,
		StorageRef: entryDir,
		PausedAt:   m.clockFn(),
		GuestCID:   guestCID,
	}
	m.mu.Lock()
	m.state[cls.Name] = append(m.state[cls.Name], entry)
	m.mu.Unlock()
	return nil
}

// Claim atomically removes a single matching entry from the pool and
// returns it. Returns ok=false when no compatible entry exists; the
// caller must fall back to cold boot.
//
// Template provenance (ADR-0005 invariant 4) and the clean-base scan
// verdict (invariant 1) are enforced here, at the hand-over boundary:
// an entry whose on-disk provenance record is missing, unreadable, or
// claims any source other than the class-image boot path — or whose
// secret-scan verdict is missing, incomplete, or not clean — is never
// handed out. It is torn down instead, and the scan continues to the
// next candidate. Entries baked before the scan verdict existed
// therefore drain out of the pool and are rebuilt clean.
func (m *Manager) Claim(ctx context.Context, className, imageRef string) (Entry, bool, error) {
	for {
		e, found := m.popMatching(className, imageRef)
		if !found {
			return Entry{}, false, nil
		}
		err := poolentry.Verify(e.StorageRef, e.ImageRef)
		if err == nil {
			_, err = poolentry.VerifyScan(e.StorageRef)
		}
		if err != nil {
			// Refused entry: destroy it (state, key, socket, CID)
			// rather than returning it to the pool, and keep scanning.
			if relErr := m.releaseEntry(ctx, e, true); relErr != nil {
				return Entry{}, false, fmt.Errorf("pool: refusing entry %s (%v) and teardown failed: %w", e.ID, err, relErr)
			}
			continue
		}
		// Hand the CID registration over to the restore path: the
		// entry leaves the pool, and the gRPC server re-registers the
		// CID to the claiming sandbox once the restored guest
		// confirms it. Allocate never re-issues released values, so
		// nothing can grab the CID in between.
		if m.CIDs != nil && e.GuestCID != 0 {
			m.CIDs.Release(e.GuestCID)
		}
		return *e, true, nil
	}
}

// popMatching removes and returns the first entry for className whose
// image matches the optional filter.
func (m *Manager) popMatching(className, imageRef string) (*Entry, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	entries := m.state[className]
	for i, e := range entries {
		if imageRef != "" && e.ImageRef != imageRef {
			continue
		}
		// Remove in-place, preserving order.
		m.state[className] = append(entries[:i], entries[i+1:]...)
		return e, true
	}
	return nil, false
}

// ReleaseClaimed tears down an entry that was already removed from
// the pool by Claim. Claim detaches the entry from internal state, so
// Release (which looks the entry up by ID) cannot find it; callers
// that consumed an entry — successfully restored or not — use this to
// erase its on-disk state (ADR-0005: pool state is never restored
// twice). The entry's CID registration was already handed over at
// Claim time and is NOT touched here: on a successful restore the
// restored sandbox now owns it.
func (m *Manager) ReleaseClaimed(ctx context.Context, e Entry) error {
	return m.releaseEntry(ctx, &e, false)
}

// Release tears down a claimed or expired entry: it deletes the
// storage backing and resumes the Firecracker VM long enough to
// terminate it. In Phase 3 the termination is the launcher's
// responsibility; the Manager only cleans up its own state.
func (m *Manager) Release(ctx context.Context, entryID string) error {
	e := m.popByID(entryID)
	if e == nil {
		return fmt.Errorf("pool: entry %q not found", entryID)
	}
	return m.releaseEntry(ctx, e, true)
}

// releaseEntry tears down the on-disk artefacts for an entry. The
// paused Firecracker process is signalled by removing its API socket
// file and relying on the surrounding launcher/kata machinery to reap
// the process; the pool Manager's own contract ends at the moment
// the entry directory is gone and the entry is out of internal state.
//
// The function is idempotent: a missing directory or socket is
// treated as success so reconcile retries do not thrash.
//
// Destruction order matters (ADR-0005 invariant 5): the entry's sealed
// DEK is zero-overwritten and unlinked FIRST, cryptographically
// erasing the encrypted state/memory files even if the subsequent
// directory removal is interrupted or the filesystem's overwrite
// semantics are weak (copy-on-write).
//
// freeCID releases the entry's guest-CID registration; it is true for
// entries torn down while still pool-owned (expired, pruned, drained,
// refused) and false for claimed entries, whose CID ownership moved to
// the restored sandbox at Claim time.
func (m *Manager) releaseEntry(_ context.Context, e *Entry, freeCID bool) error {
	if e == nil {
		return nil
	}
	if freeCID && m.CIDs != nil && e.GuestCID != 0 {
		m.CIDs.Release(e.GuestCID)
	}
	if e.StorageRef != "" {
		if err := atrest.Shred(filepath.Join(e.StorageRef, poolentry.DEKFile)); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("pool: destroy sealed DEK for %q: %w", e.ID, err)
		}
		if err := os.RemoveAll(e.StorageRef); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("pool: remove %q: %w", e.StorageRef, err)
		}
	}
	if e.KataSocket != "" {
		if err := os.Remove(e.KataSocket); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("pool: remove socket %q: %w", e.KataSocket, err)
		}
	}
	return nil
}

// Size returns the total number of pool entries across all classes,
// primarily for testing and metric emission.
func (m *Manager) Size() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	total := 0
	for _, entries := range m.state {
		total += len(entries)
	}
	return total
}

// CountClass returns the number of entries for a single class.
func (m *Manager) CountClass(className string) int {
	return m.countClass(className)
}

// FillByClass returns a snapshot of the per-class entry counts.
// Classes whose pools drained to zero in the last reconcile do not
// appear — callers that export gauges should Reset before applying
// the map so stale series disappear with their pools.
func (m *Manager) FillByClass() map[string]int {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]int, len(m.state))
	for className, entries := range m.state {
		out[className] = len(entries)
	}
	return out
}

func (m *Manager) countClass(className string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.state[className])
}

func (m *Manager) popOne(className string) *Entry {
	m.mu.Lock()
	defer m.mu.Unlock()
	entries := m.state[className]
	if len(entries) == 0 {
		return nil
	}
	e := entries[len(entries)-1]
	m.state[className] = entries[:len(entries)-1]
	return e
}

func (m *Manager) popByID(id string) *Entry {
	m.mu.Lock()
	defer m.mu.Unlock()
	for className, entries := range m.state {
		for i, e := range entries {
			if e.ID == id {
				m.state[className] = append(entries[:i], entries[i+1:]...)
				return e
			}
		}
	}
	return nil
}

// popExpired removes and returns entries for cls whose PausedAt is
// older than the class's PreWarmTTL (default 24h when unset).
func (m *Manager) popExpired(cls *setecv1alpha1.SandboxClass) []*Entry {
	ttl := 24 * time.Hour
	if cls.Spec.PreWarmTTL != nil && cls.Spec.PreWarmTTL.Duration > 0 {
		ttl = cls.Spec.PreWarmTTL.Duration
	}
	now := m.clockFn()

	m.mu.Lock()
	defer m.mu.Unlock()
	entries := m.state[cls.Name]
	var kept []*Entry
	var expired []*Entry
	for _, e := range entries {
		if now.Sub(e.PausedAt) >= ttl {
			expired = append(expired, e)
		} else {
			kept = append(kept, e)
		}
	}
	m.state[cls.Name] = kept
	return expired
}

// drainClass removes and returns every entry for className. Used by
// ReconcilePools when a class was dropped from the input set.
func (m *Manager) drainClass(className string) []*Entry {
	m.mu.Lock()
	defer m.mu.Unlock()
	entries := m.state[className]
	delete(m.state, className)
	return entries
}
