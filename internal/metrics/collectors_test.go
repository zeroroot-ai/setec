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

package metrics

import (
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
)

// newTestCollectors wires a fresh registry per test so state does not
// leak across cases.
func newTestCollectors(t *testing.T) (*Collectors, *prometheus.Registry) {
	t.Helper()
	reg := prometheus.NewRegistry()
	return NewCollectorsWith(reg), reg
}

func TestRecordPhaseTransition(t *testing.T) {
	t.Parallel()
	c, _ := newTestCollectors(t)

	c.RecordPhaseTransition("tenant-a", "standard", setecv1alpha1.SandboxPhaseRunning)
	c.RecordPhaseTransition("tenant-a", "standard", setecv1alpha1.SandboxPhaseRunning)
	c.RecordPhaseTransition("tenant-b", "standard", setecv1alpha1.SandboxPhaseRunning)

	if got := testutil.ToFloat64(c.SandboxTotal.WithLabelValues(
		string(setecv1alpha1.SandboxPhaseRunning), "tenant-a", "standard")); got != 2 {
		t.Errorf("tenant-a Running counter = %v, want 2", got)
	}
	if got := testutil.ToFloat64(c.SandboxTotal.WithLabelValues(
		string(setecv1alpha1.SandboxPhaseRunning), "tenant-b", "standard")); got != 1 {
		t.Errorf("tenant-b Running counter = %v, want 1", got)
	}
}

func TestRecordDuration(t *testing.T) {
	t.Parallel()
	c, _ := newTestCollectors(t)

	c.RecordDuration("tenant-a", "standard", string(setecv1alpha1.SandboxPhaseRunning), 250*time.Millisecond)
	c.RecordDuration("tenant-a", "standard", string(setecv1alpha1.SandboxPhaseRunning), 500*time.Millisecond)

	// Use CollectAndCount on the metric family to confirm observations
	// landed. testutil.CollectAndCount reports one series per unique
	// label combo; we should see exactly one here.
	if got, want := testutil.CollectAndCount(c.SandboxDuration), 1; got != want {
		t.Errorf("CollectAndCount = %d, want %d", got, want)
	}
}

func TestRecordColdStart(t *testing.T) {
	t.Parallel()
	c, _ := newTestCollectors(t)

	// RecordColdStart (deprecated) duplicates vmm into both runtime and vmm labels.
	c.RecordColdStart("firecracker", "standard", 3*time.Second)

	if got, want := testutil.CollectAndCount(c.SandboxColdStart), 1; got != want {
		t.Errorf("CollectAndCount = %d, want %d", got, want)
	}
}

func TestObserveColdStart(t *testing.T) {
	t.Parallel()
	c, _ := newTestCollectors(t)

	// ObserveColdStart carries distinct runtime and vmm labels.
	c.ObserveColdStart("firecracker", "firecracker", "standard", 3*time.Second)
	c.ObserveColdStart("kata", "kata", "gpu", 5*time.Second)

	if got, want := testutil.CollectAndCount(c.SandboxColdStart), 2; got != want {
		t.Errorf("CollectAndCount = %d, want %d", got, want)
	}
}

func TestIncFallback(t *testing.T) {
	t.Parallel()
	c, _ := newTestCollectors(t)

	c.IncFallback("kata", "firecracker")
	c.IncFallback("kata", "firecracker")
	c.IncFallback("gvisor", "native")

	if got := testutil.ToFloat64(c.FallbackTotal.WithLabelValues("kata", "firecracker")); got != 2 {
		t.Errorf("fallback kata→firecracker = %v, want 2", got)
	}
	if got := testutil.ToFloat64(c.FallbackTotal.WithLabelValues("gvisor", "native")); got != 1 {
		t.Errorf("fallback gvisor→native = %v, want 1", got)
	}
}

func TestSetNodeRuntimeAvailable(t *testing.T) {
	t.Parallel()
	c, _ := newTestCollectors(t)

	c.SetNodeRuntimeAvailable("firecracker", true)
	c.SetNodeRuntimeAvailable("kata", false)

	if got := testutil.ToFloat64(c.NodeRuntimeAvailable.WithLabelValues("firecracker")); got != 1 {
		t.Errorf("firecracker available = %v, want 1", got)
	}
	if got := testutil.ToFloat64(c.NodeRuntimeAvailable.WithLabelValues("kata")); got != 0 {
		t.Errorf("kata available = %v, want 0", got)
	}
}

func TestIncNodeProbeError(t *testing.T) {
	t.Parallel()
	c, _ := newTestCollectors(t)

	c.IncNodeProbeError("firecracker", "binary_missing")
	c.IncNodeProbeError("firecracker", "binary_missing")
	c.IncNodeProbeError("kata", "timeout")

	if got := testutil.ToFloat64(c.NodeRuntimeProbeErrors.WithLabelValues("firecracker", "binary_missing")); got != 2 {
		t.Errorf("probe errors firecracker/binary_missing = %v, want 2", got)
	}
	if got := testutil.ToFloat64(c.NodeRuntimeProbeErrors.WithLabelValues("kata", "timeout")); got != 1 {
		t.Errorf("probe errors kata/timeout = %v, want 1", got)
	}
}

func TestSetActive(t *testing.T) {
	t.Parallel()
	c, _ := newTestCollectors(t)

	c.SetActive("tenant-a", "standard", +3)
	c.SetActive("tenant-a", "standard", -1)

	if got := testutil.ToFloat64(c.SandboxActive.WithLabelValues("tenant-a", "standard")); got != 2 {
		t.Errorf("gauge = %v, want 2", got)
	}
}

// TestEmptyTenantLabel locks in the Requirement 5.4 behaviour: an unset
// tenant is recorded as an explicit empty string rather than a missing
// label.
func TestEmptyTenantLabel(t *testing.T) {
	t.Parallel()
	c, _ := newTestCollectors(t)

	c.RecordPhaseTransition("", "standard", setecv1alpha1.SandboxPhaseRunning)
	if got := testutil.ToFloat64(c.SandboxTotal.WithLabelValues(
		string(setecv1alpha1.SandboxPhaseRunning), "", "standard")); got != 1 {
		t.Errorf("empty-tenant counter = %v, want 1", got)
	}
}

// TestNilReceiverNoPanic verifies the defensive nil guards on each Record*
// method; callers in error paths must never crash the operator.
func TestNilReceiverNoPanic(t *testing.T) {
	t.Parallel()
	var c *Collectors

	// None of the following must panic.
	c.RecordPhaseTransition("", "", setecv1alpha1.SandboxPhasePending)
	c.RecordDuration("", "", "", time.Second)
	c.RecordColdStart("", "", time.Second)
	c.ObserveColdStart("", "", "", time.Second)
	c.SetActive("", "", 1)
	c.IncFallback("", "")
	c.SetNodeRuntimeAvailable("", false)
	c.IncNodeProbeError("", "")
}

// TestCollectAndLint runs testutil.CollectAndFormat against every metric
// to verify the exposition-format output is parseable (Requirement 5.5).
func TestCollectAndLint(t *testing.T) {
	t.Parallel()
	c, reg := newTestCollectors(t)

	c.RecordPhaseTransition("tenant", "cls", setecv1alpha1.SandboxPhasePending)
	c.RecordDuration("tenant", "cls", string(setecv1alpha1.SandboxPhasePending), time.Millisecond)
	c.RecordColdStart("firecracker", "cls", time.Second)
	c.SetActive("tenant", "cls", 1)
	c.IncFallback("kata", "firecracker")
	c.SetNodeRuntimeAvailable("firecracker", true)
	c.IncNodeProbeError("firecracker", "binary_missing")

	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather(): %v", err)
	}

	want := []string{
		"setec_sandbox_total",
		"setec_sandbox_duration_seconds",
		"setec_sandbox_cold_start_seconds",
		"setec_sandbox_active",
		"setec_sandbox_fallback_total",
		"setec_node_runtime_available",
		"setec_node_runtime_probe_errors_total",
	}
	seen := map[string]bool{}
	for _, mf := range mfs {
		seen[mf.GetName()] = true
	}
	for _, w := range want {
		if !seen[w] {
			t.Errorf("metric %q missing from registry (saw %v)", w, keys(seen))
		}
	}
}

// keys renders the given map's keys deterministically for error messages.
func keys(m map[string]bool) string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return strings.Join(out, ",")
}
