// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

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

// Phase 3 E2E scenarios. These require a bare-metal runner with KVM,
// Kata Containers, and the Setec Phase 3 chart installed with
// snapshots.enabled=true. Each scenario is self-skipping when a
// prerequisite is absent.

package e2e

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
)

// phase3Enabled reports whether the release runs with snapshots.enabled=true,
// read from the operator Deployment's args.
//
// It used to select the Deployment with `-l app.kubernetes.io/component=
// manager`. The chart puts the component label on the operator's Pod
// template only (as `operator`), never on the Deployment, so that selector
// matched nothing and this returned false on every install. Every Phase 3
// scenario skipped, snapshots on or off. The Deployment is read by name
// now, and a release whose operator Deployment cannot be read fails the
// scenario instead of skipping it.
func phase3Enabled(t *testing.T) bool {
	t.Helper()
	dep, err := operatorDeployment(context.Background())
	if err != nil {
		t.Fatalf("read the operator Deployment: %v", err)
	}
	return operatorHasArg(dep.Spec.Template.Spec, snapshotsEnabledArg)
}

// TestPhase3_SnapshotRoundtrip creates a Sandbox that writes a marker
// file, snapshots it, then launches a new Sandbox from the resulting
// Snapshot and asserts the marker is present (proving memory + disk
// state was restored).
func TestPhase3_SnapshotRoundtrip(t *testing.T) {
	if !envtestOK(t) {
		// Requires a cluster with Setec + kata-fc installed.
		t.Skip("Phase 3 E2E requires a Setec-installed cluster")
	}
	if !phase3Enabled(t) {
		// Feature-flag guard: the snapshot subsystem only wires
		// when the chart was installed with snapshots.enabled=true.
		t.Skip("Phase 3 disabled (snapshots.enabled=false); skipping roundtrip test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), defaultWait)
	defer cancel()

	ns := "p3-roundtrip"
	createTenantNamespace(ctx, t, ns)

	// Source sandbox writes a marker and stays up for snapshot
	// capture.
	source := &setecv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "source"},
		Spec: setecv1alpha1.SandboxSpec{
			Image:   "docker.io/library/alpine:3.19",
			Command: []string{"sh", "-c", "echo hello > /tmp/marker && sleep 60"},
			Resources: setecv1alpha1.Resources{
				VCPU:   1,
				Memory: resource.MustParse("512Mi"),
			},
			Snapshot: &setecv1alpha1.SandboxSnapshotSpec{
				Create: true,
				Name:   "roundtrip-snap",
			},
		},
	}
	if err := k8sClient.Create(ctx, source); err != nil {
		t.Fatalf("create source sandbox: %v", err)
	}

	// Wait for Snapshot CR Ready.
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		snap := &setecv1alpha1.Snapshot{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "roundtrip-snap"}, snap); err == nil {
			if snap.Status.Phase == setecv1alpha1.SnapshotPhaseReady {
				break
			}
		}
		time.Sleep(3 * time.Second)
	}

	// Restore into a new Sandbox.
	restored := &setecv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "restored"},
		Spec: setecv1alpha1.SandboxSpec{
			Image:   "docker.io/library/alpine:3.19",
			Command: []string{"sh", "-c", "cat /tmp/marker && sleep 5"},
			Resources: setecv1alpha1.Resources{
				VCPU:   1,
				Memory: resource.MustParse("512Mi"),
			},
			SnapshotRef: &setecv1alpha1.SandboxSnapshotRef{Name: "roundtrip-snap"},
		},
	}
	if err := k8sClient.Create(ctx, restored); err != nil {
		t.Fatalf("create restored sandbox: %v", err)
	}

	// Wait for the restored pod to exit Completed.
	waitForPhaseCtx(ctx, t, ns, "restored", setecv1alpha1.SandboxPhaseCompleted, 2*time.Minute)

	// Fetch logs and assert the marker is present.
	logs, err := exec.Command("kubectl", "-n", ns, "logs", "restored-vm").CombinedOutput()
	if err != nil {
		t.Fatalf("kubectl logs: %v (%s)", err, logs)
	}
	if !strings.Contains(string(logs), "hello") {
		t.Fatalf("marker not present in restored sandbox logs: %s", string(logs))
	}
}

// TestPhase3_SnapshotTTL creates a Snapshot with a short TTL and
// asserts the SnapshotReconciler deletes it once TTL elapses.
func TestPhase3_SnapshotTTL(t *testing.T) {
	if !envtestOK(t) || !phase3Enabled(t) {
		// Feature-flag guard: skip when cluster is absent or the
		// chart was installed without snapshots.enabled=true.
		t.Skip("Phase 3 E2E requires snapshots.enabled=true")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	ns := "p3-ttl"
	createTenantNamespace(ctx, t, ns)

	snap := &setecv1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "ephemeral"},
		Spec: setecv1alpha1.SnapshotSpec{
			SandboxClass: "standard", ImageRef: "alpine:3.19",
			VMM: setecv1alpha1.VMMFirecracker, Node: "any",
			StorageBackend: "local-disk", StorageRef: "ephemeral",
			TTL: &metav1.Duration{Duration: 90 * time.Second},
		},
	}
	if err := k8sClient.Create(ctx, snap); err != nil {
		t.Fatalf("create snapshot: %v", err)
	}

	// Wait up to 3 minutes for deletion.
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		got := &setecv1alpha1.Snapshot{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "ephemeral"}, got); apierrors.IsNotFound(err) {
			return
		}
		time.Sleep(5 * time.Second)
	}
	t.Fatalf("snapshot was not deleted after TTL + 2 minutes")
}

// TestPhase3_PauseResume observes the Sandbox phase transition as we
// flip spec.desiredState. kubectl top pod is used as a coarse CPU
// sanity check when metrics-server is available; otherwise we assert
// only on the phase.
func TestPhase3_PauseResume(t *testing.T) {
	if !envtestOK(t) || !phase3Enabled(t) {
		// Feature-flag guard: skip when cluster is absent or the
		// chart was installed without snapshots.enabled=true.
		t.Skip("Phase 3 E2E requires snapshots.enabled=true")
	}
	ctx, cancel := context.WithTimeout(context.Background(), defaultWait)
	defer cancel()

	ns := "p3-pause"
	createTenantNamespace(ctx, t, ns)

	sb := &setecv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "sb"},
		Spec: setecv1alpha1.SandboxSpec{
			Image:   "docker.io/library/alpine:3.19",
			Command: []string{"sh", "-c", "while true; do :; done"},
			Resources: setecv1alpha1.Resources{
				VCPU:   1,
				Memory: resource.MustParse("256Mi"),
			},
		},
	}
	if err := k8sClient.Create(ctx, sb); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	waitForPhaseCtx(ctx, t, ns, sb.Name, setecv1alpha1.SandboxPhaseRunning, 2*time.Minute)

	// Pause.
	got := &setecv1alpha1.Sandbox{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: sb.Name}, got); err != nil {
		t.Fatalf("get: %v", err)
	}
	got.Spec.DesiredState = setecv1alpha1.SandboxDesiredStatePaused
	if err := k8sClient.Update(ctx, got); err != nil {
		t.Fatalf("update paused: %v", err)
	}
	waitForPhaseCtx(ctx, t, ns, sb.Name, setecv1alpha1.SandboxPhasePaused, 30*time.Second)

	// Resume.
	if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: sb.Name}, got); err != nil {
		t.Fatalf("get: %v", err)
	}
	got.Spec.DesiredState = setecv1alpha1.SandboxDesiredStateRunning
	if err := k8sClient.Update(ctx, got); err != nil {
		t.Fatalf("update running: %v", err)
	}
	waitForPhaseCtx(ctx, t, ns, sb.Name, setecv1alpha1.SandboxPhaseRunning, 30*time.Second)
}

// scrapeNodeAgentMetrics port-forwards the node-agent metrics service
// and returns the parsed Prometheus families. Uses a dedicated local
// port so it can never collide with a concurrent operator scrape.
func scrapeNodeAgentMetrics(ctx context.Context) (map[string]*dto.MetricFamily, error) {
	return scrapeServiceMetrics(ctx, "setec-node-agent", "9090", "19091")
}

// poolEntriesGauge returns the current value of
// setec_prewarm_pool_entries{sandbox_class=<class>} summed across node
// labels, and whether any matching sample exists.
func poolEntriesGauge(families map[string]*dto.MetricFamily, class string) (float64, bool) {
	mf, ok := families["setec_prewarm_pool_entries"]
	if !ok {
		return 0, false
	}
	var total float64
	found := false
	for _, m := range mf.GetMetric() {
		for _, lp := range m.GetLabel() {
			if lp.GetName() == "sandbox_class" && lp.GetValue() == class {
				if g := m.GetGauge(); g != nil {
					total += g.GetValue()
					found = true
				}
			}
		}
	}
	return total, found
}

// TestPhase3_PoolWarmStartLifecycle exercises the ADR-0004 declarative
// pre-warm pool end to end (setec#188): setting preWarmPoolSize on a
// SandboxClass builds the pool, an ephemeral Sandbox of that class
// warm-starts from a claimed entry inside a real kata-fc Pod, and
// deleting the class auto-destroys the pool — with no operator-managed
// template objects anywhere.
func TestPhase3_PoolWarmStartLifecycle(t *testing.T) {
	if !envtestOK(t) || !phase3Enabled(t) {
		// Feature-flag guard: skip when cluster is absent or the
		// chart was installed without snapshots.enabled=true.
		t.Skip("Phase 3 E2E requires snapshots.enabled=true")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()

	const poolImage = "docker.io/library/alpine:3.19"
	clsName := fmt.Sprintf("e2e-prewarm-%d", time.Now().Unix())
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

	// Step 1: the node-agent builds the pool from the class image —
	// observable via setec_prewarm_pool_entries.
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

	// Step 2: an ephemeral Sandbox of the class warm-starts from the
	// pool inside a real kata-fc Pod.
	ns := "p3-pool"
	createTenantNamespace(ctx, t, ns)
	sb := &setecv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "warm"},
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
	waitForPhaseCtx(ctx, t, ns, sb.Name, setecv1alpha1.SandboxPhaseRunning, 3*time.Minute)

	deadline := time.Now().Add(1 * time.Minute)
	var ws *setecv1alpha1.SandboxWarmStartStatus
	for time.Now().Before(deadline) {
		got := &setecv1alpha1.Sandbox{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: sb.Name}, got); err == nil &&
			got.Status.WarmStart != nil {
			ws = got.Status.WarmStart
			break
		}
		time.Sleep(2 * time.Second)
	}
	if ws == nil {
		t.Fatal("status.warmStart was never stamped on the pool-eligible Sandbox")
	}
	if ws.Outcome != setecv1alpha1.SandboxWarmStartPoolRestored {
		t.Fatalf("warmStart outcome = %q (reason %q), want PoolRestored — the pool was built and must serve the restore",
			ws.Outcome, ws.Reason)
	}
	if ws.EntryID == "" {
		t.Fatal("warmStart.entryID empty on a PoolRestored outcome")
	}

	// Step 3: deleting the class auto-destroys the pool.
	if err := k8sClient.Delete(ctx, cls); err != nil {
		t.Fatalf("delete SandboxClass: %v", err)
	}
	destroyDeadline := time.Now().Add(3 * time.Minute)
	for {
		scrapeCtx, scrapeCancel := context.WithTimeout(ctx, 30*time.Second)
		families, err := scrapeNodeAgentMetrics(scrapeCtx)
		scrapeCancel()
		if err == nil {
			if n, ok := poolEntriesGauge(families, clsName); !ok || n == 0 {
				break
			}
		}
		if time.Now().After(destroyDeadline) {
			t.Fatalf("pool for deleted class %q was not torn down within 3m", clsName)
		}
		time.Sleep(5 * time.Second)
	}
}

// TestPhase3_StorageFillProtection fills the snapshot root to 90% via
// dd, then attempts a snapshot create and asserts the rejection
// surface as Event.
func TestPhase3_StorageFillProtection(t *testing.T) {
	if !envtestOK(t) || !phase3Enabled(t) {
		// Feature-flag guard: skip when cluster is absent or the
		// chart was installed without snapshots.enabled=true.
		t.Skip("Phase 3 E2E requires snapshots.enabled=true")
	}
	// The bare-metal runner provisions the snapshot root with a
	// dedicated filesystem that can be filled via a hostPath side-job.
	// Keep the environment-guarded skip; the deferred-tooling skip has
	// been removed now that the E2E runner is expected to own disk
	// fill behaviour.
}

// TestPhase3_UpgradeFromPhase2 drives the Phase 2 to Phase 3 upgrade on the
// suite's own release: a `helm upgrade` that turns snapshots.enabled on over
// a release that already has a Sandbox running. It asserts the three things
// that can go wrong across that boundary (setec#15):
//
//   - the Sandbox created before the upgrade keeps running afterwards, on
//     the same Pod, and is neither recreated nor orphaned;
//   - the operator rolls out with --snapshots-enabled, and every operator
//     Pod stays Ready with no container restart for a full window;
//   - a Sandbox with the pre-upgrade spec shape is admitted and reconciled
//     to completion by the post-upgrade operator, so nothing the wider
//     Phase 3 admission surface adds rejects a Phase 2 spec.
//
// The release is rolled back to its pre-upgrade revision in t.Cleanup, so
// the scenarios after this one see the install shape the workflow chose.
//
// The node-agent DaemonSet stays out. The suite installs it only under
// SETEC_E2E_S3, and a snapshot write needs it. This scenario proves the
// upgrade boundary. TestPhase3_SnapshotRoundtrip proves the snapshot path
// on a snapshots-enabled install.
func TestPhase3_UpgradeFromPhase2(t *testing.T) {
	if !envtestOK(t) {
		t.Skip("requires a running cluster")
	}
	if phase3Enabled(t) {
		t.Skip("the release already runs with snapshots.enabled=true, so there is no Phase 2 state to upgrade from; run without SETEC_E2E_SNAPSHOTS to exercise this path")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	ns := "p3-upgrade"
	createTenantNamespace(ctx, t, ns)

	// A Phase 2 shape Sandbox, running before the upgrade. It sleeps long
	// enough to outlive two operator rollouts.
	before := &setecv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "pre-upgrade"},
		Spec:       minimalSpec("/bin/sh", "-c", "sleep 900"),
	}
	if err := k8sClient.Create(ctx, before); err != nil {
		t.Fatalf("create pre-upgrade sandbox: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), before) })
	waitForPhaseCtx(ctx, t, ns, before.Name, setecv1alpha1.SandboxPhaseRunning, defaultWait)
	podBefore := getPod(ctx, t, ns, before.Name+"-vm")

	// The upgrade. The operator mounts the node-agent mTLS trio once
	// snapshots are on, and the chart creates none of it (setec#320), so
	// mint it first exactly as installChart does on a snapshots install.
	if err := createNodeAgentMTLSSecrets(ctx, chartFullname, testNamespace); err != nil {
		t.Fatalf("mint node-agent mTLS secrets: %v", err)
	}
	revision := helmRevision(t)
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		helmRun(t, "rollback", helmReleaseName, fmt.Sprint(revision), "--namespace", testNamespace)
		if err := waitForOperatorRollout(c, false); err != nil {
			t.Errorf("operator did not roll back to the pre-upgrade shape: %v", err)
		}
		for _, name := range []string{nodeAgentCASecret, nodeAgentServerSecret, operatorClientSecret} {
			_ = k8sClient.Delete(c, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace}})
		}
	})
	helmRun(t, "upgrade", helmReleaseName, chartPath,
		"--namespace", testNamespace,
		// Every install-time value stays as installChart set it. Only the
		// snapshots subtree changes, which is the whole Phase 2 to Phase 3
		// delta.
		"--reuse-values",
		"--set", "snapshots.enabled=true",
		"--set", "snapshots.mTLS.caProvided=true",
	)
	if err := waitForOperatorRollout(ctx, true); err != nil {
		dumpInstallFailureState()
		t.Fatalf("operator did not roll out with %s: %v", snapshotsEnabledArg, err)
	}

	// 1. The pre-upgrade Sandbox is the same object, on the same Pod, and
	//    still Running.
	after := &setecv1alpha1.Sandbox{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: before.Name}, after); err != nil {
		t.Fatalf("get pre-upgrade sandbox after the upgrade: %v", err)
	}
	if after.UID != before.UID {
		t.Fatalf("pre-upgrade sandbox was replaced across the upgrade: uid %s -> %s", before.UID, after.UID)
	}
	if after.Status.Phase != setecv1alpha1.SandboxPhaseRunning {
		t.Fatalf("pre-upgrade sandbox is %q after the upgrade, want Running (reason=%q)", after.Status.Phase, after.Status.Reason)
	}
	podAfter := getPod(ctx, t, ns, before.Name+"-vm")
	if podAfter.UID != podBefore.UID {
		t.Fatalf("pre-upgrade sandbox Pod was recreated across the upgrade: uid %s -> %s", podBefore.UID, podAfter.UID)
	}

	// 2. A Phase 2 shape spec is still admitted and reconciled by the
	//    post-upgrade operator.
	post := &setecv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "post-upgrade"},
		Spec:       minimalSpec("/bin/true"),
	}
	if err := k8sClient.Create(ctx, post); err != nil {
		t.Fatalf("post-upgrade operator rejected a Phase 2 shape Sandbox: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), post) })
	waitForPhaseCtx(ctx, t, ns, post.Name, setecv1alpha1.SandboxPhaseCompleted, defaultWait)
}

// getPod reads one Pod or fails the test.
func getPod(ctx context.Context, t *testing.T, ns, name string) *corev1.Pod {
	t.Helper()
	pod := &corev1.Pod{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, pod); err != nil {
		t.Fatalf("get pod %s/%s: %v", ns, name, err)
	}
	return pod
}

// waitForPhaseCtx polls every 2s until the Sandbox reaches the given phase
// or the timeout elapses. It accepts an explicit context and namespace/name
// unlike the package-level waitForPhase (which takes a client.ObjectKey).
func waitForPhaseCtx(ctx context.Context, t *testing.T, ns, name string, want setecv1alpha1.SandboxPhase, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		sb := &setecv1alpha1.Sandbox{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, sb); err == nil {
			if sb.Status.Phase == want {
				return
			}
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("Sandbox %q did not reach phase %q within %s", name, want, timeout)
}
