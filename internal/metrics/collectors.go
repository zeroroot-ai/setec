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

// Package metrics owns the Prometheus collectors the operator exposes on
// the controller-runtime metrics server. A single Collectors struct bundles
// the counters, histograms, and gauges so callers compose one dependency
// instead of four, and tests can swap the backing registry without
// reaching into global state.
//
// Label cardinality note: every metric uses a fixed label set
// (tenant, sandbox_class, phase, vmm, runtime). "tenant" is always the
// empty string in single-tenant mode to avoid the Prometheus anti-pattern
// of sometimes-present labels. Cardinality therefore scales with
// (tenants x classes x phases) = O(small) for typical deployments.
// Do NOT add high-cardinality labels such as Sandbox name or UID without a
// compelling reason.
//
// Runtime values are bounded: "firecracker", "kata", "gvisor", "native".
// Backend/reason values for probe-error metrics are also bounded — see
// IncNodeProbeError for the enumerated reason values.
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
	// Deprecated: LabelVMM is superseded by LabelRuntime. It is retained for
	// one release to preserve backward compatibility with existing dashboards
	// and will be removed in the next spec iteration. Use LabelRuntime for
	// all new code.
	LabelVMM = "vmm"
	// LabelRuntime is the canonical replacement for LabelVMM. Bounded values:
	// "firecracker", "kata", "gvisor", "native".
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
	// moment its Pod transitioned to Running. Both runtime and vmm labels
	// are present during the dual-write transition period so existing
	// dashboards keep working.
	SandboxColdStart *prometheus.HistogramVec

	// SandboxActive gauges the current number of active Sandboxes per
	// tenant and class. Driven by SetActive(tenant, class, delta).
	SandboxActive *prometheus.GaugeVec

	// SnapshotDuration observes the time a snapshot operation takes,
	// labeled by operation (create, restore, delete, pause, resume).
	// Phase 3 only.
	SnapshotDuration *prometheus.HistogramVec

	// PoolFill gauges the number of pre-warmed pool entries currently
	// paused on a given node for a given SandboxClass. Populated by
	// the node-agent pool manager and exposed via the node-agent
	// metrics endpoint. Phase 3 only.
	PoolFill *prometheus.GaugeVec

	// FallbackTotal counts runtime fallback events. Labels:
	//   from — the runtime that was attempted (bounded: see LabelRuntime)
	//   to   — the runtime that was substituted (bounded: see LabelRuntime)
	FallbackTotal *prometheus.CounterVec

	// NodeRuntimeAvailable gauges whether a given runtime is available on
	// the node (1 = available, 0 = unavailable). Label: runtime (bounded).
	NodeRuntimeAvailable *prometheus.GaugeVec

	// NodeRuntimeProbeErrors counts probe failures per backend and reason.
	// Labels:
	//   backend — runtime backend being probed (bounded: see LabelRuntime)
	//   reason  — failure category; bounded values:
	//               "binary_missing"    — executable not found on node
	//               "exec_failed"       — binary present but execution failed
	//               "timeout"           — probe did not complete within deadline
	//               "permission_denied" — insufficient privilege to run probe
	//               "unknown"           — uncategorised error (catch-all)
	NodeRuntimeProbeErrors *prometheus.CounterVec
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
		// Dual-write transition: both "runtime" (new canonical label) and
		// "vmm" (deprecated) are present until the next spec removes vmm.
		SandboxColdStart: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "setec_sandbox_cold_start_seconds",
				Help:    "Time (s) from Sandbox creation to Pod Running.",
				Buckets: prometheus.ExponentialBuckets(0.1, 2, 12),
			},
			[]string{LabelRuntime, LabelVMM, LabelSandboxClass},
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
		PoolFill: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "setec_prewarm_pool_entries",
				Help: "Number of pre-warmed pool entries currently paused on a node for a class.",
			},
			[]string{LabelNode, LabelSandboxClass},
		),
		FallbackTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "setec_sandbox_fallback_total",
				Help: "Total number of runtime fallback events (from one backend to another).",
			},
			[]string{"from", "to"},
		),
		NodeRuntimeAvailable: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "setec_node_runtime_available",
				Help: "Whether a runtime backend is available on this node (1 = available, 0 = unavailable).",
			},
			[]string{LabelRuntime},
		),
		NodeRuntimeProbeErrors: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "setec_node_runtime_probe_errors_total",
				Help: "Total number of runtime probe errors. See label 'reason' for bounded failure categories.",
			},
			[]string{"backend", "reason"},
		),
	}

	if reg != nil {
		reg.MustRegister(
			c.SandboxTotal, c.SandboxDuration, c.SandboxColdStart, c.SandboxActive,
			c.SnapshotDuration, c.PoolFill,
			c.FallbackTotal, c.NodeRuntimeAvailable, c.NodeRuntimeProbeErrors,
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

// RecordColdStart observes a Sandbox's time-to-Running into the cold-start
// histogram. vmm is the SandboxClass.spec.vmm value (or the operator's
// default) and class is the SandboxClass name (or empty string).
//
// Deprecated: use ObserveColdStart which carries both the new runtime label
// and the legacy vmm label. RecordColdStart duplicates the vmm value into
// the runtime label to maintain backward compatibility during the transition.
func (c *Collectors) RecordColdStart(vmm, class string, d time.Duration) {
	if c == nil {
		return
	}
	// Dual-write: runtime receives the same value as vmm so existing callers
	// continue working without change. New callers should use ObserveColdStart.
	c.SandboxColdStart.WithLabelValues(vmm, vmm, class).Observe(d.Seconds())
}

// ObserveColdStart observes a Sandbox's time-to-Running into the cold-start
// histogram with explicit runtime and vmm labels for the dual-write period.
// runtime is the canonical label (bounded: "firecracker", "kata", "gvisor",
// "native"). vmm is the legacy label value kept for dashboard compatibility.
// Pass the same string for both if only one is known.
func (c *Collectors) ObserveColdStart(runtime, vmm, class string, d time.Duration) {
	if c == nil {
		return
	}
	c.SandboxColdStart.WithLabelValues(runtime, vmm, class).Observe(d.Seconds())
}

// SetActive adjusts the active-sandbox gauge by delta (positive on
// Pending→Running, negative on Running→Completed/Failed). The controller
// is responsible for the signed delta; this helper enforces no invariants
// beyond label normalisation.
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

// SetPoolFill sets the pool-fill gauge for a given node/class pair.
// Phase 3 only; called by the node-agent pool manager after
// ReconcilePools.
func (c *Collectors) SetPoolFill(node, class string, entries int) {
	if c == nil {
		return
	}
	c.PoolFill.WithLabelValues(node, class).Set(float64(entries))
}

// IncFallback increments FallbackTotal for the given from/to runtime pair.
// Both from and to must be bounded runtime values: "firecracker", "kata",
// "gvisor", "native".
func (c *Collectors) IncFallback(from, to string) {
	if c == nil {
		return
	}
	c.FallbackTotal.WithLabelValues(from, to).Inc()
}

// SetNodeRuntimeAvailable sets the NodeRuntimeAvailable gauge for the given
// runtime to 1 (available) or 0 (unavailable). runtime must be a bounded
// value: "firecracker", "kata", "gvisor", "native".
func (c *Collectors) SetNodeRuntimeAvailable(runtime string, available bool) {
	if c == nil {
		return
	}
	v := 0.0
	if available {
		v = 1.0
	}
	c.NodeRuntimeAvailable.WithLabelValues(runtime).Set(v)
}

// IncNodeProbeError increments NodeRuntimeProbeErrors for the given backend
// and reason. backend is a bounded runtime value; reason must be one of:
//
//	"binary_missing"    — executable not found on node
//	"exec_failed"       — binary present but execution failed
//	"timeout"           — probe did not complete within deadline
//	"permission_denied" — insufficient privilege to run probe
//	"unknown"           — uncategorised error (catch-all)
func (c *Collectors) IncNodeProbeError(backend, reason string) {
	if c == nil {
		return
	}
	c.NodeRuntimeProbeErrors.WithLabelValues(backend, reason).Inc()
}
