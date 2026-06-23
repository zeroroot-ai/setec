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

// Package leasepool implements the warm-pool lease layer over the Setec
// SandboxService. It keeps a pool of pre-warmed Sandboxes per (tenant,
// SandboxClass) so a caller can Lease one without paying cold-boot
// latency, Exec inside it, then Release it — which destroys the Sandbox
// (destroy-on-release; a dirty sandbox is never reused) and replenishes
// the pool back to its warm target.
//
// The pool is deliberately decoupled from the Kubernetes client through
// the Backend interface: the frontend wires a CR-backed implementation,
// while tests drive an in-memory fake. This keeps the pool's claim and
// replenish logic unit-testable without envtest.
package leasepool

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// ErrPoolEmpty is returned by Lease when the pool has no ready entry and
// the caller asked to fail rather than cold-launch.
var ErrPoolEmpty = errors.New("leasepool: no warm sandbox available")

// ErrLeaseNotFound is returned when an Exec or Release references a lease
// the manager does not know about.
var ErrLeaseNotFound = errors.New("leasepool: lease not found")

// PoolTemplate is everything needed to materialise one pre-warmed
// Sandbox for a class. The frontend derives it from the SandboxClass
// (PreWarmImage, DefaultResources, PreWarmPoolSize) so the lease pool is
// grounded in the existing cluster policy rather than a parallel config.
type PoolTemplate struct {
	// Namespace is the tenant namespace the pool's Sandboxes live in.
	Namespace string
	// SandboxClass is the class entries are launched against. Required.
	SandboxClass string
	// Image is the OCI reference pool entries boot. Required.
	Image string
	// Command is the entrypoint pool entries run while warm. A warm entry
	// runs a long-lived idle command so it stays Running until leased;
	// the real workload is supplied at Exec time.
	Command []string
	// SnapshotName, when set, restores pool entries from this Snapshot
	// (the warm-start mechanism) instead of cold-booting. The Snapshot
	// must live in Namespace.
	SnapshotName string
	// Target is the number of warm entries the pool maintains. When zero
	// the pool holds no standby entries and every Lease cold-launches.
	Target int
	// VCPU and Memory carry the resource budget warm entries are launched
	// with (the class's DefaultResources). VCPU<=0 leaves resources unset
	// so the operator applies the class default at admission.
	VCPU   int32
	Memory string
}

// SandboxRef identifies a launched Sandbox.
type SandboxRef struct {
	// ID is the <namespace>/<name>/<uid> tuple, identical in shape to
	// SandboxService ids.
	ID string
	// Name is the Sandbox CR name.
	Name string
	// Namespace is the Sandbox CR namespace.
	Namespace string
}

// Backend is the set of Sandbox operations the pool needs. The frontend
// implements it against the controller-runtime client; tests implement a
// fake. Launch must return promptly with a reference; readiness is polled
// separately via Ready.
type Backend interface {
	// Launch creates a warm Sandbox for the template and returns its ref.
	Launch(ctx context.Context, tmpl PoolTemplate) (SandboxRef, error)
	// Ready reports whether the referenced Sandbox has reached a state in
	// which it can accept an Exec (i.e. Running). A Sandbox that has died
	// returns ready=false, alive=false so the pool can prune it.
	Ready(ctx context.Context, ref SandboxRef) (ready, alive bool, err error)
	// Destroy deletes the Sandbox CR (destroy-on-release).
	Destroy(ctx context.Context, ref SandboxRef) error
}

// lease tracks one checked-out Sandbox.
type lease struct {
	id           string
	ref          SandboxRef
	sandboxClass string
	execStarted  bool
}

// entry is a pool slot: either warming (ready=false) or ready to lease.
type entry struct {
	ref   SandboxRef
	ready bool
}

// Hooks lets the caller observe pool size changes (e.g. to update a
// Prometheus gauge) without the pool importing the metrics package.
type Hooks struct {
	// OnFill is invoked with the class and its current ready/leased counts
	// whenever they change. Optional.
	OnFill func(sandboxClass string, ready, leased int)
}

// Manager maintains warm pools keyed by SandboxClass within a single
// tenant namespace. The frontend constructs one Manager per resolved
// namespace (lazily) so leases never cross tenant boundaries.
type Manager struct {
	backend Backend
	hooks   Hooks
	// idFn generates lease ids; overridable in tests for determinism.
	idFn func() string

	mu        sync.Mutex
	templates map[string]PoolTemplate // class -> template
	pools     map[string][]entry      // class -> warm entries
	leases    map[string]*lease       // lease id -> lease
}

// NewManager builds a pool Manager over the given Backend.
func NewManager(b Backend, hooks Hooks) *Manager {
	return &Manager{
		backend:   b,
		hooks:     hooks,
		idFn:      randID,
		templates: map[string]PoolTemplate{},
		pools:     map[string][]entry{},
		leases:    map[string]*lease{},
	}
}

// Register records (or updates) the template for a class. Idempotent; the
// frontend calls it on the first Lease for a class. Registration does not
// itself launch entries — call Replenish (or rely on the Run loop).
func (m *Manager) Register(tmpl PoolTemplate) {
	m.mu.Lock()
	m.templates[tmpl.SandboxClass] = tmpl
	if _, ok := m.pools[tmpl.SandboxClass]; !ok {
		m.pools[tmpl.SandboxClass] = nil
	}
	m.mu.Unlock()
}

// LeaseResult is the outcome of a successful Lease.
type LeaseResult struct {
	LeaseID string
	Ref     SandboxRef
	// Warm is true when the lease was satisfied from a pre-warmed entry,
	// false when it was cold-launched on demand.
	Warm bool
}

// Lease claims a ready Sandbox for the class. When the pool has a ready
// entry it is handed out immediately (the fast path). Otherwise, if
// failIfEmpty is false, a Sandbox is cold-launched on demand and the pool
// is replenished in the background; if failIfEmpty is true ErrPoolEmpty is
// returned.
func (m *Manager) Lease(ctx context.Context, sandboxClass string, failIfEmpty bool) (LeaseResult, error) {
	m.mu.Lock()
	tmpl, ok := m.templates[sandboxClass]
	if !ok {
		m.mu.Unlock()
		return LeaseResult{}, fmt.Errorf("leasepool: class %q not registered", sandboxClass)
	}

	if ref, found := m.popReadyLocked(sandboxClass); found {
		lr := m.recordLeaseLocked(sandboxClass, ref)
		m.mu.Unlock()
		// Refill the slot we just consumed.
		go m.replenishAsync(sandboxClass)
		return LeaseResult{LeaseID: lr.id, Ref: ref, Warm: true}, nil
	}
	m.mu.Unlock()

	if failIfEmpty {
		return LeaseResult{}, ErrPoolEmpty
	}

	// Cold path: launch on demand, then make it ready before handing out.
	ref, err := m.backend.Launch(ctx, tmpl)
	if err != nil {
		return LeaseResult{}, fmt.Errorf("cold-launch: %w", err)
	}
	if err := m.waitReady(ctx, ref); err != nil {
		_ = m.backend.Destroy(context.WithoutCancel(ctx), ref)
		return LeaseResult{}, err
	}

	m.mu.Lock()
	lr := m.recordLeaseLocked(sandboxClass, ref)
	m.mu.Unlock()
	go m.replenishAsync(sandboxClass)
	return LeaseResult{LeaseID: lr.id, Ref: ref, Warm: false}, nil
}

// LeaseInfo resolves a lease id to its Sandbox ref and class, marking the
// single permitted Exec as started. The second call for a lease returns
// alreadyRun=true so the service can reject a duplicate Exec.
func (m *Manager) LeaseInfo(leaseID string) (ref SandboxRef, sandboxClass string, alreadyRun bool, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, ok := m.leases[leaseID]
	if !ok {
		return SandboxRef{}, "", false, ErrLeaseNotFound
	}
	if l.execStarted {
		return l.ref, l.sandboxClass, true, nil
	}
	l.execStarted = true
	return l.ref, l.sandboxClass, false, nil
}

// Release destroys the leased Sandbox (destroy-on-release) and triggers a
// replenish so the pool returns to its warm target. Releasing an unknown
// lease is a no-op success so callers may release idempotently.
func (m *Manager) Release(ctx context.Context, leaseID string) error {
	m.mu.Lock()
	l, ok := m.leases[leaseID]
	if !ok {
		m.mu.Unlock()
		return nil
	}
	delete(m.leases, leaseID)
	class := l.sandboxClass
	m.notifyLocked(class)
	m.mu.Unlock()

	if err := m.backend.Destroy(ctx, l.ref); err != nil {
		return fmt.Errorf("destroy leased sandbox: %w", err)
	}
	go m.replenishAsync(class)
	return nil
}

// Replenish brings the class's pool up to its target by launching new
// warm entries for any shortfall, and prunes entries whose Sandbox has
// died. It is safe to call repeatedly and concurrently.
func (m *Manager) Replenish(ctx context.Context, sandboxClass string) error {
	m.mu.Lock()
	tmpl, ok := m.templates[sandboxClass]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("leasepool: class %q not registered", sandboxClass)
	}
	m.pruneDeadLocked(ctx, sandboxClass)
	have := len(m.pools[sandboxClass])
	want := tmpl.Target
	shortfall := want - have
	m.mu.Unlock()

	var errs []error
	for range shortfall {
		ref, err := m.backend.Launch(ctx, tmpl)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		m.mu.Lock()
		m.pools[sandboxClass] = append(m.pools[sandboxClass], entry{ref: ref, ready: false})
		m.mu.Unlock()
	}

	// Promote warming entries that have become ready.
	m.refreshReadiness(ctx, sandboxClass)
	return errors.Join(errs...)
}

// Status reports the ready/target/leased counts for a class.
func (m *Manager) Status(sandboxClass string) (ready, target, leased int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range m.pools[sandboxClass] {
		if e.ready {
			ready++
		}
	}
	if t, ok := m.templates[sandboxClass]; ok {
		target = t.Target
	}
	for _, l := range m.leases {
		if l.sandboxClass == sandboxClass {
			leased++
		}
	}
	return ready, target, leased
}

// Run is the background replenish loop. It periodically replenishes every
// registered class and refreshes readiness until ctx is cancelled.
func (m *Manager) Run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for _, class := range m.classes() {
				_ = m.Replenish(ctx, class)
			}
		}
	}
}

// --- internal helpers (most assume m.mu held where noted) ---

func (m *Manager) classes() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.templates))
	for c := range m.templates {
		out = append(out, c)
	}
	return out
}

// popReadyLocked removes and returns the first ready entry. Caller holds mu.
func (m *Manager) popReadyLocked(class string) (SandboxRef, bool) {
	pool := m.pools[class]
	for i, e := range pool {
		if e.ready {
			m.pools[class] = append(pool[:i], pool[i+1:]...)
			return e.ref, true
		}
	}
	return SandboxRef{}, false
}

// recordLeaseLocked registers a new lease. Caller holds mu.
func (m *Manager) recordLeaseLocked(class string, ref SandboxRef) *lease {
	l := &lease{id: m.idFn(), ref: ref, sandboxClass: class}
	m.leases[l.id] = l
	m.notifyLocked(class)
	return l
}

// pruneDeadLocked drops entries whose Sandbox is no longer alive. Caller
// holds mu; uses the backend without releasing it because Ready is a
// fast cached read in practice.
func (m *Manager) pruneDeadLocked(ctx context.Context, class string) {
	pool := m.pools[class]
	kept := pool[:0]
	for _, e := range pool {
		_, alive, err := m.backend.Ready(ctx, e.ref)
		if err == nil && !alive {
			continue // drop dead entry
		}
		kept = append(kept, e)
	}
	m.pools[class] = kept
}

// refreshReadiness polls warming entries and promotes the ones that have
// reached Running.
func (m *Manager) refreshReadiness(ctx context.Context, class string) {
	m.mu.Lock()
	refs := make([]SandboxRef, 0)
	for i := range m.pools[class] {
		if !m.pools[class][i].ready {
			refs = append(refs, m.pools[class][i].ref)
		}
	}
	m.mu.Unlock()

	for _, ref := range refs {
		ready, _, err := m.backend.Ready(ctx, ref)
		if err != nil || !ready {
			continue
		}
		m.mu.Lock()
		for i := range m.pools[class] {
			if m.pools[class][i].ref.ID == ref.ID {
				m.pools[class][i].ready = true
			}
		}
		m.notifyLocked(class)
		m.mu.Unlock()
	}
}

// waitReady blocks until ref is ready, ctx is done, or ref dies.
func (m *Manager) waitReady(ctx context.Context, ref SandboxRef) error {
	const poll = 200 * time.Millisecond
	for {
		ready, alive, err := m.backend.Ready(ctx, ref)
		if err != nil {
			return fmt.Errorf("poll readiness: %w", err)
		}
		if ready {
			return nil
		}
		if !alive {
			return fmt.Errorf("sandbox %s died before becoming ready", ref.ID)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
	}
}

// replenishAsync runs a replenish detached from a request context so a
// lease/release returns promptly while the pool refills in the background.
func (m *Manager) replenishAsync(class string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	_ = m.Replenish(ctx, class)
}

// notifyLocked invokes the fill hook with current counts. Caller holds mu.
func (m *Manager) notifyLocked(class string) {
	if m.hooks.OnFill == nil {
		return
	}
	ready := 0
	for _, e := range m.pools[class] {
		if e.ready {
			ready++
		}
	}
	leased := 0
	for _, l := range m.leases {
		if l.sandboxClass == class {
			leased++
		}
	}
	m.hooks.OnFill(class, ready, leased)
}
