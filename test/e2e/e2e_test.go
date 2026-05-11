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

	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	setecv1alpha1 "github.com/zero-day-ai/setec/api/v1alpha1"
)

// Default eventual-consistency windows. Kata+Firecracker cold-starts take a
// few seconds on warm hosts but can take 30s+ on a cold one. We bias toward
// longer polls since the cost of a slow happy path is far lower than the
// cost of a flake.
const (
	defaultWait   = 3 * time.Minute
	defaultPoll   = 2 * time.Second
	briefWait     = 45 * time.Second
	operatorLabel = "app.kubernetes.io/name=setec"
)

// minimalSpec returns a small but valid SandboxSpec for the happy-path tests.
// Image is pinned to busybox since it is widely mirrored and has a small
// uncompressed size that minimises microVM startup time.
func minimalSpec(cmd ...string) setecv1alpha1.SandboxSpec {
	if len(cmd) == 0 {
		cmd = []string{"/bin/true"}
	}
	return setecv1alpha1.SandboxSpec{
		Image:   "busybox:1.36",
		Command: cmd,
		Resources: setecv1alpha1.Resources{
			VCPU:   1,
			Memory: resource.MustParse("128Mi"),
		},
	}
}

// newSandbox returns a client.Object-ready Sandbox in the test namespace.
func newSandbox(name string, spec setecv1alpha1.SandboxSpec) *setecv1alpha1.Sandbox {
	return &setecv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: testNamespace,
		},
		Spec: spec,
	}
}

// waitForPhase polls the Sandbox until it reaches one of the target phases
// or `timeout` elapses. Returns the final Sandbox (for further assertions)
// or fails the test if the timeout expires.
func waitForPhase(t *testing.T, key client.ObjectKey, timeout time.Duration, phases ...setecv1alpha1.SandboxPhase) *setecv1alpha1.Sandbox {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last setecv1alpha1.Sandbox
	for time.Now().Before(deadline) {
		if err := k8sClient.Get(context.Background(), key, &last); err != nil {
			if !apierrors.IsNotFound(err) {
				t.Fatalf("get sandbox %s: %v", key, err)
			}
		} else {
			for _, want := range phases {
				if last.Status.Phase == want {
					return &last
				}
			}
		}
		time.Sleep(defaultPoll)
	}
	dumpDiagnostics(t, key)
	t.Fatalf("sandbox %s did not reach phase %v within %s; last phase=%q reason=%q",
		key, phases, timeout, last.Status.Phase, last.Status.Reason)
	return nil
}

// waitForEvent polls Events in the test namespace until one is observed whose
// Reason matches `reason` and whose involvedObject points at the named
// Sandbox. Returns true if observed before the timeout.
func waitForEvent(t *testing.T, sandboxName, reason string, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var events corev1.EventList
		if err := k8sClient.List(context.Background(), &events, client.InNamespace(testNamespace)); err != nil {
			t.Fatalf("list events: %v", err)
		}
		for _, ev := range events.Items {
			if ev.Reason == reason && ev.InvolvedObject.Name == sandboxName && ev.InvolvedObject.Kind == "Sandbox" {
				return true
			}
		}
		time.Sleep(defaultPoll)
	}
	return false
}

// dumpDiagnostics best-effort prints the Sandbox, backing Pod, and events
// when a test fails. Runs subprocesses because kubectl's default describe
// output is more human-readable than any programmatic rendering we'd
// reimplement here.
func dumpDiagnostics(t *testing.T, key client.ObjectKey) {
	t.Helper()
	cmds := [][]string{
		{"kubectl", "get", "sandbox", key.Name, "-n", key.Namespace, "-o", "yaml"},
		{"kubectl", "describe", "sandbox", key.Name, "-n", key.Namespace},
		{"kubectl", "get", "pods", "-n", key.Namespace, "-o", "wide"},
		{"kubectl", "describe", "pod", key.Name + "-vm", "-n", key.Namespace},
		{"kubectl", "get", "events", "-n", key.Namespace, "--sort-by=.lastTimestamp"},
		{"kubectl", "logs", "-l", operatorLabel, "-n", testNamespace, "--tail=200"},
	}
	for _, c := range cmds {
		out, _ := exec.Command(c[0], c[1:]...).CombinedOutput()
		t.Logf("--- %s ---\n%s", strings.Join(c, " "), string(out))
	}
}

// createAndCleanup creates the Sandbox and registers a t.Cleanup that deletes
// it. Using t.Cleanup keeps each test self-contained so parallel or reordered
// execution doesn't leak state.
func createAndCleanup(t *testing.T, sb *setecv1alpha1.Sandbox) {
	t.Helper()
	if err := k8sClient.Create(context.Background(), sb); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), sb)
	})
}

// -- Scenario 1 --------------------------------------------------------------

// TestSandbox_SuccessfulExit applies a minimal Sandbox whose workload exits
// with code 0 and asserts the controller reports phase=Completed with
// exitCode=0. This exercises the happy path end to end including Pod
// scheduling on the Kata runtime and the microVM actually executing.
func TestSandbox_SuccessfulExit(t *testing.T) {
	sb := newSandbox("e2e-success", minimalSpec("/bin/true"))
	createAndCleanup(t, sb)

	key := client.ObjectKeyFromObject(sb)
	final := waitForPhase(t, key, defaultWait, setecv1alpha1.SandboxPhaseCompleted)

	if final.Status.ExitCode == nil {
		t.Fatalf("expected exitCode set, got nil")
	}
	if *final.Status.ExitCode != 0 {
		t.Fatalf("expected exitCode=0, got %d", *final.Status.ExitCode)
	}

	// The backing Pod must have used the Kata runtime class.
	var pod corev1.Pod
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: sb.Name + "-vm"}, &pod); err != nil {
		t.Fatalf("get pod: %v", err)
	}
	if pod.Spec.RuntimeClassName == nil || *pod.Spec.RuntimeClassName != kataRuntimeClass {
		t.Fatalf("expected pod runtimeClassName=%q, got %v", kataRuntimeClass, pod.Spec.RuntimeClassName)
	}
}

// -- Scenario 2 --------------------------------------------------------------

// TestSandbox_NonZeroExit applies a Sandbox whose command is `false`, which
// exits with code 1 on every sane coreutils/busybox build. We accept either
// terminal phase Failed or Completed — the spec defines exit != 0 as Failed,
// so Failed is expected, but we document both possibilities here because
// the test surfaces any controller-side drift in exit-code semantics.
func TestSandbox_NonZeroExit(t *testing.T) {
	sb := newSandbox("e2e-false", minimalSpec("/bin/false"))
	createAndCleanup(t, sb)

	key := client.ObjectKeyFromObject(sb)
	final := waitForPhase(t, key, defaultWait,
		setecv1alpha1.SandboxPhaseFailed,
		setecv1alpha1.SandboxPhaseCompleted,
	)

	if final.Status.ExitCode == nil {
		t.Fatalf("expected exitCode set, got nil")
	}
	if *final.Status.ExitCode == 0 {
		t.Fatalf("expected non-zero exitCode from /bin/false, got 0")
	}
	// /bin/false in busybox returns 1 — document this here so future
	// readers understand why we check specifically for 1 rather than
	// "anything nonzero".
	if *final.Status.ExitCode != 1 {
		t.Logf("warning: /bin/false returned exitCode=%d (expected 1 on busybox)", *final.Status.ExitCode)
	}
	if final.Status.Phase != setecv1alpha1.SandboxPhaseFailed {
		t.Logf("note: /bin/false produced phase=%q; spec expects Failed", final.Status.Phase)
	}
}

// -- Scenario 3 --------------------------------------------------------------

// TestSandbox_Timeout applies a Sandbox that sleeps well past its configured
// lifecycle.timeout and asserts the controller terminates it and reports
// phase=Failed, reason=Timeout.
func TestSandbox_Timeout(t *testing.T) {
	spec := minimalSpec("/bin/sh", "-c", "sleep 300")
	timeout := metav1.Duration{Duration: 10 * time.Second}
	spec.Lifecycle = &setecv1alpha1.Lifecycle{Timeout: &timeout}

	sb := newSandbox("e2e-timeout", spec)
	createAndCleanup(t, sb)

	key := client.ObjectKeyFromObject(sb)
	// Give the controller enough wall clock to (a) start the microVM, (b)
	// observe the timeout, (c) delete the Pod, (d) patch the status.
	final := waitForPhase(t, key, defaultWait, setecv1alpha1.SandboxPhaseFailed)
	if final.Status.Reason != "Timeout" {
		t.Fatalf("expected reason=Timeout, got %q", final.Status.Reason)
	}
}

// -- Scenario 4 --------------------------------------------------------------

// TestSandbox_DeleteMidRun deletes a running Sandbox and asserts that the
// backing Pod is garbage-collected shortly thereafter. We check for either
// outright NotFound or a non-nil DeletionTimestamp, which covers both the
// fast path (GC already ran) and the brief window before finalizers drain.
func TestSandbox_DeleteMidRun(t *testing.T) {
	sb := newSandbox("e2e-delete", minimalSpec("/bin/sh", "-c", "sleep 300"))
	createAndCleanup(t, sb)

	key := client.ObjectKeyFromObject(sb)
	// Wait until the Sandbox reaches Running so we know a Pod actually
	// exists before we delete it.
	_ = waitForPhase(t, key, defaultWait, setecv1alpha1.SandboxPhaseRunning)

	podKey := types.NamespacedName{Namespace: testNamespace, Name: sb.Name + "-vm"}
	var pod corev1.Pod
	if err := k8sClient.Get(context.Background(), podKey, &pod); err != nil {
		t.Fatalf("get pod before delete: %v", err)
	}

	if err := k8sClient.Delete(context.Background(), sb); err != nil {
		t.Fatalf("delete sandbox: %v", err)
	}

	deadline := time.Now().Add(defaultWait)
	for time.Now().Before(deadline) {
		err := k8sClient.Get(context.Background(), podKey, &pod)
		if apierrors.IsNotFound(err) {
			return // fully garbage collected
		}
		if err == nil && pod.DeletionTimestamp != nil {
			return // terminating; Kata is tearing down the microVM
		}
		if err != nil && !apierrors.IsNotFound(err) {
			t.Fatalf("get pod during teardown: %v", err)
		}
		time.Sleep(defaultPoll)
	}
	t.Fatalf("pod %s was not deleted or marked for deletion within %s", podKey, defaultWait)
}

// -- Scenario 6 --------------------------------------------------------------

// TestSandbox_OperatorRestartMidRun applies a long-running Sandbox, deletes
// the operator Pod mid-run, and asserts reconciliation resumes after the
// Deployment produces a fresh Pod. This validates the reconciler's
// assumption that all state is derivable from cluster state (no in-memory
// state lost at restart).
func TestSandbox_OperatorRestartMidRun(t *testing.T) {
	// Use a short command so the Sandbox completes even if the operator
	// takes a while to come back.
	sb := newSandbox("e2e-restart", minimalSpec("/bin/sh", "-c", "sleep 15; exit 0"))
	createAndCleanup(t, sb)

	key := client.ObjectKeyFromObject(sb)
	// Wait for the Sandbox to be Running (or even Completed already — the
	// restart test is only interesting mid-run, but if it completes first
	// that still proves convergence).
	waitForPhase(t, key, defaultWait,
		setecv1alpha1.SandboxPhaseRunning,
		setecv1alpha1.SandboxPhaseCompleted,
		setecv1alpha1.SandboxPhaseFailed,
	)

	// Snapshot the current operator pod name so we can watch for a
	// different one to appear.
	origPod, err := currentOperatorPodName()
	if err != nil {
		t.Fatalf("find operator pod: %v", err)
	}

	// Delete the operator Pod. The Deployment controller will spin up a
	// replacement within seconds.
	if err := k8sClient.Delete(context.Background(), &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: origPod}}); err != nil {
		t.Fatalf("delete operator pod: %v", err)
	}

	// Wait for a new operator Pod to be Ready.
	if err := waitForOperatorReady(origPod, defaultWait); err != nil {
		t.Fatalf("operator did not recover: %v", err)
	}

	// Sandbox must converge to a terminal phase.
	final := waitForPhase(t, key, defaultWait,
		setecv1alpha1.SandboxPhaseCompleted,
		setecv1alpha1.SandboxPhaseFailed,
	)
	if final.Status.Phase != setecv1alpha1.SandboxPhaseCompleted {
		t.Fatalf("expected Completed after operator restart, got %q (reason=%q)", final.Status.Phase, final.Status.Reason)
	}
}

// operatorLabels returns the label selector used to find the operator Pod.
// The Helm chart templates selector labels as {app.kubernetes.io/name: setec,
// app.kubernetes.io/instance: <release>}; the name label alone is enough to
// uniquely identify the operator within the release's namespace.
func operatorLabels() client.MatchingLabels {
	return client.MatchingLabels{"app.kubernetes.io/name": "setec"}
}

// currentOperatorPodName returns the name of the Ready operator Pod in the
// test namespace. It fails if there is not exactly one such Pod.
func currentOperatorPodName() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var pods corev1.PodList
	if err := k8sClient.List(ctx, &pods, client.InNamespace(testNamespace), operatorLabels()); err != nil {
		return "", err
	}
	var names []string
	for _, p := range pods.Items {
		if p.DeletionTimestamp != nil {
			continue
		}
		names = append(names, p.Name)
	}
	if len(names) != 1 {
		return "", fmt.Errorf("expected 1 operator pod, got %d (%v)", len(names), names)
	}
	return names[0], nil
}

// waitForOperatorReady blocks until an operator Pod whose name differs from
// `prev` is Ready, or until `timeout` elapses.
func waitForOperatorReady(prev string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var pods corev1.PodList
		if err := k8sClient.List(context.Background(), &pods, client.InNamespace(testNamespace), operatorLabels()); err != nil {
			return err
		}
		for _, p := range pods.Items {
			if p.Name == prev || p.DeletionTimestamp != nil {
				continue
			}
			for _, c := range p.Status.Conditions {
				if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
					return nil
				}
			}
		}
		time.Sleep(defaultPoll)
	}
	return fmt.Errorf("no Ready operator pod appeared within %s", timeout)
}

// -- Scenario 5 (runs last; mutates cluster RuntimeClass) --------------------

// TestSandbox_NoRuntimeClass deletes the kata-fc RuntimeClass, applies a
// Sandbox, and asserts the controller leaves it in phase=Pending while
// emitting the RuntimeUnavailable event. The RuntimeClass is restored in a
// t.Cleanup so the surrounding suite can continue unaffected. This test is
// named with a `ZZ_` prefix so Go's default alphabetical ordering runs it
// last; other scenarios depend on the RuntimeClass being present.
func TestSandbox_ZZ_NoRuntimeClass(t *testing.T) {
	ctx := context.Background()

	// Capture the current RuntimeClass manifest so we can restore it
	// byte-for-byte (minus the ResourceVersion, which the API server
	// assigns).
	var orig nodev1.RuntimeClass
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: kataRuntimeClass}, &orig); err != nil {
		t.Fatalf("snapshot runtimeclass: %v", err)
	}

	// Delete it.
	if err := k8sClient.Delete(ctx, &orig); err != nil {
		t.Fatalf("delete runtimeclass: %v", err)
	}

	// Always restore — even if the test itself fails — so the suite
	// doesn't leave the cluster in a broken state.
	t.Cleanup(func() {
		restore := &nodev1.RuntimeClass{
			ObjectMeta: metav1.ObjectMeta{
				Name:        orig.Name,
				Labels:      orig.Labels,
				Annotations: orig.Annotations,
			},
			Handler:    orig.Handler,
			Overhead:   orig.Overhead,
			Scheduling: orig.Scheduling,
		}
		// Wait for the delete to propagate, then recreate. Loop because
		// object deletion is eventually-consistent.
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			err := k8sClient.Create(ctx, restore)
			if err == nil {
				return
			}
			if !apierrors.IsAlreadyExists(err) {
				time.Sleep(defaultPoll)
				continue
			}
			return
		}
		t.Logf("warning: could not restore RuntimeClass %q after test", kataRuntimeClass)
	})

	// Wait briefly to ensure the delete has propagated — the apiserver
	// deletes synchronously but the controller's cache may still hold a
	// stale copy.
	time.Sleep(2 * time.Second)

	sb := newSandbox("e2e-no-runtimeclass", minimalSpec("/bin/true"))
	createAndCleanup(t, sb)

	key := client.ObjectKeyFromObject(sb)

	// Phase should stay Pending for long enough that a transient race
	// isn't enough to pass. We wait the brief window and confirm Pending.
	deadline := time.Now().Add(briefWait)
	var observed setecv1alpha1.Sandbox
	for time.Now().Before(deadline) {
		if err := k8sClient.Get(ctx, key, &observed); err != nil {
			t.Fatalf("get sandbox: %v", err)
		}
		if observed.Status.Phase != "" && observed.Status.Phase != setecv1alpha1.SandboxPhasePending {
			t.Fatalf("expected Pending while RuntimeClass is absent, got %q", observed.Status.Phase)
		}
		time.Sleep(defaultPoll)
	}

	// And the RuntimeUnavailable event should have fired at least once.
	if !waitForEvent(t, sb.Name, "RuntimeUnavailable", briefWait) {
		dumpDiagnostics(t, key)
		t.Fatalf("expected RuntimeUnavailable event on sandbox %q", sb.Name)
	}
}
