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
	"errors"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
	"github.com/zeroroot-ai/setec/internal/firecracker"
	"github.com/zeroroot-ai/setec/internal/snapshot/storage"
)

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "setec-pool-test-")
	if err != nil {
		panic(err)
	}
	testPoolRoot = dir
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// fakeFirecracker records API invocations; default methods succeed.
type fakeFirecracker struct {
	mu                  sync.Mutex
	pauseCalls          int
	resumeCalls         int
	createSnapshotCalls int
	loadSnapshotCalls   int
	pauseErr            error
}

func (f *fakeFirecracker) Pause(_ context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pauseCalls++
	return f.pauseErr
}
func (f *fakeFirecracker) Resume(_ context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resumeCalls++
	return nil
}
func (f *fakeFirecracker) CreateSnapshot(_ context.Context, _, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createSnapshotCalls++
	return nil
}
func (f *fakeFirecracker) LoadSnapshot(_ context.Context, _, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loadSnapshotCalls++
	return nil
}

// fakeStorage satisfies storage.StorageBackend with in-memory state.
type fakeStorage struct {
	mu      sync.Mutex
	saves   []string
	deletes []string
	data    map[string][]byte
}

func newFakeStorage() *fakeStorage { return &fakeStorage{data: map[string][]byte{}} }

func (s *fakeStorage) Save(_ context.Context, id string, _ io.Reader) (int64, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.data[id]; exists {
		return 0, "", storage.ErrAlreadyExists
	}
	s.data[id] = []byte{}
	s.saves = append(s.saves, id)
	return 0, id, nil
}
func (s *fakeStorage) Open(_ context.Context, id string) (io.ReadCloser, error) {
	return nil, errors.New("not used")
}
func (s *fakeStorage) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.data[id]; !ok {
		return storage.ErrNotFound
	}
	delete(s.data, id)
	s.deletes = append(s.deletes, id)
	return nil
}
func (s *fakeStorage) Stat(_ context.Context, id string) (int64, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.data[id]
	return 0, ok, nil
}

// countingPrefetcher records prefetch invocations.
type countingPrefetcher struct{ n atomic.Int32 }

func (c *countingPrefetcher) Prefetch(_ context.Context, _ []string) error {
	c.n.Add(1)
	return nil
}

// fakeLauncher records Launch invocations and, on success, calls the
// configured firecracker.Client's Pause + CreateSnapshot so the
// existing tests that assert on fakeFirecracker counters continue to
// work. The fake writes non-zero state/memory files under the entry
// directory so releaseEntry's filesystem cleanup is exercised end to
// end.
type fakeLauncher struct {
	factory FirecrackerFactory
	err     error
	mu      sync.Mutex
	calls   int
	opts    []LaunchOptions
}

func (l *fakeLauncher) Launch(ctx context.Context, opts LaunchOptions) error {
	l.mu.Lock()
	l.calls++
	l.opts = append(l.opts, opts)
	l.mu.Unlock()
	if l.err != nil {
		return l.err
	}
	fc := l.factory(opts.SocketPath)
	if err := fc.Pause(ctx); err != nil {
		return err
	}
	// Create the entry directory so release cleanup has something to remove.
	entryDir := opts.StorageRoot + "/" + opts.EntryID
	if err := os.MkdirAll(entryDir, 0o750); err != nil {
		return err
	}
	statePath := entryDir + "/state.bin"
	memPath := entryDir + "/memory.bin"
	if err := fc.CreateSnapshot(ctx, statePath, memPath); err != nil {
		return err
	}
	_ = os.WriteFile(statePath, []byte("state"), 0o644)
	_ = os.WriteFile(memPath, []byte("mem"), 0o644)
	return nil
}

// newTestManager assembles a Manager wired to the provided fakes.
func newTestManager(storageBackend storage.StorageBackend, pre *countingPrefetcher, fc *fakeFirecracker, concurrentBoots int) *Manager {
	return newTestManagerWithFactory(storageBackend, pre, func(_ string) firecracker.Client { return fc }, concurrentBoots)
}

// newTestManagerWithFactory exposes the firecracker factory override so
// tests that need to intercept per-VM clients (e.g. concurrency)
// can supply their own.
func newTestManagerWithFactory(storageBackend storage.StorageBackend, pre *countingPrefetcher, ff FirecrackerFactory, concurrentBoots int) *Manager {
	m := New(storageBackend, pre, ff, "node-a")
	if concurrentBoots > 0 {
		m.MaxConcurrentBoots = concurrentBoots
	}
	// Point pool storage at an ephemeral path; tests that care about
	// the value override it.
	m.PoolStorageRoot = testPoolRoot
	m.KernelPath = "/nonexistent/vmlinux"
	m.RootfsPath = "/nonexistent/rootfs"
	m.Launcher = &fakeLauncher{factory: ff}
	return m
}

// testPoolRoot must stay writable for the duration of the test binary.
// Using a package-level t.TempDir is not possible; we use /tmp and
// a per-process subdirectory that the tests clean up in TestMain.
var testPoolRoot string

func newClass(image string, size int32, ttl time.Duration) setecv1alpha1.SandboxClass {
	var ttlPtr *metav1.Duration
	if ttl > 0 {
		ttlPtr = &metav1.Duration{Duration: ttl}
	}
	return setecv1alpha1.SandboxClass{
		ObjectMeta: metav1.ObjectMeta{Name: "std"},
		Spec: setecv1alpha1.SandboxClassSpec{
			VMM:             setecv1alpha1.VMMFirecracker,
			PreWarmPoolSize: size,
			PreWarmImage:    image,
			PreWarmTTL:      ttlPtr,
		},
	}
}

func TestReconcile_BootsNEntries(t *testing.T) {
	s := newFakeStorage()
	pre := &countingPrefetcher{}
	fc := &fakeFirecracker{}
	m := newTestManager(s, pre, fc, 4)
	fakeL := m.Launcher.(*fakeLauncher)

	cls := newClass("ghcr.io/org/app:v1", 3, 0)
	if err := m.ReconcilePools(context.Background(), []setecv1alpha1.SandboxClass{cls}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got := m.CountClass("std"); got != 3 {
		t.Fatalf("count = %d, want 3", got)
	}
	if got := pre.n.Load(); got != 1 {
		t.Fatalf("prefetch count = %d, want 1", got)
	}
	fakeL.mu.Lock()
	calls := fakeL.calls
	fakeL.mu.Unlock()
	if calls != 3 {
		t.Fatalf("launcher calls = %d, want 3", calls)
	}
	// StorageBackend must remain untouched — pool boots no longer
	// create placeholder saves there.
	if len(s.saves) != 0 {
		t.Fatalf("storage backend should see zero saves from pool boots, got %d", len(s.saves))
	}
}

func TestReconcile_NoOpWhenAlreadyAtSize(t *testing.T) {
	s := newFakeStorage()
	pre := &countingPrefetcher{}
	fc := &fakeFirecracker{}
	m := newTestManager(s, pre, fc, 4)
	fakeL := m.Launcher.(*fakeLauncher)

	cls := newClass("ghcr.io/org/app:v1", 2, time.Hour)
	_ = m.ReconcilePools(context.Background(), []setecv1alpha1.SandboxClass{cls})
	fakeL.mu.Lock()
	calls1 := fakeL.calls
	fakeL.mu.Unlock()
	_ = m.ReconcilePools(context.Background(), []setecv1alpha1.SandboxClass{cls})
	fakeL.mu.Lock()
	calls2 := fakeL.calls
	fakeL.mu.Unlock()
	if calls2 != calls1 {
		t.Fatalf("second reconcile should not launch more: %d -> %d", calls1, calls2)
	}
	if m.CountClass("std") != 2 {
		t.Fatalf("count should remain 2")
	}
}

func TestReconcile_ScaleDownReleasesExcess(t *testing.T) {
	s := newFakeStorage()
	pre := &countingPrefetcher{}
	fc := &fakeFirecracker{}
	m := newTestManager(s, pre, fc, 4)

	cls := newClass("ghcr.io/org/app:v1", 3, 0)
	_ = m.ReconcilePools(context.Background(), []setecv1alpha1.SandboxClass{cls})

	// Capture the entry directories so we can assert they are removed
	// by the scale-down.
	avail := m.QueryAvailable("std", "")
	refs := make([]string, 0, len(avail))
	for _, e := range avail {
		refs = append(refs, e.StorageRef)
	}

	cls.Spec.PreWarmPoolSize = 1
	if err := m.ReconcilePools(context.Background(), []setecv1alpha1.SandboxClass{cls}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if m.CountClass("std") != 1 {
		t.Fatalf("after scale-down count = %d, want 1", m.CountClass("std"))
	}
	// Exactly two entry directories must have been removed; the
	// surviving one is still present on disk.
	removed := 0
	for _, r := range refs {
		if _, err := os.Stat(r); os.IsNotExist(err) {
			removed++
		}
	}
	if removed != 2 {
		t.Fatalf("removed entry dirs = %d, want 2 (refs=%v)", removed, refs)
	}
}

func TestReconcile_ScaleUpIncrements(t *testing.T) {
	s := newFakeStorage()
	pre := &countingPrefetcher{}
	fc := &fakeFirecracker{}
	m := newTestManager(s, pre, fc, 2)

	cls := newClass("ghcr.io/org/app:v1", 1, 0)
	_ = m.ReconcilePools(context.Background(), []setecv1alpha1.SandboxClass{cls})
	cls.Spec.PreWarmPoolSize = 3
	if err := m.ReconcilePools(context.Background(), []setecv1alpha1.SandboxClass{cls}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if m.CountClass("std") != 3 {
		t.Fatalf("count = %d, want 3", m.CountClass("std"))
	}
}

func TestReconcile_ConcurrencyBounded(t *testing.T) {
	s := newFakeStorage()
	pre := &countingPrefetcher{}
	fc := &fakeFirecracker{}

	// Add a slow-pause behavior so the cap is observable.
	var active atomic.Int32
	var peak atomic.Int32
	fc.pauseErr = nil
	factory := func(_ string) firecracker.Client {
		return &instrumentedFC{fc: fc, active: &active, peak: &peak}
	}
	m := newTestManagerWithFactory(s, pre, factory, 2)

	cls := newClass("ghcr.io/org/app:v1", 8, 0)
	if err := m.ReconcilePools(context.Background(), []setecv1alpha1.SandboxClass{cls}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if peak.Load() > 2 {
		t.Fatalf("peak concurrent boots = %d, exceeded cap 2", peak.Load())
	}
}

// instrumentedFC measures the number of concurrent Pause calls,
// which is the proxy for "currently booting" in bootOne. Each Pause
// briefly holds a counter so the test can assert the MaxConcurrent
// boundary is respected.
type instrumentedFC struct {
	fc     *fakeFirecracker
	active *atomic.Int32
	peak   *atomic.Int32
}

func (i *instrumentedFC) Pause(ctx context.Context) error {
	cur := i.active.Add(1)
	for {
		p := i.peak.Load()
		if cur <= p || i.peak.CompareAndSwap(p, cur) {
			break
		}
	}
	defer i.active.Add(-1)
	time.Sleep(5 * time.Millisecond)
	return i.fc.Pause(ctx)
}
func (i *instrumentedFC) Resume(ctx context.Context) error { return i.fc.Resume(ctx) }
func (i *instrumentedFC) CreateSnapshot(ctx context.Context, s, mfp string) error {
	return i.fc.CreateSnapshot(ctx, s, mfp)
}
func (i *instrumentedFC) LoadSnapshot(ctx context.Context, s, mfp string) error {
	return i.fc.LoadSnapshot(ctx, s, mfp)
}

func TestReconcile_TTLRecycling(t *testing.T) {
	s := newFakeStorage()
	pre := &countingPrefetcher{}
	fc := &fakeFirecracker{}
	m := newTestManager(s, pre, fc, 4)
	fakeL := m.Launcher.(*fakeLauncher)

	// Fix clock; short TTL.
	now := time.Now()
	m.SetClock(func() time.Time { return now })

	cls := newClass("ghcr.io/org/app:v1", 2, 10*time.Minute)
	if err := m.ReconcilePools(context.Background(), []setecv1alpha1.SandboxClass{cls}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if m.CountClass("std") != 2 {
		t.Fatalf("seed count = %d, want 2", m.CountClass("std"))
	}
	fakeL.mu.Lock()
	seededCalls := fakeL.calls
	fakeL.mu.Unlock()

	// Capture the entry dirs before the TTL roll so we can assert they
	// are gone after the recycle.
	availPre := m.QueryAvailable("std", "")
	preDirs := make([]string, 0, len(availPre))
	for _, e := range availPre {
		preDirs = append(preDirs, e.StorageRef)
	}

	// Advance the clock past TTL.
	m.SetClock(func() time.Time { return now.Add(30 * time.Minute) })
	if err := m.ReconcilePools(context.Background(), []setecv1alpha1.SandboxClass{cls}); err != nil {
		t.Fatalf("Reconcile(TTL): %v", err)
	}
	if m.CountClass("std") != 2 {
		t.Fatalf("post-TTL count = %d, want 2 (should be reprovisioned)", m.CountClass("std"))
	}
	fakeL.mu.Lock()
	postCalls := fakeL.calls
	fakeL.mu.Unlock()
	if postCalls <= seededCalls {
		t.Fatalf("launches after recycle = %d, want > %d", postCalls, seededCalls)
	}
	for _, d := range preDirs {
		if _, err := os.Stat(d); !os.IsNotExist(err) {
			t.Fatalf("expired entry dir %q should be removed: %v", d, err)
		}
	}
}

func TestClaim_ReturnsEntryAndRemoves(t *testing.T) {
	s := newFakeStorage()
	pre := &countingPrefetcher{}
	fc := &fakeFirecracker{}
	m := newTestManager(s, pre, fc, 4)

	cls := newClass("img:v1", 2, 0)
	_ = m.ReconcilePools(context.Background(), []setecv1alpha1.SandboxClass{cls})

	e, ok, err := m.Claim(context.Background(), "std", "img:v1")
	if err != nil || !ok {
		t.Fatalf("Claim: %v %v", ok, err)
	}
	if e.ID == "" {
		t.Fatalf("claimed entry has empty ID")
	}
	if m.CountClass("std") != 1 {
		t.Fatalf("count after Claim = %d, want 1", m.CountClass("std"))
	}
}

func TestClaim_NoMatch(t *testing.T) {
	s := newFakeStorage()
	pre := &countingPrefetcher{}
	fc := &fakeFirecracker{}
	m := newTestManager(s, pre, fc, 4)

	_, ok, err := m.Claim(context.Background(), "ghost", "img")
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if ok {
		t.Fatalf("expected ok=false when nothing matches")
	}
}

func TestClaim_WrongImageMisses(t *testing.T) {
	s := newFakeStorage()
	pre := &countingPrefetcher{}
	fc := &fakeFirecracker{}
	m := newTestManager(s, pre, fc, 4)

	cls := newClass("img:v1", 1, 0)
	_ = m.ReconcilePools(context.Background(), []setecv1alpha1.SandboxClass{cls})

	_, ok, _ := m.Claim(context.Background(), "std", "other:v2")
	if ok {
		t.Fatalf("Claim should miss on image mismatch")
	}
}

func TestRelease_DeletesStorage(t *testing.T) {
	s := newFakeStorage()
	pre := &countingPrefetcher{}
	fc := &fakeFirecracker{}
	m := newTestManager(s, pre, fc, 4)

	cls := newClass("img:v1", 1, 0)
	_ = m.ReconcilePools(context.Background(), []setecv1alpha1.SandboxClass{cls})
	e, _, _ := m.Claim(context.Background(), "std", "img:v1")

	// Re-add the claimed entry to the pool so Release can find it.
	m.mu.Lock()
	m.state[e.ClassName] = append(m.state[e.ClassName], &e)
	m.mu.Unlock()

	if _, err := os.Stat(e.StorageRef); err != nil {
		t.Fatalf("pre-release StorageRef should exist: %v", err)
	}
	if err := m.Release(context.Background(), e.ID); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if _, err := os.Stat(e.StorageRef); !os.IsNotExist(err) {
		t.Fatalf("StorageRef dir should be removed, got: %v", err)
	}
}

func TestRelease_NotFound(t *testing.T) {
	m := newTestManager(newFakeStorage(), &countingPrefetcher{}, &fakeFirecracker{}, 4)
	if err := m.Release(context.Background(), "ghost"); err == nil {
		t.Fatalf("expected not-found error")
	}
}

func TestReconcile_DropsClassNotListed(t *testing.T) {
	s := newFakeStorage()
	pre := &countingPrefetcher{}
	fc := &fakeFirecracker{}
	m := newTestManager(s, pre, fc, 4)

	cls := newClass("img:v1", 2, 0)
	_ = m.ReconcilePools(context.Background(), []setecv1alpha1.SandboxClass{cls})
	if m.CountClass("std") != 2 {
		t.Fatalf("pre drop: %d", m.CountClass("std"))
	}
	// Reconcile with empty list — pool must drain.
	if err := m.ReconcilePools(context.Background(), nil); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if m.CountClass("std") != 0 {
		t.Fatalf("post drop: %d", m.CountClass("std"))
	}
}

func TestReconcile_RejectsPoolWithoutImage(t *testing.T) {
	s := newFakeStorage()
	pre := &countingPrefetcher{}
	fc := &fakeFirecracker{}
	m := newTestManager(s, pre, fc, 4)

	cls := newClass("", 1, 0) // empty image but size>0
	err := m.ReconcilePools(context.Background(), []setecv1alpha1.SandboxClass{cls})
	if err == nil {
		t.Fatalf("expected error on missing image")
	}
}

func TestQueryAvailable_FiltersAndCopies(t *testing.T) {
	s := newFakeStorage()
	pre := &countingPrefetcher{}
	fc := &fakeFirecracker{}
	m := newTestManager(s, pre, fc, 4)

	cls := newClass("img:v1", 3, 0)
	_ = m.ReconcilePools(context.Background(), []setecv1alpha1.SandboxClass{cls})

	got := m.QueryAvailable("std", "")
	if len(got) != 3 {
		t.Fatalf("QueryAvailable = %d, want 3", len(got))
	}
	filtered := m.QueryAvailable("std", "other:v9")
	if len(filtered) != 0 {
		t.Fatalf("filtered = %d, want 0", len(filtered))
	}
	// Ensure the returned slice is a copy — mutating it does not
	// disturb internal state.
	got[0].ID = "MUTATED"
	if m.QueryAvailable("std", "")[0].ID == "MUTATED" {
		t.Fatalf("QueryAvailable returned live internal state")
	}
}

func TestSize(t *testing.T) {
	s := newFakeStorage()
	pre := &countingPrefetcher{}
	fc := &fakeFirecracker{}
	m := newTestManager(s, pre, fc, 4)
	if m.Size() != 0 {
		t.Fatalf("fresh Size = %d, want 0", m.Size())
	}
	cls := newClass("img:v1", 2, 0)
	_ = m.ReconcilePools(context.Background(), []setecv1alpha1.SandboxClass{cls})
	if m.Size() != 2 {
		t.Fatalf("Size = %d, want 2", m.Size())
	}
}
