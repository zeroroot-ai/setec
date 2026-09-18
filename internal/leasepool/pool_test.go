// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package leasepool

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

// fakeBackend is an in-memory Backend. Launched sandboxes become ready
// immediately unless readyAfter is set; destroyed sandboxes are removed.
type fakeBackend struct {
	mu         sync.Mutex
	seq        atomic.Int64
	live       map[string]bool // id -> alive
	ready      map[string]bool // id -> ready
	launches   atomic.Int64
	destroys   atomic.Int64
	launchErr  error
	readyAfter map[string]int // id -> remaining not-ready polls
	// launchGate, when set, blocks every Launch until it is closed and
	// reports each arrival on launchArrived first. Tests use it to hold
	// concurrent Replenish calls at the backend boundary.
	launchGate    chan struct{}
	launchArrived chan struct{}
}

func newFakeBackend() *fakeBackend {
	return &fakeBackend{
		live:       map[string]bool{},
		ready:      map[string]bool{},
		readyAfter: map[string]int{},
	}
}

func (f *fakeBackend) Launch(_ context.Context, tmpl PoolTemplate) (SandboxRef, error) {
	if f.launchErr != nil {
		return SandboxRef{}, f.launchErr
	}
	if f.launchGate != nil {
		f.launchArrived <- struct{}{}
		<-f.launchGate
	}
	f.launches.Add(1)
	n := f.seq.Add(1)
	name := fmt.Sprintf("sbx-%d", n)
	id := tmpl.Namespace + "/" + name + "/uid-" + name
	f.mu.Lock()
	f.live[id] = true
	f.ready[id] = true // ready immediately by default
	f.mu.Unlock()
	return SandboxRef{ID: id, Name: name, Namespace: tmpl.Namespace}, nil
}

func (f *fakeBackend) Ready(_ context.Context, ref SandboxRef) (bool, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if rem, ok := f.readyAfter[ref.ID]; ok && rem > 0 {
		f.readyAfter[ref.ID] = rem - 1
		return false, f.live[ref.ID], nil
	}
	return f.ready[ref.ID], f.live[ref.ID], nil
}

func (f *fakeBackend) Destroy(_ context.Context, ref SandboxRef) error {
	f.destroys.Add(1)
	f.mu.Lock()
	delete(f.live, ref.ID)
	delete(f.ready, ref.ID)
	f.mu.Unlock()
	return nil
}

func (f *fakeBackend) kill(id string) {
	f.mu.Lock()
	f.live[id] = false
	f.ready[id] = false
	f.mu.Unlock()
}

func tmplFor(class string, target int) PoolTemplate {
	return PoolTemplate{Namespace: "team-a", SandboxClass: class, Image: "img:1", Target: target}
}

func TestReplenishLaunchesUpToTarget(t *testing.T) {
	t.Parallel()
	b := newFakeBackend()
	m := NewManager(b, Hooks{})
	m.Register(tmplFor("fast", 3))

	if err := m.Replenish(context.Background(), "fast"); err != nil {
		t.Fatalf("Replenish: %v", err)
	}
	ready, target, leased := m.Status("fast")
	if ready != 3 || target != 3 || leased != 0 {
		t.Fatalf("want ready=3 target=3 leased=0, got ready=%d target=%d leased=%d", ready, target, leased)
	}
	if got := b.launches.Load(); got != 3 {
		t.Fatalf("want 3 launches, got %d", got)
	}

	// Idempotent: a second Replenish at target launches nothing more.
	if err := m.Replenish(context.Background(), "fast"); err != nil {
		t.Fatalf("Replenish 2: %v", err)
	}
	if got := b.launches.Load(); got != 3 {
		t.Fatalf("want still 3 launches, got %d", got)
	}
}

func TestLeaseFastPathConsumesWarmEntryAndReplenishes(t *testing.T) {
	t.Parallel()
	b := newFakeBackend()
	m := NewManager(b, Hooks{})
	m.Register(tmplFor("fast", 2))
	mustReplenish(t, m, "fast")

	failIfEmpty := true
	res, err := m.Lease(context.Background(), "fast", failIfEmpty)
	if err != nil {
		t.Fatalf("Lease: %v", err)
	}
	if !res.Warm {
		t.Fatalf("expected warm lease from a filled pool")
	}
	if res.LeaseID == "" || res.Ref.ID == "" {
		t.Fatalf("lease result missing ids: %+v", res)
	}

	// The consumed slot is replenished in the background. Drain that
	// goroutine, then assert the pool is back at target.
	m.bg.Wait()
	ready, _, leased := m.Status("fast")
	if ready != 2 || leased != 1 {
		t.Fatalf("want ready=2 leased=1 after lease, got ready=%d leased=%d", ready, leased)
	}
}

func TestLeaseFailIfEmptyReturnsErrPoolEmpty(t *testing.T) {
	t.Parallel()
	b := newFakeBackend()
	m := NewManager(b, Hooks{})
	m.Register(tmplFor("fast", 0))

	_, err := m.Lease(context.Background(), "fast", true)
	if !errors.Is(err, ErrPoolEmpty) {
		t.Fatalf("want ErrPoolEmpty, got %v", err)
	}
	if got := b.launches.Load(); got != 0 {
		t.Fatalf("fail-if-empty must not launch; got %d launches", got)
	}
}

func TestLeaseColdLaunchesWhenEmptyAndNotFailing(t *testing.T) {
	t.Parallel()
	b := newFakeBackend()
	m := NewManager(b, Hooks{})
	m.Register(tmplFor("fast", 0))

	res, err := m.Lease(context.Background(), "fast", false)
	if err != nil {
		t.Fatalf("Lease cold: %v", err)
	}
	if res.Warm {
		t.Fatalf("expected cold (non-warm) lease from an empty pool")
	}
	if b.launches.Load() < 1 {
		t.Fatalf("cold path must launch at least once")
	}
}

func TestReleaseDestroysSandboxAndReplenishes(t *testing.T) {
	t.Parallel()
	b := newFakeBackend()
	m := NewManager(b, Hooks{})
	m.Register(tmplFor("fast", 1))
	mustReplenish(t, m, "fast")

	res, err := m.Lease(context.Background(), "fast", true)
	if err != nil {
		t.Fatalf("Lease: %v", err)
	}
	destroysBefore := b.destroys.Load()

	if err := m.Release(context.Background(), res.LeaseID); err != nil {
		t.Fatalf("Release: %v", err)
	}
	// destroy-on-release: the leased sandbox is destroyed.
	if b.destroys.Load() != destroysBefore+1 {
		t.Fatalf("Release must destroy the leased sandbox exactly once")
	}
	// Lease is gone.
	if _, _, _, err := m.LeaseInfo(res.LeaseID); !errors.Is(err, ErrLeaseNotFound) {
		t.Fatalf("released lease should be unknown, got %v", err)
	}
	// Lease and Release each spawned a background replenish. Drain both,
	// then assert the pool is back at target: exactly one entry, not two.
	m.bg.Wait()
	ready, _, leased := m.Status("fast")
	if ready != 1 || leased != 0 {
		t.Fatalf("want ready=1 leased=0 after release, got ready=%d leased=%d", ready, leased)
	}
	if got := b.launches.Load(); got != 2 {
		t.Fatalf("want 2 launches in total (initial fill + one refill), got %d", got)
	}
}

// TestConcurrentReplenishFillsEachSlotOnce holds every Launch at the
// backend boundary and fires several Replenish calls at a pool one slot
// short. Only one of them may reach the backend. Before the launching
// reservation, every call computed the same shortfall and the pool
// over-filled, which is what made TestReleaseDestroysSandboxAndReplenishes
// time out under full-suite load (setec#11).
func TestConcurrentReplenishFillsEachSlotOnce(t *testing.T) {
	t.Parallel()
	const callers = 8
	b := newFakeBackend()
	b.launchGate = make(chan struct{})
	b.launchArrived = make(chan struct{}, callers)
	m := NewManager(b, Hooks{})
	m.Register(tmplFor("fast", 1))

	done := make(chan error, callers)
	for range callers {
		go func() { done <- m.Replenish(context.Background(), "fast") }()
	}

	// One caller reaches the backend and blocks on the gate.
	<-b.launchArrived
	// The other callers must return without launching. A second arrival
	// at the backend is the over-fill.
	for returned := 0; returned < callers-1; {
		select {
		case <-b.launchArrived:
			t.Fatalf("a second Replenish reached the backend for the same slot")
		case err := <-done:
			if err != nil {
				t.Fatalf("Replenish: %v", err)
			}
			returned++
		}
	}
	close(b.launchGate)
	if err := <-done; err != nil {
		t.Fatalf("gated Replenish: %v", err)
	}

	ready, target, leased := m.Status("fast")
	if ready != 1 || target != 1 || leased != 0 {
		t.Fatalf("want ready=1 target=1 leased=0, got ready=%d target=%d leased=%d", ready, target, leased)
	}
	if got := b.launches.Load(); got != 1 {
		t.Fatalf("want exactly 1 launch, got %d", got)
	}
}

// TestReplenishReleasesReservationOnLaunchError makes sure a failed
// Launch gives its slot back, so the next Replenish retries it instead
// of treating the slot as filled forever.
func TestReplenishReleasesReservationOnLaunchError(t *testing.T) {
	t.Parallel()
	b := newFakeBackend()
	b.launchErr = errors.New("boom")
	m := NewManager(b, Hooks{})
	m.Register(tmplFor("fast", 1))

	if err := m.Replenish(context.Background(), "fast"); err == nil {
		t.Fatalf("Replenish must report the launch error")
	}
	b.launchErr = nil
	if err := m.Replenish(context.Background(), "fast"); err != nil {
		t.Fatalf("Replenish after error: %v", err)
	}
	ready, _, _ := m.Status("fast")
	if ready != 1 {
		t.Fatalf("want the slot filled on retry, got ready=%d", ready)
	}
}

func TestReleaseUnknownLeaseIsNoop(t *testing.T) {
	t.Parallel()
	m := NewManager(newFakeBackend(), Hooks{})
	if err := m.Release(context.Background(), "does-not-exist"); err != nil {
		t.Fatalf("releasing unknown lease should be a no-op, got %v", err)
	}
}

func TestExecExactlyOncePerLease(t *testing.T) {
	t.Parallel()
	b := newFakeBackend()
	m := NewManager(b, Hooks{})
	m.Register(tmplFor("fast", 1))
	mustReplenish(t, m, "fast")
	res, err := m.Lease(context.Background(), "fast", true)
	if err != nil {
		t.Fatalf("Lease: %v", err)
	}

	if _, _, already, err := m.LeaseInfo(res.LeaseID); err != nil || already {
		t.Fatalf("first LeaseInfo: already=%v err=%v", already, err)
	}
	if _, _, already, err := m.LeaseInfo(res.LeaseID); err != nil || !already {
		t.Fatalf("second LeaseInfo must report alreadyRun: already=%v err=%v", already, err)
	}
}

func TestReplenishPrunesDeadEntries(t *testing.T) {
	t.Parallel()
	b := newFakeBackend()
	m := NewManager(b, Hooks{})
	m.Register(tmplFor("fast", 2))
	mustReplenish(t, m, "fast")

	// Kill one warm entry out from under the pool.
	ready, _, _ := m.Status("fast")
	if ready != 2 {
		t.Fatalf("precondition: want 2 ready, got %d", ready)
	}
	// Find a live id and kill it.
	b.mu.Lock()
	var victim string
	for id, alive := range b.live {
		if alive {
			victim = id
			break
		}
	}
	b.mu.Unlock()
	b.kill(victim)

	// Replenish should prune the dead entry and relaunch to target.
	if err := m.Replenish(context.Background(), "fast"); err != nil {
		t.Fatalf("Replenish: %v", err)
	}
	ready, _, _ = m.Status("fast")
	if ready != 2 {
		t.Fatalf("want pool restored to 2 ready after prune+relaunch, got %d", ready)
	}
}

func TestLeaseUnregisteredClassErrors(t *testing.T) {
	t.Parallel()
	m := NewManager(newFakeBackend(), Hooks{})
	if _, err := m.Lease(context.Background(), "ghost", false); err == nil {
		t.Fatalf("leasing an unregistered class should error")
	}
}

func TestColdLaunchWaitsForReadiness(t *testing.T) {
	t.Parallel()
	b := newFakeBackend()
	m := NewManager(b, Hooks{})
	m.Register(tmplFor("slow", 0))

	// Make the next launched sandbox take 2 polls to become ready.
	// We can't know its id up front, so set a default via a wrapper:
	// patch readyAfter for the next id (sbx-1).
	b.mu.Lock()
	b.readyAfter["team-a/sbx-1/uid-sbx-1"] = 2
	b.ready["team-a/sbx-1/uid-sbx-1"] = true
	b.mu.Unlock()

	res, err := m.Lease(context.Background(), "slow", false)
	if err != nil {
		t.Fatalf("Lease cold: %v", err)
	}
	if res.Warm {
		t.Fatalf("cold lease must report warm=false")
	}
}

func TestHooksFireOnLeaseAndRelease(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var lastReady, lastLeased int
	b := newFakeBackend()
	m := NewManager(b, Hooks{OnFill: func(_ string, ready, leased int) {
		mu.Lock()
		lastReady, lastLeased = ready, leased
		mu.Unlock()
	}})
	m.Register(tmplFor("fast", 1))
	mustReplenish(t, m, "fast")
	res, err := m.Lease(context.Background(), "fast", true)
	if err != nil {
		t.Fatalf("Lease: %v", err)
	}
	// Lease notifies the hook before it returns; the background refill
	// notifies again with the same leased count once its entry is ready.
	m.bg.Wait()
	mu.Lock()
	gotReady, gotLeased := lastReady, lastLeased
	mu.Unlock()
	if gotLeased != 1 || gotReady != 1 {
		t.Fatalf("after lease want hook ready=1 leased=1, got ready=%d leased=%d", gotReady, gotLeased)
	}
	if err := m.Release(context.Background(), res.LeaseID); err != nil {
		t.Fatalf("Release: %v", err)
	}
	m.bg.Wait()
	mu.Lock()
	gotReady, gotLeased = lastReady, lastLeased
	mu.Unlock()
	if gotLeased != 0 || gotReady != 1 {
		t.Fatalf("after release want hook ready=1 leased=0, got ready=%d leased=%d", gotReady, gotLeased)
	}
}

// --- helpers ---

// mustReplenish replenishes a class's pool and fails the test on error.
//
// though every current caller uses "fast".
//
//nolint:unparam // class is a parameter for call-site readability even
func mustReplenish(t *testing.T, m *Manager, class string) {
	t.Helper()
	if err := m.Replenish(context.Background(), class); err != nil {
		t.Fatalf("Replenish(%s): %v", class, err)
	}
}
