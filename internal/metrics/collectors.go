// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

// Package metrics owns the Prometheus collectors the operator exposes on
// the controller-runtime metrics server. A single Collectors struct bundles
// the counters, histograms, and gauges so callers compose one dependency
// instead of four, and tests can swap the backing registry without
// reaching into global state.
//
// Label cardinality note: every metric uses a fixed label set
// (tenant, sandbox_class, phase, runtime). "tenant" is always the
// empty string in single-tenant mode to avoid the Prometheus anti-pattern
// of sometimes-present labels. Cardinality therefore scales with
// (tenants x classes x phases) = O(small) for typical deployments.
// Do NOT add high-cardinality labels such as Sandbox name or UID without a
// compelling reason.
//
// The runtime label value is always "launcher": the launcher is the only
// runtime (setec#198).
package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
)

// Label names used across every collector. Kept as exported constants so
// test assertions and downstream dashboard code reference a single source
// of truth.
const (
	LabelTenant       = "tenant"
	LabelSandboxClass = "sandbox_class"
	LabelPhase        = "phase"
	// LabelRuntime is the runtime of a Sandbox. Its one value is
	// "launcher" (setec#198).
	LabelRuntime = "runtime"
	// LabelOperation is the Phase 3 snapshot operation label:
	// "create", "restore", "delete", "pause", "resume".
	LabelOperation = "operation"
	// LabelNode is the node name label used for pool-fill gauges so
	// operators can pinpoint an under-provisioned node.
	LabelNode = "node"
)

// Collectors bundles the Phase 2+ metrics. Callers receive this via
// the reconciler's constructor; do not embed a *Collectors pointer into
// globals.
type Collectors struct {
	// SandboxTotal counts Sandbox phase transitions. Increments once
	// per observed transition — not per reconcile — so it approximates
	// the total number of sandboxes observed at each phase.
	SandboxTotal *prometheus.CounterVec

	// SandboxDuration observes the time a sandbox spent in each phase
	// (or in the whole reconcile, depending on caller semantics).
	SandboxDuration *prometheus.HistogramVec

	// SandboxColdStart observes the time from Sandbox creation to the
	// moment its Pod transitioned to Running, labeled by runtime and
	// class.
	SandboxColdStart *prometheus.HistogramVec

	// SandboxActive gauges the current number of active Sandboxes per
	// tenant and class. Driven by SetActive(tenant, class, delta).
	SandboxActive *prometheus.GaugeVec

	// SnapshotDuration observes the time a snapshot operation takes,
	// labeled by operation (create, restore, delete, pause, resume).
	// Phase 3 only.
	SnapshotDuration *prometheus.HistogramVec

	// WarmStartTotal counts warm-start attempts by outcome.
	// Bounded outcome values: "restored" (loaded a warm base),
	// "miss" (no Ready base; cold boot), "error" (the load failed;
	// cold boot).
	WarmStartTotal *prometheus.CounterVec

	// WarmPoolReady gauges the Ready bases of the launcher warm pool of
	// each SandboxClass (setec#103).
	WarmPoolReady *prometheus.GaugeVec

	// WarmPoolTarget gauges the bases the warm pool of each SandboxClass
	// wants: PreWarmPoolSize, or 0 for an idle or inactive pool. The
	// underfilled alert compares the two gauges.
	WarmPoolTarget *prometheus.GaugeVec
}

// NewCollectors constructs a fresh Collectors bundle and registers every
// collector with controller-runtime's metrics registry. Returns the
// bundle so callers can reuse it for Record* invocations. Registration
// uses MustRegister inside the package-private constructor (NewCollectors
// runs once at startup; a registration panic indicates a programming bug,
// not runtime input).
func NewCollectors() *Collectors {
	return NewCollectorsWith(ctrlmetrics.Registry)
}

// NewCollectorsWith is the same as NewCollectors but accepts a caller-owned
// registerer, enabling tests to isolate metric state per-test.
// controller-runtime's global registry is the default in production.
func NewCollectorsWith(reg prometheus.Registerer) *Collectors {
	c := &Collectors{
		SandboxTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "setec_sandbox_total",
				Help: "Total number of Sandbox phase transitions observed.",
			},
			[]string{LabelPhase, LabelTenant, LabelSandboxClass},
		),
		SandboxDuration: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "setec_sandbox_duration_seconds",
				Help:    "Time (s) spent in each Sandbox phase.",
				Buckets: prometheus.DefBuckets,
			},
			[]string{LabelPhase, LabelTenant, LabelSandboxClass},
		),
		SandboxColdStart: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "setec_sandbox_cold_start_seconds",
				Help:    "Time (s) from Sandbox creation to Pod Running.",
				Buckets: prometheus.ExponentialBuckets(0.1, 2, 12),
			},
			[]string{LabelRuntime, LabelSandboxClass},
		),
		SandboxActive: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "setec_sandbox_active",
				Help: "Number of currently active Sandboxes.",
			},
			[]string{LabelTenant, LabelSandboxClass},
		),
		SnapshotDuration: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "setec_snapshot_duration_seconds",
				Help:    "Time (s) spent in a snapshot operation (create, restore, delete, pause, resume).",
				Buckets: prometheus.ExponentialBuckets(0.01, 2, 14),
			},
			[]string{LabelOperation, LabelSandboxClass},
		),
		WarmStartTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "setec_warmstart_total",
				Help: "Pool warm-start attempts by outcome (restored, miss, error) and SandboxClass.",
			},
			[]string{"outcome", LabelSandboxClass},
		),
		WarmPoolReady: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "setec_warm_pool_ready_bases",
				Help: "Ready bases of the launcher warm pool of each SandboxClass.",
			},
			[]string{LabelSandboxClass},
		),
		WarmPoolTarget: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "setec_warm_pool_target_bases",
				Help: "Bases the launcher warm pool of each SandboxClass wants.",
			},
			[]string{LabelSandboxClass},
		),
	}

	if reg != nil {
		reg.MustRegister(
			c.SandboxTotal, c.SandboxDuration, c.SandboxColdStart, c.SandboxActive,
			c.SnapshotDuration, c.WarmStartTotal, c.WarmPoolReady, c.WarmPoolTarget,
		)
	}

	return c
}

// normalizeTenantLabel makes sure the tenant label is always present with
// a literal empty-string value when unset. This avoids the Prometheus
// label-cardinality anti-pattern of "sometimes label, sometimes absent".
func normalizeTenantLabel(tenant string) string {
	return tenant
}

// RecordPhaseTransition increments SandboxTotal for the given phase.
// Callers use this on the observed transition, not on every reconcile,
// so the counter's rate approximates transition throughput.
func (c *Collectors) RecordPhaseTransition(tenant, class string, phase setecv1alpha1.SandboxPhase) {
	if c == nil {
		return
	}
	c.SandboxTotal.WithLabelValues(string(phase), normalizeTenantLabel(tenant), class).Inc()
}

// RecordDuration observes the given duration into SandboxDuration.
// Phase is stringified by the caller so this function stays pure Go
// without importing the v1alpha1 phase enum at every call site.
func (c *Collectors) RecordDuration(tenant, class, phase string, d time.Duration) {
	if c == nil {
		return
	}
	c.SandboxDuration.WithLabelValues(phase, normalizeTenantLabel(tenant), class).Observe(d.Seconds())
}

// ObserveColdStart observes a Sandbox's time-to-Running into the cold-start
// histogram. runtime is always "launcher" (setec#198).
func (c *Collectors) ObserveColdStart(runtime, class string, d time.Duration) {
	if c == nil {
		return
	}
	c.SandboxColdStart.WithLabelValues(runtime, class).Observe(d.Seconds())
}

// SetActive adjusts the active-sandbox gauge by delta (positive on
// Pending→Running, negative on Running→Completed/Failed). The controller
// is responsible for the signed delta; this helper enforces no invariants
// beyond label normalization.
func (c *Collectors) SetActive(tenant, class string, delta int) {
	if c == nil {
		return
	}
	c.SandboxActive.WithLabelValues(normalizeTenantLabel(tenant), class).Add(float64(delta))
}

// RecordSnapshotDuration observes the given duration in the snapshot-
// operation histogram. operation is one of "create", "restore",
// "delete", "pause", "resume". Phase 3 only.
func (c *Collectors) RecordSnapshotDuration(operation, class string, d time.Duration) {
	if c == nil {
		return
	}
	c.SnapshotDuration.WithLabelValues(operation, class).Observe(d.Seconds())
}

// IncWarmStart increments the warm-start counter for a bounded
// outcome ("restored", "miss", "error") and SandboxClass. Called by
// the snapshot Coordinator after every pool warm-start attempt.
func (c *Collectors) IncWarmStart(outcome, class string) {
	if c == nil {
		return
	}
	c.WarmStartTotal.WithLabelValues(outcome, class).Inc()
}

// SetWarmPool sets the Ready bases and the wanted bases of the warm pool
// of a class.
func (c *Collectors) SetWarmPool(class string, ready, target int) {
	if c == nil {
		return
	}
	c.WarmPoolReady.WithLabelValues(class).Set(float64(ready))
	c.WarmPoolTarget.WithLabelValues(class).Set(float64(target))
}
