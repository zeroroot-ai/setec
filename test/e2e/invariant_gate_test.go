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

package e2e

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
	"github.com/zeroroot-ai/setec/internal/status"
)

// nodeAgentDSName resolves the chart's node-agent DaemonSet name.
func nodeAgentDSName(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("kubectl",
		"-n", testNamespace,
		"get", "daemonset",
		"-l", "app.kubernetes.io/component=node-agent",
		"-o", "jsonpath={.items[0].metadata.name}",
	).CombinedOutput()
	if err != nil || len(out) == 0 {
		t.Fatalf("resolve node-agent DaemonSet: %v: %s", err, out)
	}
	return strings.TrimSpace(string(out))
}

// setNodeAgentFlag rewrites one --flag=value argument on the
// node-agent container and waits for the DaemonSet rollout. Returns a
// restore function for t.Cleanup.
func setNodeAgentFlag(t *testing.T, flag, from, to string) func() {
	t.Helper()
	ds := nodeAgentDSName(t)

	patch := func(old, new string) {
		t.Helper()
		out, err := exec.Command("kubectl",
			"-n", testNamespace,
			"get", "daemonset", ds,
			"-o", "jsonpath={.spec.template.spec.containers[0].args}",
		).CombinedOutput()
		if err != nil {
			t.Fatalf("read node-agent args: %v: %s", err, out)
		}
		args := string(out)
		if !strings.Contains(args, flag+"="+old) {
			t.Fatalf("node-agent args %s do not carry %s=%s", args, flag, old)
		}
		// Index of the arg in the JSON-ish list output.
		idx := -1
		for i, a := range strings.Split(strings.Trim(args, "[]\""), "\",\"") {
			if a == flag+"="+old {
				idx = i
				break
			}
		}
		if idx < 0 {
			t.Fatalf("could not locate %s=%s in node-agent args %s", flag, old, args)
		}
		jsonPatch := fmt.Sprintf(
			`[{"op":"replace","path":"/spec/template/spec/containers/0/args/%d","value":"%s=%s"}]`,
			idx, flag, new)
		if out, err := exec.Command("kubectl",
			"-n", testNamespace,
			"patch", "daemonset", ds,
			"--type=json", "-p", jsonPatch,
		).CombinedOutput(); err != nil {
			t.Fatalf("patch node-agent args: %v: %s", err, out)
		}
		if out, err := exec.Command("kubectl",
			"-n", testNamespace,
			"rollout", "status", "daemonset/"+ds, "--timeout=5m",
		).CombinedOutput(); err != nil {
			t.Fatalf("node-agent rollout after %s=%s: %v: %s", flag, new, err, out)
		}
	}

	patch(from, to)
	return func() { patch(to, from) }
}

// TestGate_UnverifiedWarmStartFailsClosed is the setec#191 acceptance
// e2e: artificially suppress one ADR-0005 per-restore verification
// (the entropy reseed, via the node-agent's --entropy-reseed=off
// opt-out) and assert the invariant gate fails the restore CLOSED in a
// non-dev namespace — the Sandbox is destroyed with the typed
// InvariantGateViolation reason, never served — and that the ordinary
// cold-boot fallback still works while the gate is active.
func TestGate_UnverifiedWarmStartFailsClosed(t *testing.T) {
	if !envtestOK(t) || !phase3Enabled(t) {
		t.Skip("invariant-gate E2E requires snapshots.enabled=true")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	// Step 0: suppress the reseed verification node-side. The node
	// will now report warm starts with entropy_reseeded=false.
	restore := setNodeAgentFlag(t, "--entropy-reseed", "require", "off")
	t.Cleanup(restore)

	const poolImage = "docker.io/library/alpine:3.19"
	clsName := fmt.Sprintf("e2e-gate-%d", time.Now().Unix())
	cls := newSandboxClass(clsName, setecv1alpha1.SandboxClassSpec{
		Runtime:         &setecv1alpha1.SandboxClassRuntime{Backend: "kata-fc"},
		PreWarmPoolSize: 1,
		PreWarmImage:    poolImage,
		PreWarmTTL:      &metav1.Duration{Duration: time.Hour},
		DefaultResources: &setecv1alpha1.Resources{
			VCPU:   1,
			Memory: resource.MustParse("256Mi"),
		},
	})
	if err := k8sClient.Create(ctx, cls); err != nil {
		t.Fatalf("create SandboxClass: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), &setecv1alpha1.SandboxClass{
			ObjectMeta: metav1.ObjectMeta{Name: clsName},
		})
	})

	// Step 1: wait for the pool to build (same observable as the
	// warm-start lifecycle e2e).
	buildDeadline := time.Now().Add(6 * time.Minute)
	for {
		scrapeCtx, scrapeCancel := context.WithTimeout(ctx, 30*time.Second)
		families, err := scrapeNodeAgentMetrics(scrapeCtx)
		scrapeCancel()
		if err == nil {
			if n, ok := poolEntriesGauge(families, clsName); ok && n >= 1 {
				break
			}
		}
		if time.Now().After(buildDeadline) {
			t.Fatalf("pool for class %q did not reach 1 entry within 6m (last scrape err: %v)", clsName, err)
		}
		time.Sleep(5 * time.Second)
	}

	// Step 2: a pool-eligible Sandbox in a non-dev namespace. The node
	// restores the entry but cannot verify the reseed, so the gate
	// must fail closed: Sandbox Failed/InvariantGateViolation with a
	// Rejected warm-start outcome, and the Pod destroyed.
	ns := "gate-nondev"
	createTenantNamespace(ctx, t, ns)
	sb := &setecv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "unverified"},
		Spec: setecv1alpha1.SandboxSpec{
			SandboxClassName: clsName,
			Image:            poolImage,
			Command:          []string{"sh", "-c", "sleep 300"},
			Resources: setecv1alpha1.Resources{
				VCPU:   1,
				Memory: resource.MustParse("256Mi"),
			},
		},
	}
	if err := k8sClient.Create(ctx, sb); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}

	deadline := time.Now().Add(5 * time.Minute)
	for {
		got := &setecv1alpha1.Sandbox{}
		err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: sb.Name}, got)
		if err == nil && got.Status.Phase == setecv1alpha1.SandboxPhaseFailed {
			if got.Status.Reason != status.ReasonInvariantGateViolation {
				t.Fatalf("Failed with reason %q, want %q", got.Status.Reason, status.ReasonInvariantGateViolation)
			}
			if got.Status.WarmStart == nil || got.Status.WarmStart.Outcome != setecv1alpha1.SandboxWarmStartRejected {
				t.Fatalf("warmStart = %+v, want outcome Rejected", got.Status.WarmStart)
			}
			break
		}
		// A Running phase would mean the gate served an unverified
		// restore — the exact failure this test exists to catch.
		if err == nil && got.Status.Phase == setecv1alpha1.SandboxPhaseRunning &&
			got.Status.WarmStart != nil &&
			got.Status.WarmStart.Outcome == setecv1alpha1.SandboxWarmStartPoolRestored {
			t.Fatal("gate served a warm start whose reseed verification was suppressed")
		}
		if time.Now().After(deadline) {
			t.Fatalf("sandbox never reached Failed/%s (last: %+v)", status.ReasonInvariantGateViolation, got.Status)
		}
		time.Sleep(3 * time.Second)
	}

	// The typed event must be on the record.
	evOut, _ := exec.Command("kubectl",
		"-n", ns, "get", "events",
		"--field-selector", "reason=InvariantGateViolation",
		"-o", "jsonpath={.items[*].reason}",
	).CombinedOutput()
	if !strings.Contains(string(evOut), "InvariantGateViolation") {
		t.Fatalf("no InvariantGateViolation event recorded, got: %s", evOut)
	}

	// Step 3: cold-boot fallback still works with the gate active. The
	// single pool entry was consumed by the rejected claim, so this
	// Sandbox misses the pool and must cold-boot to Running — the gate
	// only refuses unverified RESTORES, never a cold boot.
	cold := &setecv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "coldboot"},
		Spec: setecv1alpha1.SandboxSpec{
			SandboxClassName: clsName,
			Image:            poolImage,
			Command:          []string{"sh", "-c", "sleep 60"},
			Resources: setecv1alpha1.Resources{
				VCPU:   1,
				Memory: resource.MustParse("256Mi"),
			},
		},
	}
	if err := k8sClient.Create(ctx, cold); err != nil {
		t.Fatalf("create cold-boot sandbox: %v", err)
	}
	waitForPhaseCtx(ctx, t, ns, cold.Name, setecv1alpha1.SandboxPhaseRunning, 3*time.Minute)

	got := &setecv1alpha1.Sandbox{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: cold.Name}, got); err != nil {
		t.Fatalf("get cold-boot sandbox: %v", err)
	}
	if got.Status.WarmStart != nil && got.Status.WarmStart.Outcome == setecv1alpha1.SandboxWarmStartPoolRestored {
		t.Fatal("cold-boot sandbox unexpectedly warm-started from a pool that should be empty")
	}
}
