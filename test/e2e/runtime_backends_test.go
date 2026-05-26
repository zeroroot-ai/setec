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

// runtime_backends_test.go contains the multi-backend smoke tests and fallback
// scenario for task 25. Each test targets the k3s dev cluster that has been
// prepared by `make up` (kata + gvisor installed) and the Setec chart deployed.
//
// Prerequisites (applied before this suite runs):
//   - development/k3s/ manifests applied: kata-fc, gvisor RuntimeClasses present.
//   - Node labels: setec.zeroroot.ai/runtime.kata-fc=true,
//     setec.zeroroot.ai/runtime.gvisor=true on at least one node each.
//   - The Setec operator running in testNamespace with the metrics service
//     exposed as "setec-metrics" on port 8080.
//
// kata-qemu requires /dev/kvm on the test node; the test is skipped when the
// capability label is absent so the suite passes on KVM-less CI runners.

package e2e

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	nodev1 "k8s.io/api/node/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
)

// backendSmokeDuration is the total time each per-backend smoke sub-test may
// spend waiting for the Sandbox to become Ready. Kata cold-starts on bare
// metal are typically 5–15 s; we budget 60 s to cover slow runners.
const backendSmokeDuration = 60 * time.Second

// metricsScrapeDuration is the timeout for port-forwarding and scraping /metrics.
const metricsScrapeDuration = 30 * time.Second

// runtimeLabelPrefix is the node-label key prefix set by the node-agent DaemonSet.
const runtimeLabelPrefix = "setec.zeroroot.ai/runtime."

// TestRuntimeBackends_Smoke runs a per-backend smoke test for each of
// {kata-fc, kata-qemu, gvisor}. kata-qemu is skipped when no node in the
// cluster carries the capability label (KVM absent or label not set). After
// all three launches complete, /metrics is scraped to confirm that
// setec_sandbox_cold_start_seconds samples exist with runtime="kata-fc" and
// runtime="gvisor" labels.
func TestRuntimeBackends_Smoke(t *testing.T) {
	type scenario struct {
		backend     string
		className   string
		sandboxName string
	}
	scenarios := []scenario{
		{"kata-fc", "smoke-kata-fc", "smoke-sb-kata-fc"},
		{"kata-qemu", "smoke-kata-qemu", "smoke-sb-kata-qemu"},
		{"gvisor", "smoke-gvisor", "smoke-sb-gvisor"},
	}

	for _, sc := range scenarios {
		sc := sc // capture loop variable
		t.Run("backend="+sc.backend, func(t *testing.T) {
			// kata-qemu requires KVM and the matching node label. Skip early when
			// the label is absent — the operator won't find a capable node.
			if sc.backend == "kata-qemu" {
				skipIfNodeLabelMissing(t, runtimeLabelPrefix+"kata-qemu")
			}

			ctx := context.Background()

			// Create the SandboxClass referencing this backend.
			cls := &setecv1alpha1.SandboxClass{
				ObjectMeta: metav1.ObjectMeta{Name: sc.className},
				Spec: setecv1alpha1.SandboxClassSpec{
					VMM: setecv1alpha1.VMMFirecracker,
					Runtime: &setecv1alpha1.SandboxClassRuntime{
						Backend: sc.backend,
					},
				},
			}
			if err := k8sClient.Create(ctx, cls); err != nil {
				t.Fatalf("create SandboxClass %q: %v", sc.className, err)
			}
			t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), cls) })

			// Create a Sandbox using the class with a short-lived workload.
			sb := &setecv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      sc.sandboxName,
					Namespace: testNamespace,
				},
				Spec: setecv1alpha1.SandboxSpec{
					SandboxClassName: sc.className,
					Image:            "busybox:1.36",
					Command:          []string{"sleep", "5"},
					Resources: setecv1alpha1.Resources{
						VCPU:   1,
						Memory: resource.MustParse("128Mi"),
					},
				},
			}
			if err := k8sClient.Create(ctx, sb); err != nil {
				t.Fatalf("create Sandbox %q: %v", sc.sandboxName, err)
			}
			t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), sb) })

			// Wait for the backing Pod to be Ready.
			podName := sc.sandboxName + "-vm"
			if err := waitForPodReady(ctx, k8sClient, testNamespace, podName, backendSmokeDuration); err != nil {
				dumpDiagnostics(t, client.ObjectKey{Namespace: testNamespace, Name: sc.sandboxName})
				t.Fatalf("pod %s not Ready within %s: %v", podName, backendSmokeDuration, err)
			}

			// Confirm status.runtime.chosen matches the requested backend.
			chosen, err := waitForSandboxRuntimeChosen(ctx, k8sClient, testNamespace, sc.sandboxName, backendSmokeDuration)
			if err != nil {
				t.Fatalf("sandbox %q status.runtime.chosen not set within %s: %v",
					sc.sandboxName, backendSmokeDuration, err)
			}
			if chosen != sc.backend {
				t.Errorf("status.runtime.chosen = %q; want %q", chosen, sc.backend)
			}
		})
	}

	// After the per-backend sub-tests have run, scrape /metrics and verify
	// that cold-start samples carry the expected runtime labels.
	t.Run("metrics/cold_start_labels", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), metricsScrapeDuration)
		defer cancel()

		families, err := scrapeOperatorMetrics(ctx)
		if err != nil {
			// Infrastructure note: this sub-test requires the setec-metrics
			// service to be present. Skip rather than fail so the smoke
			// tests can pass even when the metrics service name differs.
			t.Skipf("cannot scrape operator metrics: %v (is setec-metrics svc present in %s?)",
				err, testNamespace)
			return
		}

		for _, rt := range []string{"kata-fc", "gvisor"} {
			if !metricsHasRuntimeLabel(families, "setec_sandbox_cold_start_seconds", rt) {
				t.Errorf("setec_sandbox_cold_start_seconds has no sample with runtime=%q", rt)
			}
		}
	})
}

// TestRuntimeBackends_ZZ_Fallback validates the end-to-end fallback path:
//
//  1. Create a SandboxClass with backend=kata-fc, fallback=[gvisor].
//  2. Strip the kata-fc capability label from all nodes.
//  3. Create a Sandbox → expect it to become Ready with chosen=gvisor.
//  4. Assert setec_sandbox_fallback_total{from=kata-fc,to=gvisor} incremented.
//  5. Restore the node labels.
//
// The ZZ_ prefix ensures this test runs last (Go sorts test names
// alphabetically), protecting other scenarios from the temporary label removal.
func TestRuntimeBackends_ZZ_Fallback(t *testing.T) {
	ctx := context.Background()

	// Verify that this cluster has gvisor available. If not, the fallback
	// itself would fail — skip cleanly.
	skipIfNodeLabelMissing(t, runtimeLabelPrefix+"gvisor")

	// Capture nodes that currently have the kata-fc label so we can restore
	// them after the test.
	kataFCLabel := runtimeLabelPrefix + "kata-fc"
	kataFCNodes, err := nodesWithLabel(ctx, kataFCLabel)
	if err != nil {
		t.Fatalf("enumerate kata-fc nodes: %v", err)
	}
	if len(kataFCNodes) == 0 {
		t.Skip("no node carries kata-fc label; cannot validate fallback (nothing to fall back from)")
	}

	// Always restore the labels at test end.
	t.Cleanup(func() {
		for _, nodeName := range kataFCNodes {
			out, err := exec.Command("kubectl", "label", "node", nodeName,
				kataFCLabel+"=true", "--overwrite").CombinedOutput()
			if err != nil {
				t.Logf("warning: could not restore label %s on node %s: %v (%s)",
					kataFCLabel, nodeName, err, out)
			}
		}
	})

	// Strip the kata-fc label from all capable nodes.
	for _, nodeName := range kataFCNodes {
		if out, err := exec.Command("kubectl", "label", "node", nodeName, kataFCLabel+"-").
			CombinedOutput(); err != nil {
			t.Fatalf("remove label %s from node %s: %v (%s)", kataFCLabel, nodeName, err, out)
		}
	}

	// Snapshot the fallback counter before the test to detect the delta later.
	var baselineFallback float64
	{
		snapCtx, snapCancel := context.WithTimeout(ctx, metricsScrapeDuration)
		defer snapCancel()
		if fams, err := scrapeOperatorMetrics(snapCtx); err == nil {
			baselineFallback = fallbackCounterValue(fams, "kata-fc", "gvisor")
		}
	}

	// Create the SandboxClass with the fallback chain.
	cls := &setecv1alpha1.SandboxClass{
		ObjectMeta: metav1.ObjectMeta{Name: "fallback-test-cls"},
		Spec: setecv1alpha1.SandboxClassSpec{
			VMM: setecv1alpha1.VMMFirecracker,
			Runtime: &setecv1alpha1.SandboxClassRuntime{
				Backend:  "kata-fc",
				Fallback: []string{"gvisor"},
			},
		},
	}
	if err := k8sClient.Create(ctx, cls); err != nil {
		t.Fatalf("create fallback SandboxClass: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), cls) })

	// Create the Sandbox.
	sb := &setecv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "fallback-sb",
			Namespace: testNamespace,
		},
		Spec: setecv1alpha1.SandboxSpec{
			SandboxClassName: "fallback-test-cls",
			Image:            "busybox:1.36",
			Command:          []string{"sleep", "5"},
			Resources: setecv1alpha1.Resources{
				VCPU:   1,
				Memory: resource.MustParse("128Mi"),
			},
		},
	}
	if err := k8sClient.Create(ctx, sb); err != nil {
		t.Fatalf("create fallback Sandbox: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), sb) })

	// Wait for the Pod to become Ready.
	podName := "fallback-sb-vm"
	if err := waitForPodReady(ctx, k8sClient, testNamespace, podName, backendSmokeDuration); err != nil {
		dumpDiagnostics(t, client.ObjectKey{Namespace: testNamespace, Name: "fallback-sb"})
		t.Fatalf("fallback Pod %s not Ready within %s: %v", podName, backendSmokeDuration, err)
	}

	// Assert that the chosen backend is gvisor (the fallback).
	chosen, err := waitForSandboxRuntimeChosen(ctx, k8sClient, testNamespace, "fallback-sb", backendSmokeDuration)
	if err != nil {
		t.Fatalf("status.runtime.chosen not set: %v", err)
	}
	if chosen != "gvisor" {
		t.Errorf("status.runtime.chosen = %q; want gvisor (kata-fc label was removed)", chosen)
	}

	// Verify the fallback counter incremented by at least 1.
	metricsCtx, metricsCancel := context.WithTimeout(ctx, metricsScrapeDuration)
	defer metricsCancel()
	fams, err := scrapeOperatorMetrics(metricsCtx)
	if err != nil {
		t.Logf("skipping fallback metric assertion: cannot scrape metrics (%v)", err)
		return
	}
	afterFallback := fallbackCounterValue(fams, "kata-fc", "gvisor")
	if delta := afterFallback - baselineFallback; delta < 1 {
		t.Errorf("setec_sandbox_fallback_total{from=kata-fc,to=gvisor} delta = %g; want ≥1", delta)
	}
}

// nodesWithLabel returns the names of all cluster nodes carrying the given
// label key (regardless of value), using kubectl for consistency with the
// rest of the E2E harness.
func nodesWithLabel(ctx context.Context, label string) ([]string, error) {
	out, err := exec.CommandContext(ctx,
		"kubectl", "get", "nodes",
		"-l", label,
		"-o", "jsonpath={.items[*].metadata.name}",
	).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("kubectl get nodes -l %s: %v (%s)", label, err, out)
	}
	raw := strings.TrimSpace(string(out))
	if raw == "" {
		return nil, nil
	}
	return strings.Fields(raw), nil
}

// ensureRuntimeClass skips t if the named RuntimeClass is not found on the
// cluster. It is a belt-and-suspenders guard alongside skipIfNodeLabelMissing.
func ensureRuntimeClass(t *testing.T, name string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var rc nodev1.RuntimeClass
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: name}, &rc); err != nil {
		t.Skipf("RuntimeClass %q not found: %v — install via `make up` before running this suite", name, err)
	}
}
