// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package pool

import (
	"context"
	"log"
	"time"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
)

// SandboxClassLister is the lookup surface the TickReconciler depends
// on. In production the node-agent wraps a controller-runtime or
// client-go cached lister; tests inject a static function.
type SandboxClassLister func() []setecv1alpha1.SandboxClass

// TickReconciler periodically drives the pool Manager's
// ReconcilePools against the currently-known set of SandboxClasses.
// It owns no state of its own; it composes a Manager, a Lister, and
// an interval.
type TickReconciler struct {
	Manager  *Manager
	Lister   SandboxClassLister
	Interval time.Duration
	// Logger is an optional logging hook. Defaults to the standard
	// library log package when nil.
	Logger func(format string, args ...any)
	// FillObserver, when non-nil, receives the per-class entry counts
	// after every reconcile pass. The node-agent wires this to the
	// setec_prewarm_pool_entries gauge so pool state is observable
	// without a gRPC round-trip (ADR-0004 acceptance).
	FillObserver func(fills map[string]int)
}

// Run blocks until ctx is cancelled, ticking every Interval. The
// first reconcile fires immediately so a freshly-started node-agent
// pays a warm-up cost up front rather than after the first tick.
func (r *TickReconciler) Run(ctx context.Context) {
	interval := r.Interval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	if r.Manager == nil || r.Lister == nil {
		r.logf("pool reconciler: missing Manager or Lister, goroutine exiting")
		return
	}

	// Fire once immediately so operators see the pool fill on boot.
	r.runOnce(ctx)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			r.logf("pool reconciler: context cancelled, exiting")
			return
		case <-ticker.C:
			r.runOnce(ctx)
		}
	}
}

func (r *TickReconciler) runOnce(ctx context.Context) {
	classes := r.Lister()
	if err := r.Manager.ReconcilePools(ctx, classes); err != nil {
		r.logf("pool reconciler: ReconcilePools returned: %v", err)
	}
	if r.FillObserver != nil {
		r.FillObserver(r.Manager.FillByClass())
	}
}

func (r *TickReconciler) logf(format string, args ...any) {
	if r.Logger != nil {
		r.Logger(format, args...)
		return
	}
	log.Printf(format, args...)
}
