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
// candidate list (primary + fallback) has both a registered Dispatcher and
// at least one capable node.
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

// Select picks the first backend from the candidate list whose Dispatcher is
// registered AND whose name appears in nodeCapabilities.
//
// The candidate list is built as follows:
//  1. If class is non-nil and class.Spec.Runtime is non-nil and
//     class.Spec.Runtime.Backend is non-empty, use that as the primary, with
//     class.Spec.Runtime.Fallback as the tail.
//  2. Otherwise use cfg.Defaults.Runtime.Backend as the primary, with
//     cfg.Defaults.Runtime.Fallback as the tail.
//
// Selection.FellBack is true when the selected backend is not the primary
// (position 0) candidate.  Select does not mutate class, cfg, or
// nodeCapabilities.
//
// Returns ErrNoEligibleRuntime when no candidate satisfies both constraints.
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

	for i, backend := range candidates {
		d, ok := r.dispatchers[backend]
		if !ok {
			continue
		}
		if !capSet[backend] {
			continue
		}
		sel := &Selection{
			Backend:    backend,
			Dispatcher: d,
			FellBack:   i > 0,
		}
		if i > 0 {
			sel.FromBackend = primary
		}
		return sel, nil
	}

	return nil, fmt.Errorf("%w: requested=%q fallback=%v registered=%v nodeCapabilities=%v",
		ErrNoEligibleRuntime, primary, fallback, r.enabledBackendsLocked(), nodeCapabilities)
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
