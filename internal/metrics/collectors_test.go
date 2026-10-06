// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

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

func TestObserveColdStart(t *testing.T) {
	t.Parallel()
	c, _ := newTestCollectors(t)

	c.ObserveColdStart("launcher", "standard", 3*time.Second)
	c.ObserveColdStart("launcher", "gpu", 5*time.Second)

	if got, want := testutil.CollectAndCount(c.SandboxColdStart), 2; got != want {
		t.Errorf("CollectAndCount = %d, want %d", got, want)
	}
}

// TestSetWarmPool pins both gauges of the warm pool: the underfilled alert
// compares them, so a target that is never set makes the alert unable to
// fire.
func TestSetWarmPool(t *testing.T) {
	t.Parallel()
	c, _ := newTestCollectors(t)

	c.SetWarmPool("standard", 1, 3)

	if got := testutil.ToFloat64(c.WarmPoolReady.WithLabelValues("standard")); got != 1 {
		t.Errorf("ready bases = %v, want 1", got)
	}
	if got := testutil.ToFloat64(c.WarmPoolTarget.WithLabelValues("standard")); got != 3 {
		t.Errorf("target bases = %v, want 3", got)
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

// TestEmptyTenantLabel locks in the Requirement 5.4 behavior: an unset
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
	c.ObserveColdStart("", "", time.Second)
	c.SetActive("", "", 1)
	c.SetWarmPool("", 0, 0)
}

// TestCollectAndLint runs testutil.CollectAndFormat against every metric
// to verify the exposition-format output is parseable (Requirement 5.5).
func TestCollectAndLint(t *testing.T) {
	t.Parallel()
	c, reg := newTestCollectors(t)

	c.RecordPhaseTransition("tenant", "cls", setecv1alpha1.SandboxPhasePending)
	c.ObserveColdStart("firecracker", "cls", time.Second)
	c.SetActive("tenant", "cls", 1)

	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather(): %v", err)
	}

	want := []string{
		"setec_sandbox_total",
		"setec_sandbox_cold_start_seconds",
		"setec_sandbox_active",
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
