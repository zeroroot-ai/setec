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

// Package main provides the runtime-agent DaemonSet binary. The probe loop
// lives in run.go so it can be unit-tested without standing up a real HTTP
// server or Kubernetes cluster.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/zeroroot-ai/setec/internal/metrics"
	"github.com/zeroroot-ai/setec/internal/runtime"
	"github.com/zeroroot-ai/setec/internal/runtimeagent"
	"github.com/zeroroot-ai/setec/internal/runtimeagent/probe"
)

const (
	// defaultProbeInterval is used when cfg.Defaults.Runtime.ProbeInterval is zero.
	defaultProbeInterval = 5 * time.Minute

	// startupBudget is the total context budget for the initial probe cycle.
	startupBudget = 10 * time.Second

	// perProbeBudget is the per-probe context timeout derived from the spec.
	perProbeBudget = 2 * time.Second
)

// Dependencies bundles all external collaborators so Run can be unit-tested
// without real Kubernetes or real probes.
type Dependencies struct {
	// Client is the controller-runtime client used to patch Node objects.
	Client client.Client

	// Probes is the filtered list of probes to run on each cycle.
	Probes []probe.Probe

	// NodeName is the Kubernetes Node this DaemonSet pod runs on.
	NodeName string

	// Interval is how often to re-probe. Zero falls back to defaultProbeInterval.
	Interval time.Duration

	// Collectors holds the Prometheus metric helpers. Nil is safe (all
	// methods on *metrics.Collectors are nil-guarded).
	Collectors *metrics.Collectors
}

// Run executes the probe loop, blocking until ctx is cancelled. On the first
// iteration the probes are run immediately; subsequent iterations are
// scheduled by a ticker.
//
// The function logs label changes (previous cycle vs current) on stderr so
// operators can detect runtime availability transitions in pod logs without
// instrumenting a full structured logger dependency.
func Run(ctx context.Context, deps Dependencies) {
	interval := deps.Interval
	if interval <= 0 {
		interval = defaultProbeInterval
	}

	var previous []probe.CapabilityResult

	runOnce := func() {
		results := runProbes(ctx, deps.Probes, deps.Collectors)
		logChanges(previous, results)
		previous = results

		applyCtx, cancel := context.WithTimeout(ctx, startupBudget)
		defer cancel()
		if err := runtimeagent.Apply(applyCtx, deps.Client, deps.NodeName, results); err != nil {
			fmt.Fprintf(os.Stderr, "runtime-agent: apply labels: %v\n", err)
		}
	}

	// First probe cycle runs immediately.
	runOnce()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			fmt.Fprintln(os.Stderr, "runtime-agent: context cancelled, stopping probe loop")
			return
		case <-ticker.C:
			runOnce()
		}
	}
}

// runProbes executes each probe within perProbeBudget and collects results.
// The parent ctx is used as the base; if it is already cancelled the loop
// exits early.
func runProbes(ctx context.Context, probes []probe.Probe, col *metrics.Collectors) []probe.CapabilityResult {
	results := make([]probe.CapabilityResult, 0, len(probes))
	for _, p := range probes {
		if ctx.Err() != nil {
			return results
		}
		probeCtx, cancel := context.WithTimeout(ctx, perProbeBudget)
		r := p.Check(probeCtx)
		cancel()

		r.Backend = p.Name()
		results = append(results, r)

		col.SetNodeRuntimeAvailable(r.Backend, r.Available)
		if !r.Available && r.Reason != "" {
			col.IncNodeProbeError(r.Backend, probeFailReason(r.Reason))
		}
	}
	return results
}

// probeFailReason maps a free-form probe Reason string to one of the bounded
// label values documented on IncNodeProbeError. Probes in the probe package
// use specific English phrases; anything unrecognised falls through to
// "unknown".
func probeFailReason(reason string) string {
	switch reason {
	case "binary not found":
		return "binary_missing"
	case "exec failed":
		return "exec_failed"
	case "timeout":
		return "timeout"
	case "permission denied":
		return "permission_denied"
	default:
		return "unknown"
	}
}

// logChanges prints a single stderr line for every backend whose availability
// changed between the previous and current probe cycles.
func logChanges(previous, current []probe.CapabilityResult) {
	if len(previous) == 0 {
		// First cycle — log the initial state instead of a diff.
		for _, r := range current {
			fmt.Fprintf(os.Stderr, "runtime-agent: initial probe: backend=%s available=%v reason=%q\n",
				r.Backend, r.Available, r.Reason)
		}
		return
	}

	// Build a previous-state index.
	prev := make(map[string]bool, len(previous))
	for _, r := range previous {
		prev[r.Backend] = r.Available
	}

	for _, r := range current {
		if wasAvail, ok := prev[r.Backend]; ok && wasAvail != r.Available {
			fmt.Fprintf(os.Stderr,
				"runtime-agent: availability changed: backend=%s available=%v reason=%q\n",
				r.Backend, r.Available, r.Reason)
		}
	}
}

// filterProbes returns only those probes whose Name() matches an enabled
// backend in cfg. Ordering within the returned slice mirrors the ordering of
// allProbes so probe output is deterministic.
func filterProbes(allProbes []probe.Probe, cfg *runtime.RuntimeConfig) []probe.Probe {
	enabled := cfg.EnabledBackends()
	set := make(map[string]struct{}, len(enabled))
	for _, b := range enabled {
		set[b] = struct{}{}
	}

	out := make([]probe.Probe, 0, len(allProbes))
	for _, p := range allProbes {
		if _, ok := set[p.Name()]; ok {
			out = append(out, p)
		}
	}
	return out
}
