//go:build e2e

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

// runtime_backends_helpers_test.go contains shared helpers for the multi-backend
// E2E scenarios (tasks 25 and 25-runc). These helpers are wired to the
// package-level k8sClient and testNamespace from suite_test.go.

package e2e

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
)

// backendPollInterval is the cadence used by all polling helpers in the
// runtime-backends suite. It is intentionally shorter than defaultPoll so
// errors surface faster in CI.
const backendPollInterval = 1 * time.Second

// waitForPodReady polls until the named Pod in namespace reaches a terminal
// success state. It accepts two success conditions:
//
//   - PodReady condition == ConditionTrue (Running containers all healthy)
//   - Pod phase == Succeeded (short-lived workloads that exit 0 before the
//     first poll sees the Ready condition — common with fast runtimes like gvisor)
//
// Returns nil on success, a descriptive error on timeout.
// Uses wait.PollUntilContextTimeout — no bare time.Sleep.
func waitForPodReady(ctx context.Context, c client.Client, namespace, name string, timeout time.Duration) error {
	pollCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	return wait.PollUntilContextTimeout(pollCtx, backendPollInterval, timeout, true, func(ctx context.Context) (bool, error) {
		pod := &corev1.Pod{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, pod); err != nil {
			if apierrors.IsNotFound(err) {
				return false, nil // not yet created
			}
			return false, err
		}
		// Short-lived workloads (e.g. gvisor "sleep 5") may reach Succeeded
		// before waitForPodReady observes the transient Ready condition.
		if pod.Status.Phase == corev1.PodSucceeded {
			return true, nil
		}
		for _, cond := range pod.Status.Conditions {
			if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
				return true, nil
			}
		}
		return false, nil
	})
}

// waitForSandboxPhase polls until the Sandbox in the given namespace reaches
// the target phase, or timeout elapses. Returns the terminal Sandbox object.
func waitForSandboxPhase(ctx context.Context, c client.Client, namespace, name string, phase setecv1alpha1.SandboxPhase, timeout time.Duration) (*setecv1alpha1.Sandbox, error) {
	pollCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var last setecv1alpha1.Sandbox
	err := wait.PollUntilContextTimeout(pollCtx, backendPollInterval, timeout, true, func(ctx context.Context) (bool, error) {
		if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &last); err != nil {
			if apierrors.IsNotFound(err) {
				return false, nil
			}
			return false, err
		}
		return last.Status.Phase == phase, nil
	})
	if err != nil {
		return &last, fmt.Errorf("sandbox %s/%s did not reach phase %q within %s (last phase=%q): %w",
			namespace, name, phase, timeout, last.Status.Phase, err)
	}
	return &last, nil
}

// waitForSandboxRuntimeChosen polls until sb.Status.Runtime.Chosen is non-empty,
// then returns the chosen backend name.
func waitForSandboxRuntimeChosen(ctx context.Context, c client.Client, namespace, name string, timeout time.Duration) (string, error) {
	pollCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var chosen string
	err := wait.PollUntilContextTimeout(pollCtx, backendPollInterval, timeout, true, func(ctx context.Context) (bool, error) {
		sb := &setecv1alpha1.Sandbox{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, sb); err != nil {
			if apierrors.IsNotFound(err) {
				return false, nil
			}
			return false, err
		}
		if sb.Status.Runtime != nil && sb.Status.Runtime.Chosen != "" {
			chosen = sb.Status.Runtime.Chosen
			return true, nil
		}
		return false, nil
	})
	return chosen, err
}

// skipIfNodeLabelMissing skips t if no node in the cluster carries the given
// label key. The check uses kubectl to remain consistent with the rest of the
// E2E harness.
func skipIfNodeLabelMissing(t *testing.T, label string) {
	t.Helper()
	// kubectl get nodes -o jsonpath with a label containing dots requires the
	// dots to be escaped in the jsonpath expression.
	escaped := strings.ReplaceAll(label, ".", "\\.")
	out, err := exec.Command("kubectl", "get", "nodes",
		"-o", fmt.Sprintf("jsonpath={..labels.%s}", escaped),
	).CombinedOutput()
	if err != nil {
		t.Skipf("cannot query node labels (kubectl error: %v); skipping test requiring %q", err, label)
		return
	}
	if strings.TrimSpace(string(out)) == "" {
		t.Skipf("no node carries label %q; skipping test", label)
	}
}

// scrapeOperatorMetrics port-forwards the operator's metrics service and
// returns the parsed Prometheus metric families. The port-forward subprocess
// is killed when ctx is cancelled.
//
// The metrics service is expected to be named "setec-metrics" in testNamespace,
// listening on port 8080 (controller-runtime default).
func scrapeOperatorMetrics(ctx context.Context) (map[string]*dto.MetricFamily, error) {
	// Use a fixed local port; the port-forward is short-lived and scoped to
	// the test's context, so collisions are unlikely in practice.
	const localPort = "19090"

	pf := exec.CommandContext(ctx,
		"kubectl", "port-forward",
		"-n", testNamespace,
		"svc/setec-metrics",
		localPort+":8080",
	)
	pf.Stderr = io.Discard
	if err := pf.Start(); err != nil {
		return nil, fmt.Errorf("start port-forward: %w", err)
	}
	// Reap the process when it exits (context cancel closes the tunnel).
	go func() { _ = pf.Wait() }()

	// Poll until the port-forward tunnel accepts connections. A fixed sleep is
	// unreliable: on a loaded CI runner the tunnel may not be ready in 2 s, and
	// on a fast machine the sleep wastes time. We retry with a short backoff
	// instead, letting the caller's context supply the overall deadline.
	metricsURL := fmt.Sprintf("http://127.0.0.1:%s/metrics", localPort)
	var resp *http.Response
	if err := wait.PollUntilContextTimeout(ctx, 500*time.Millisecond, 15*time.Second, true, func(ctx context.Context) (bool, error) {
		r, e := http.Get(metricsURL) //nolint:noctx // short-lived polling probe; context applied at the outer level
		if e != nil {
			return false, nil // tunnel not ready yet; keep polling
		}
		resp = r
		return true, nil
	}); err != nil {
		return nil, fmt.Errorf("port-forward to svc/setec-metrics not ready within 15s: %w", err)
	}

	defer resp.Body.Close()

	var parser expfmt.TextParser
	families, err := parser.TextToMetricFamilies(bufio.NewReader(resp.Body))
	if err != nil && !isMetricParseEOF(err) {
		return nil, fmt.Errorf("parse prometheus text: %w", err)
	}
	return families, nil
}

// isMetricParseEOF reports whether the error from expfmt.TextParser is a
// benign EOF that should be treated as success. The text-format parser
// sometimes returns a non-nil error alongside a valid families map when the
// response body ends without a trailing newline.
func isMetricParseEOF(err error) bool {
	if err == nil {
		return true
	}
	s := err.Error()
	return strings.Contains(s, "EOF") || strings.Contains(s, "unexpected end")
}

// metricsHasRuntimeLabel returns true when the named metric family contains
// at least one sample with label "runtime" equal to runtimeValue.
func metricsHasRuntimeLabel(families map[string]*dto.MetricFamily, metricName, runtimeValue string) bool {
	mf, ok := families[metricName]
	if !ok {
		return false
	}
	for _, m := range mf.GetMetric() {
		for _, lp := range m.GetLabel() {
			if lp.GetName() == "runtime" && lp.GetValue() == runtimeValue {
				return true
			}
		}
	}
	return false
}

// fallbackCounterValue returns the current float64 value of
// setec_sandbox_fallback_total{from=<from>,to=<to>}, or 0.0 if no sample
// matches.
func fallbackCounterValue(families map[string]*dto.MetricFamily, from, to string) float64 {
	mf, ok := families["setec_sandbox_fallback_total"]
	if !ok {
		return 0
	}
	for _, m := range mf.GetMetric() {
		var gotFrom, gotTo string
		for _, lp := range m.GetLabel() {
			switch lp.GetName() {
			case "from":
				gotFrom = lp.GetValue()
			case "to":
				gotTo = lp.GetValue()
			}
		}
		if gotFrom == from && gotTo == to {
			if c := m.GetCounter(); c != nil {
				return c.GetValue()
			}
		}
	}
	return 0
}
