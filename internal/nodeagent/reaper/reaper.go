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

// Package reaper force-removes orphaned kata pod sandboxes whose microVM
// failed to tear down.
//
// Kata teardown can leave a sandbox half-dead: when the guest agent's ttrpc
// connection is already closed at stop time the shim logs "Agent did not stop
// sandbox" and returns without killing the VMM. The pod sandbox then stays
// NotReady while its firecracker/qemu process keeps running AND holds the
// containerd sandbox-name reservation. Every later RunPodSandbox for the same
// pod name then fails with
//
//	failed to reserve sandbox name "<pod>_0": name ... is reserved for "<old-id>"
//
// so the replacement Pod hangs in ContainerCreating forever. This is the root
// cause of the e2e contamination cascade tracked in setec#86.
//
// OrphanReaper periodically force-removes NotReady kata sandboxes older than a
// minimum age (equivalent to `crictl rmp -f`), which releases the name
// reservation and drives the leaked VMM to be reaped. Two guards keep it safe:
//   - MinAge: a sandbox must have been NotReady-eligible for at least this long,
//     so a sandbox that is transiently NotReady mid-creation is never touched.
//   - Handlers: only sandboxes whose CRI runtime handler matches one of these
//     prefixes (default "kata") are eligible — runc/system Pods and any Ready
//     sandbox are never candidates.
package reaper

import (
	"context"
	"log"
	"strings"
	"time"
)

// Sandbox is the minimal view of a CRI pod sandbox the reaper needs.
type Sandbox struct {
	ID             string
	Name           string
	Namespace      string
	RuntimeHandler string
	CreatedAt      time.Time
}

// SandboxClient is the narrow CRI surface the reaper depends on. It is an
// interface so the reaper is unit-testable without a real containerd/CRI
// endpoint (see cri.go for the production adapter).
type SandboxClient interface {
	// ListNotReadySandboxes returns every pod sandbox currently in the
	// NOT_READY state.
	ListNotReadySandboxes(ctx context.Context) ([]Sandbox, error)
	// ForceRemove force-removes the sandbox (stop-then-remove, like
	// `crictl rmp -f`), releasing its containerd name reservation.
	ForceRemove(ctx context.Context, id string) error
}

// Metrics receives reap outcomes. Fields may be nil (no-op) — keeps the reaper
// decoupled from any specific Prometheus registry.
type Metrics struct {
	// Reaped is called once per successfully reaped sandbox, labelled by the
	// sandbox's runtime handler.
	Reaped func(handler string)
	// Errors is called once per reap error (list or remove).
	Errors func()
}

const (
	defaultInterval = time.Minute
	defaultMinAge   = 3 * time.Minute
)

// OrphanReaper force-reaps orphaned kata sandboxes on a fixed interval.
type OrphanReaper struct {
	// Client lists and removes sandboxes. Required.
	Client SandboxClient
	// Interval between sweeps. Defaults to defaultInterval when <= 0.
	Interval time.Duration
	// MinAge is the minimum sandbox age before it is eligible for reaping.
	// Defaults to defaultMinAge when <= 0.
	MinAge time.Duration
	// Handlers are the CRI runtime-handler prefixes eligible for reaping.
	// Defaults to {"kata"} when empty.
	Handlers []string
	// Logger is an injectable logger; nil routes to log.Printf.
	Logger func(format string, args ...any)
	// Clock is an injectable time source for tests; nil uses time.Now.
	Clock func() time.Time
	// Metrics receives reap outcomes; zero value is a no-op.
	Metrics Metrics
}

// Run sweeps immediately, then on Interval, until ctx is cancelled. It is
// intended to run in its own goroutine.
func (r *OrphanReaper) Run(ctx context.Context) {
	interval := r.Interval
	if interval <= 0 {
		interval = defaultInterval
	}
	if r.Client == nil {
		r.logf("orphan-reaper: no CRI client configured; reaper disabled")
		return
	}

	// Pay the first sweep up front so a runner that just crashed mid-run is
	// cleaned before the next suite/workload starts.
	r.runOnce(ctx)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.runOnce(ctx)
		}
	}
}

func (r *OrphanReaper) runOnce(ctx context.Context) {
	reaped, err := r.reap(ctx)
	if err != nil {
		r.logf("orphan-reaper: sweep error: %v", err)
	}
	if reaped > 0 {
		r.logf("orphan-reaper: force-removed %d orphaned kata sandbox(es)", reaped)
	}
}

// reap performs a single sweep. It returns the number of sandboxes reaped and
// the first error encountered (a remove failure does not abort the sweep —
// remaining candidates are still processed).
func (r *OrphanReaper) reap(ctx context.Context) (int, error) {
	minAge := r.MinAge
	if minAge <= 0 {
		minAge = defaultMinAge
	}
	now := time.Now
	if r.Clock != nil {
		now = r.Clock
	}

	sandboxes, err := r.Client.ListNotReadySandboxes(ctx)
	if err != nil {
		r.metricsErrors()
		return 0, err
	}

	cutoff := now().Add(-minAge)
	var reaped int
	var firstErr error
	for _, sb := range sandboxes {
		if !r.eligible(sb.RuntimeHandler) {
			continue
		}
		// CreatedAt zero means the runtime did not report a timestamp; treat
		// it as old enough rather than never-eligible, but a positive age that
		// is still within MinAge is skipped.
		if !sb.CreatedAt.IsZero() && sb.CreatedAt.After(cutoff) {
			continue
		}
		if err := r.Client.ForceRemove(ctx, sb.ID); err != nil {
			r.metricsErrors()
			r.logf("orphan-reaper: force-remove %s (%s/%s, handler=%s) failed: %v",
				sb.ID, sb.Namespace, sb.Name, sb.RuntimeHandler, err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		reaped++
		if r.Metrics.Reaped != nil {
			r.Metrics.Reaped(sb.RuntimeHandler)
		}
		r.logf("orphan-reaper: force-removed orphaned sandbox %s (%s/%s, handler=%s, age=%s)",
			sb.ID, sb.Namespace, sb.Name, sb.RuntimeHandler, ageString(sb.CreatedAt, now()))
	}
	return reaped, firstErr
}

// eligible reports whether a runtime handler is in scope for reaping.
func (r *OrphanReaper) eligible(handler string) bool {
	handlers := r.Handlers
	if len(handlers) == 0 {
		handlers = []string{"kata"}
	}
	for _, h := range handlers {
		if h != "" && strings.HasPrefix(handler, h) {
			return true
		}
	}
	return false
}

func (r *OrphanReaper) metricsErrors() {
	if r.Metrics.Errors != nil {
		r.Metrics.Errors()
	}
}

func (r *OrphanReaper) logf(format string, args ...any) {
	if r.Logger != nil {
		r.Logger(format, args...)
		return
	}
	log.Printf(format, args...)
}

func ageString(createdAt, now time.Time) string {
	if createdAt.IsZero() {
		return "unknown"
	}
	return now.Sub(createdAt).Truncate(time.Second).String()
}
