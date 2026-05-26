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

package main

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/prometheus/client_golang/prometheus"
	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
	"github.com/zeroroot-ai/setec/internal/metrics"
	internalruntime "github.com/zeroroot-ai/setec/internal/runtime"
	"github.com/zeroroot-ai/setec/internal/runtimeagent/probe"
)

// fakeProbe is a test double for probe.Probe that returns a predetermined
// result and counts the number of times Check is called.
type fakeProbe struct {
	name   string
	result probe.CapabilityResult
	calls  atomic.Int64
}

func (f *fakeProbe) Name() string { return f.name }

func (f *fakeProbe) Check(_ context.Context) probe.CapabilityResult {
	f.calls.Add(1)
	return f.result
}

// newTestScheme returns a scheme with the types needed for fake client setup.
func newTestScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = setecv1alpha1.AddToScheme(s)
	return s
}

// newTestNode returns a minimal Node object suitable for seeding the fake client.
func newTestNode(name string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: map[string]string{},
		},
	}
}

// newTestCollectors returns a Collectors bound to an isolated Prometheus
// registry so test assertions do not cross-contaminate.
func newTestCollectors() *metrics.Collectors {
	return metrics.NewCollectorsWith(prometheus.NewRegistry())
}

// TestRunSingleCycle verifies that a single probe cycle calls labels.Apply
// via the fake client and that each probe is invoked exactly once.
func TestRunSingleCycle(t *testing.T) {
	const nodeName = "test-node"

	// Seed the fake client with a Node.
	s := newTestScheme()
	node := newTestNode(nodeName)
	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(node).
		WithStatusSubresource(&corev1.Node{}).
		Build()

	// Two fake probes: one available, one not.
	kataProbe := &fakeProbe{name: "kata-qemu", result: probe.CapabilityResult{Available: true}}
	runcProbe := &fakeProbe{name: "runc", result: probe.CapabilityResult{Available: false, Reason: "binary not found"}}

	// Use a context that we cancel shortly after the first cycle completes.
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	deps := Dependencies{
		Client:     fakeClient,
		Probes:     []probe.Probe{kataProbe, runcProbe},
		NodeName:   nodeName,
		Interval:   10 * time.Second, // long enough that only the first cycle fires
		Collectors: newTestCollectors(),
	}

	Run(ctx, deps)

	// Each probe must have been called exactly once.
	if n := kataProbe.calls.Load(); n != 1 {
		t.Errorf("kataProbe called %d times, want 1", n)
	}
	if n := runcProbe.calls.Load(); n != 1 {
		t.Errorf("runcProbe called %d times, want 1", n)
	}

	// Verify the Node label was written by inspecting the fake client's state.
	var got corev1.Node
	if err := fakeClient.Get(context.Background(), nodeKey(nodeName), &got); err != nil {
		t.Fatalf("get node: %v", err)
	}
	want := map[string]string{
		"setec.zeroroot.ai/runtime.kata-qemu": "true",
		"setec.zeroroot.ai/runtime.runc":      "false",
	}
	for k, v := range want {
		if got.Labels[k] != v {
			t.Errorf("label %q = %q, want %q", k, got.Labels[k], v)
		}
	}
}

// TestRunTickerDrivenSecondCycle verifies that the ticker fires and a second
// probe cycle executes within the test window.
func TestRunTickerDrivenSecondCycle(t *testing.T) {
	const nodeName = "ticker-node"

	s := newTestScheme()
	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(newTestNode(nodeName)).
		WithStatusSubresource(&corev1.Node{}).
		Build()

	p := &fakeProbe{name: "runc", result: probe.CapabilityResult{Available: true}}

	// Ticker fires every 50 ms; allow enough time for at least 2 cycles.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	deps := Dependencies{
		Client:     fakeClient,
		Probes:     []probe.Probe{p},
		NodeName:   nodeName,
		Interval:   50 * time.Millisecond,
		Collectors: newTestCollectors(),
	}

	Run(ctx, deps)

	// At least the first cycle plus one ticker cycle must have fired.
	if n := p.calls.Load(); n < 2 {
		t.Errorf("probe called %d times, want >= 2 (first + at least one ticker)", n)
	}
}

// TestRunCtxCancelStopsLoop verifies that cancelling the context terminates
// Run promptly without blocking.
func TestRunCtxCancelStopsLoop(t *testing.T) {
	const nodeName = "cancel-node"

	s := newTestScheme()
	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(newTestNode(nodeName)).
		WithStatusSubresource(&corev1.Node{}).
		Build()

	p := &fakeProbe{name: "gvisor", result: probe.CapabilityResult{Available: true}}

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		defer close(done)
		deps := Dependencies{
			Client:     fakeClient,
			Probes:     []probe.Probe{p},
			NodeName:   nodeName,
			Interval:   1 * time.Hour, // would block indefinitely if cancel didn't work
			Collectors: newTestCollectors(),
		}
		Run(ctx, deps)
	}()

	// Cancel after giving the first cycle time to complete.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
		// expected
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return within 2s after ctx cancel")
	}
}

// TestFilterProbes verifies that filterProbes retains only enabled backends.
func TestFilterProbes(t *testing.T) {
	all := []probe.Probe{
		&fakeProbe{name: "kata-fc"},
		&fakeProbe{name: "kata-qemu"},
		&fakeProbe{name: "gvisor"},
		&fakeProbe{name: "runc"},
	}

	cfg := &internalruntime.RuntimeConfig{
		Runtimes: map[string]internalruntime.BackendConfig{
			"gvisor": {Enabled: true, RuntimeClassName: "gvisor"},
			"runc":   {Enabled: true, RuntimeClassName: "runc"},
			// kata-fc and kata-qemu intentionally absent / not enabled
		},
		Defaults: internalruntime.DefaultsConfig{
			Runtime: internalruntime.RuntimeDefaults{Backend: "gvisor"},
		},
	}

	filtered := filterProbes(all, cfg)
	if len(filtered) != 2 {
		t.Fatalf("filterProbes returned %d probes, want 2", len(filtered))
	}
	names := make([]string, len(filtered))
	for i, p := range filtered {
		names[i] = p.Name()
	}
	// Order must match the input slice (gvisor before runc).
	if names[0] != "gvisor" {
		t.Errorf("filtered[0] = %q, want gvisor", names[0])
	}
	if names[1] != "runc" {
		t.Errorf("filtered[1] = %q, want runc", names[1])
	}
}

// TestProbeFailReason validates the bounded reason mapping used for the
// Prometheus label.
func TestProbeFailReason(t *testing.T) {
	tests := []struct {
		reason string
		want   string
	}{
		{"binary not found", "binary_missing"},
		{"exec failed", "exec_failed"},
		{"timeout", "timeout"},
		{"permission denied", "permission_denied"},
		{"some other error", "unknown"},
		{"", "unknown"},
	}
	for _, tt := range tests {
		got := probeFailReason(tt.reason)
		if got != tt.want {
			t.Errorf("probeFailReason(%q) = %q, want %q", tt.reason, got, tt.want)
		}
	}
}

// TestConditionMessageContainsBackends is an integration-flavoured test that
// verifies the Node condition message set by a single probe cycle contains the
// expected backend keys (JSON round-trip).
func TestConditionMessageContainsBackends(t *testing.T) {
	const nodeName = "cond-node"

	s := newTestScheme()
	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(newTestNode(nodeName)).
		WithStatusSubresource(&corev1.Node{}).
		Build()

	probes := []probe.Probe{
		&fakeProbe{name: "gvisor", result: probe.CapabilityResult{Available: true}},
		&fakeProbe{name: "runc", result: probe.CapabilityResult{Available: true}},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	deps := Dependencies{
		Client:     fakeClient,
		Probes:     probes,
		NodeName:   nodeName,
		Interval:   10 * time.Second,
		Collectors: newTestCollectors(),
	}
	Run(ctx, deps)

	var got corev1.Node
	if err := fakeClient.Get(context.Background(), nodeKey(nodeName), &got); err != nil {
		t.Fatalf("get node: %v", err)
	}

	// Find the SetecRuntimes condition.
	var condMsg string
	for _, cond := range got.Status.Conditions {
		if cond.Type == "SetecRuntimes" {
			condMsg = cond.Message
			break
		}
	}
	if condMsg == "" {
		t.Fatal("SetecRuntimes condition not found on node")
	}

	// Condition message is a JSON object — verify expected keys are present.
	var msg map[string]json.RawMessage
	if err := json.Unmarshal([]byte(condMsg), &msg); err != nil {
		t.Fatalf("condition message is not valid JSON: %v (got %q)", err, condMsg)
	}
	for _, backend := range []string{"gvisor", "runc"} {
		if _, ok := msg[backend]; !ok {
			t.Errorf("condition message missing backend %q; full message: %s", backend, condMsg)
		}
	}
}

// nodeKey returns the NamespacedName for a cluster-scoped Node object.
func nodeKey(name string) types.NamespacedName {
	return types.NamespacedName{Name: name}
}
