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
}

func (r *TickReconciler) logf(format string, args ...any) {
	if r.Logger != nil {
		r.Logger(format, args...)
		return
	}
	log.Printf(format, args...)
}
