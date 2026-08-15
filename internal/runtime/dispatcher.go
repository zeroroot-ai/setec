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

package runtime

import (
	"errors"
	"fmt"
	"sort"
	"sync"

	corev1 "k8s.io/api/core/v1"

	"github.com/zeroroot-ai/setec/api/v1alpha1"
)

// ErrNoEligibleRuntime is returned by Registry.Select when no backend in the
// candidate list (primary + fallback) has a registered Dispatcher.
//
// This is the terminal half of "no eligible runtime": the operator was never
// wired with any of the requested backends, there is no Dispatcher to build a
// Pod spec from, and no amount of node provisioning can change that. Only a
// configuration change (enabling the backend) can.
//
// It is deliberately NOT returned for the transient half — a Dispatcher exists
// but no node advertises its capability label yet. That case is a successful
// Selection with Provisional set; see Selection.Provisional for why the
// difference matters.
var ErrNoEligibleRuntime = errors.New("no eligible runtime")

// Dispatcher is the backend-agnostic interface every isolation runtime must
// implement.  Adding a new backend is a matter of creating a new file that
// produces a value satisfying this interface — no changes to shared code.
//
// Implementations must be safe for concurrent use; Select may be called from
// multiple goroutines simultaneously.
type Dispatcher interface {
	// Name returns the canonical backend name (e.g. "kata-fc").  It must match
	// the key used in RuntimeConfig.Runtimes.
	Name() string

	// RuntimeClassName returns the Kubernetes RuntimeClass name the Pod spec
	// should reference (e.g. "kata-fc", "runsc").
	RuntimeClassName() string

	// NodeAffinity returns the required node-affinity rule that restricts
	// scheduling to nodes capable of running this backend.  May return nil if
	// the backend has no node-affinity requirements (unusual).
	NodeAffinity() *corev1.NodeAffinity

	// Overhead returns the resource overhead for Pods using this backend.  The
	// map mirrors Pod.Spec.Overhead.  May return nil to indicate zero overhead.
	Overhead() corev1.ResourceList

	// MutatePod applies backend-specific mutations to pod after the main pod
	// spec has been constructed.  The params map carries the SandboxClass
	// runtime.params values.  MutatePod must be idempotent — calling it more
	// than once on the same Pod must produce the same result.
	MutatePod(pod *corev1.Pod, params map[string]string) error
}

// Selection is the result of a successful Registry.Select call.  It carries
// the chosen backend name, the Dispatcher implementation, and enough metadata
// to record the choice in Sandbox.status.runtime.chosen and emit fallback
// metrics.
type Selection struct {
	// Backend is the canonical name of the chosen backend (e.g. "gvisor").
	// Written to Sandbox.status.runtime.chosen by the reconciler.
	Backend string

	// Dispatcher is the implementation that will produce the RuntimeClassName,
	// NodeAffinity, Overhead, and any Pod mutations for this Sandbox.
	Dispatcher Dispatcher

	// FellBack is true when the chosen Backend differs from the backend that
	// was originally requested (primary backend in class or cluster default).
	FellBack bool

	// FromBackend is the backend name that was originally requested, populated
	// only when FellBack is true.  Used by the reconciler to increment
	// setec_sandbox_fallback_total{from,to}.
	FromBackend string

	// Provisional is true when the chosen Backend has a registered Dispatcher
	// but no node in the cluster advertises its capability label yet.  The
	// Selection is otherwise complete and usable: RuntimeClassName,
	// NodeAffinity, Overhead and MutatePod are all valid.
	//
	// This is the "not yet" case, as distinct from the "never" case that
	// ErrNoEligibleRuntime reports, and the reconciler must still create the
	// Pod for it.  A cluster autoscaler — Karpenter, cluster-autoscaler,
	// anything else — provisions in response to an unschedulable Pod and
	// nothing else.  Withholding the Pod until a capable node exists is
	// therefore self-defeating on a pool that scales to zero: no capable node
	// means no Pod, no Pod means nothing unschedulable, and nothing
	// unschedulable means no node is ever provisioned.
	//
	// Creating the Pod is also the right answer on a cluster with no
	// autoscaler at all.  It simply stays Pending, carrying the scheduler's
	// own explanation of which constraint it failed, and schedules by itself
	// the moment an administrator adds a capable node — no operator
	// intervention, no stale terminal status to clear.
	//
	// Whether an autoscaler can actually act on the Pod is a property of the
	// Pod's hard scheduling constraints versus the node pool's declared
	// labels/taints, not of this flag: a Pod whose required node affinity
	// names a label no node pool declares is unschedulable AND unprovisionable.
	// See charts/setec/values.yaml (karpenter.nodeLabels) for the invariant
	// that keeps the two in agreement.
	Provisional bool
}

// Registry holds the set of Dispatcher implementations that the operator has
// been wired with at startup.  Only backends whose BackendConfig.Enabled is
// true should be registered; the Registry does not consult config itself — the
// caller (cmd/manager/main.go) is responsible for filtering.
//
// Registry is safe for concurrent reads after the initial Register calls.
// Register itself must not be called concurrently with Select or other
// Register calls.
type Registry struct {
	mu          sync.RWMutex
	dispatchers map[string]Dispatcher
}

// NewRegistry returns an empty Registry ready for Dispatcher registration.
func NewRegistry() *Registry {
	return &Registry{
		dispatchers: make(map[string]Dispatcher),
	}
}

// Register adds d to the registry.  If a Dispatcher with the same name is
// already registered it is silently replaced.  Register is not goroutine-safe
// relative to itself — callers must complete all Register calls before
// starting concurrent Select calls.
func (r *Registry) Register(d Dispatcher) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.dispatchers[d.Name()] = d
}

// EnabledBackends returns the sorted list of backend names currently in the
// registry.  The slice is a snapshot; later Register calls do not affect it.
func (r *Registry) EnabledBackends() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.dispatchers))
	for name := range r.dispatchers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Select picks a backend from the candidate list.
//
// The candidate list is built as follows:
//  1. If class is non-nil and class.Spec.Runtime is non-nil and
//     class.Spec.Runtime.Backend is non-empty, use that as the primary, with
//     class.Spec.Runtime.Fallback as the tail.
//  2. Otherwise use cfg.Defaults.Runtime.Backend as the primary, with
//     cfg.Defaults.Runtime.Fallback as the tail.
//
// Selection happens in two passes, because "no capable node" and "no such
// backend" are different conditions with different remedies and only the
// second one is terminal:
//
//	Pass 1 — a backend that is registered AND currently advertised by at least
//	one node.  This is the only pass that may fall back: the point of a
//	fallback chain is to move a Sandbox onto a backend the cluster can run
//	right now, so it is meaningful only while some candidate can run right now.
//
//	Pass 2 — no candidate has a capable node.  Falling back here would buy
//	nothing (a fallback with no capable node is no more schedulable than the
//	primary with no capable node), so the first REGISTERED candidate wins and
//	the Selection is marked Provisional.  The caller creates the Pod anyway;
//	that unschedulable Pod is what makes a scale-to-zero pool provision a node.
//	See Selection.Provisional.
//
// Selection.FellBack is true when the selected backend is not the primary
// (position 0) candidate — in pass 2 that means the primary has no Dispatcher
// at all, which is a permanent condition and a real fallback.  Select does not
// mutate class, cfg, or nodeCapabilities.
//
// Returns ErrNoEligibleRuntime only when NO candidate has a registered
// Dispatcher, which no node provisioning could ever fix.
func (r *Registry) Select(
	class *v1alpha1.SandboxClass,
	cfg *RuntimeConfig,
	nodeCapabilities []string,
) (*Selection, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	primary, fallback := candidateChain(class, cfg)

	capSet := toSet(nodeCapabilities)
	candidates := append([]string{primary}, fallback...)

	newSelection := func(i int, backend string, d Dispatcher, provisional bool) *Selection {
		sel := &Selection{
			Backend:     backend,
			Dispatcher:  d,
			FellBack:    i > 0,
			Provisional: provisional,
		}
		if i > 0 {
			sel.FromBackend = primary
		}
		return sel
	}

	// Pass 1: registered and runnable on a node that exists today.
	for i, backend := range candidates {
		d, ok := r.dispatchers[backend]
		if !ok {
			continue
		}
		if !capSet[backend] {
			continue
		}
		return newSelection(i, backend, d, false), nil
	}

	// Pass 2: registered, but nothing capable is up yet.
	for i, backend := range candidates {
		d, ok := r.dispatchers[backend]
		if !ok {
			continue
		}
		return newSelection(i, backend, d, true), nil
	}

	return nil, fmt.Errorf("%w: no candidate backend has a registered dispatcher: requested=%q fallback=%v registered=%v",
		ErrNoEligibleRuntime, primary, fallback, r.enabledBackendsLocked())
}

// candidateChain derives the primary backend name and the ordered fallback
// slice from a SandboxClass and the cluster-wide RuntimeConfig defaults.
// It does not consult the registry or any node state.
func candidateChain(class *v1alpha1.SandboxClass, cfg *RuntimeConfig) (primary string, fallback []string) {
	if class != nil && class.Spec.Runtime != nil && class.Spec.Runtime.Backend != "" {
		return class.Spec.Runtime.Backend, copyStrings(class.Spec.Runtime.Fallback)
	}
	return cfg.Defaults.Runtime.Backend, copyStrings(cfg.Defaults.Runtime.Fallback)
}

// enabledBackendsLocked returns the sorted backend names; caller must hold
// r.mu.RLock.
func (r *Registry) enabledBackendsLocked() []string {
	names := make([]string, 0, len(r.dispatchers))
	for name := range r.dispatchers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// toSet converts a string slice to a set for O(1) membership tests.
func toSet(ss []string) map[string]bool {
	m := make(map[string]bool, len(ss))
	for _, s := range ss {
		m[s] = true
	}
	return m
}

// copyStrings returns a shallow copy of ss, or nil when ss is empty.  This
// avoids aliasing the caller's slice in the returned Selection.
func copyStrings(ss []string) []string {
	if len(ss) == 0 {
		return nil
	}
	out := make([]string, len(ss))
	copy(out, ss)
	return out
}
