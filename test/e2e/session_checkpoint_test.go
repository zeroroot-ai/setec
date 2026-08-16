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

// Session-checkpoint e2e (setec#194, ADR-0006 L2 / ADR-0007): the
// suspend-idle → resume loop and drain → resume-on-another-node, with
// process state carried across by memory checkpoints on the
// S3-compatible store (MinIO in the dev env).
//
// Prerequisites beyond the usual e2e substrate:
//   - the node-agent DaemonSet runs with the S3 checkpoint backend
//     configured (snapshots.s3.enabled with a reachable MinIO/S3
//     bucket) — gated by SETEC_E2E_S3=1;
//   - the drain scenario additionally needs >= 2 sandbox-capable
//     nodes and is skipped otherwise.
//
// The in-guest probe prints monotonically increasing TICK-<n> lines.
// A resume that preserved process state CONTINUES the sequence; a
// restart from the workspace would begin again at TICK-1, so the
// assertion "a tick higher than the pre-suspend maximum appears, and
// TICK-1 appears exactly once in the final logs" distinguishes the
// two.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
)

// ckptTickerCommand emits TICK-<n> every 2 seconds from an in-guest
// process. Sequence continuity across a suspend/resume proves the
// process state survived (memory checkpoint), not just the disk.
var ckptTickerCommand = []string{"/bin/sh", "-c",
	"i=0; while true; do i=$((i+1)); echo TICK-$i; sleep 2; done"}

// loudSkip skips the test with a banner that survives `go test`'s
// terse SKIP line and a CI log scroll. A silently skipped verification
// test is indistinguishable from a passing one, which is exactly how
// the session path reached staging at zero-percent verified; every
// skip in this file therefore says what is missing and what it would
// take to satisfy it.
func loudSkip(t *testing.T, reason, remedy string) {
	t.Helper()
	fmt.Fprintf(os.Stderr, `
================================================================================
SKIPPED: %s
  reason: %s
  remedy: %s
  NOTE:   this scenario was NOT verified. A green run that contains this
          banner has not exercised the session-checkpoint path.
================================================================================
`, t.Name(), reason, remedy)
	t.Skipf("%s (%s)", reason, remedy)
}

// requireS3Checkpoints skips unless the harness declares the S3
// checkpoint backend configured on the node-agents.
func requireS3Checkpoints(t *testing.T) {
	t.Helper()
	if os.Getenv("SETEC_E2E_S3") == "" {
		loudSkip(t,
			"SETEC_E2E_S3 is not set, so the suite installed the release without the node-agent S3 checkpoint backend",
			"run with SETEC_E2E_S3=1 and SETEC_E2E_S3_BUCKET=<bucket> (staging: set the repository variable STAGING_SESSION_S3_READY=1 so the e2e workflow exports them)")
	}
}

// drainCapacityEnv opts the drain scenario in. The drain scenario is
// the ONLY scenario in the suite that needs a second sandbox-capable
// metal node, and on staging the setec-metal Karpenter NodePool is
// deliberately capped at cpu:48 — exactly one m5zn.metal — because the
// owner's standing instruction is "cheapest possible, no warm nodes".
// A second m5zn.metal costs roughly $4/hour on-demand in us-east-1, so
// the ceiling is raised by hand for a run and lowered again, and this
// variable is how the operator declares they did it.
const drainCapacityEnv = "SETEC_E2E_SESSION_DRAIN"

// drainRemedy is the exact procedure the skip banner points at.
const drainRemedy = "raise the setec-metal NodePool ceiling to cpu:96 via a gitops PR " +
	"(setec.karpenter.nodePools.metal.limits.cpu in the umbrella values), wait for the " +
	"second m5zn.metal to join (~$4/hr on-demand, us-east-1), then re-run with " +
	drainCapacityEnv + "=1 — and revert the ceiling PR when the run finishes"

// requireTwoSandboxNodes enforces the drain scenario's two-node
// precondition as a DELIBERATE, OPT-IN condition rather than a silent
// capability probe.
//
// Without the opt-in it skips loudly. WITH the opt-in it FAILS when the
// capacity is not actually there: the operator has asserted they paid
// for a second node, so "quietly skipped anyway" would be the worst of
// both worlds — cost incurred, nothing verified.
func requireTwoSandboxNodes(t *testing.T, backend string) {
	t.Helper()
	optedIn := os.Getenv(drainCapacityEnv) != ""

	nodes := &corev1.NodeList{}
	if err := k8sClient.List(context.Background(), nodes); err != nil {
		t.Fatalf("list nodes: %v", err)
	}
	label := "setec.zeroroot.ai/runtime." + backend
	var capable []string
	for _, n := range nodes.Items {
		if n.Labels[label] != "true" {
			continue
		}
		// A cordoned node is not somewhere the replacement Pod can go.
		if n.Spec.Unschedulable {
			continue
		}
		capable = append(capable, n.Name)
	}

	if !optedIn {
		loudSkip(t,
			fmt.Sprintf("%s is not set; the drain scenario needs a SECOND %s-capable node and staging runs exactly one by design (found %d: %v)",
				drainCapacityEnv, backend, len(capable), capable),
			drainRemedy)
		return
	}
	if len(capable) < 2 {
		t.Fatalf(`%s=1 asserts a second %s-capable node was provisioned for this run, but only %d schedulable node(s) carry %s=true: %v.
Either the NodePool ceiling was never raised, the second m5zn.metal has not joined yet, or the runtime probe is mislabelling the nodes (setec#281 class — check the label before blaming this test).
Failing rather than skipping: the opt-in means capacity was paid for, so a silent skip would verify nothing at full cost.`,
			drainCapacityEnv, backend, len(capable), label, capable)
	}
	t.Logf("drain scenario opted in via %s=1; %d schedulable %s-capable nodes: %v",
		drainCapacityEnv, len(capable), backend, capable)
}

// checkpointBackend is the runtime backend the checkpoint scenarios
// require. Memory checkpointing is a Firecracker snapshot operation
// (the node-agent drives the Firecracker API socket directly), so
// kata-qemu cannot serve these scenarios — the value is a named
// constant so the node-capability probe and the SandboxClass can never
// drift apart, which is how a "have 2 nodes" probe can silently look
// at the wrong label.
const checkpointBackend = "kata-fc"

// checkpointClassName names the SandboxClass fixture these scenarios
// install (and remove) around themselves. SandboxClass is
// cluster-scoped, so the name is uniquified by the suite's stamped
// namespace: a fixed name makes two concurrent e2e runs on the same
// cluster fight over one object and delete each other's fixture.
func checkpointClassName() string { return "e2e-session-checkpoint-" + testNamespace }

// installCheckpointClass creates a SandboxClass with sessionCheckpoint
// enabled and the given idle timeout, cleaning it up with the test.
func installCheckpointClass(t *testing.T, idle time.Duration) {
	t.Helper()
	cls := &setecv1alpha1.SandboxClass{
		ObjectMeta: metav1.ObjectMeta{Name: checkpointClassName()},
		Spec: setecv1alpha1.SandboxClassSpec{
			Runtime: &setecv1alpha1.SandboxClassRuntime{Backend: checkpointBackend},
			// Without this the checkpoint Sandbox never leaves Pending on
			// staging: the setec-metal NodePool taints its nodes
			// setec.zeroroot.ai/sandbox-host=true:NoSchedule and SandboxSpec
			// has no scheduling fields, so the class is the only place it can
			// be declared. Observed on run 31918112679; see
			// sandboxHostTolerations. This scenario is the one a human pays a
			// Terraform apply and a metal node to reach, so it must not be the
			// run that discovers the taint.
			Tolerations: sandboxHostTolerations(),
			SessionCheckpoint: &setecv1alpha1.SessionCheckpointSpec{
				Backend: "s3",
			},
		},
	}
	if idle > 0 {
		cls.Spec.SessionIdleTimeout = &metav1.Duration{Duration: idle}
	}
	if err := k8sClient.Create(context.Background(), cls); err != nil {
		t.Fatalf("create checkpoint SandboxClass: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), cls)
	})
}

// checkpointSessionSpec builds the session spec pinned to the
// checkpoint class.
func checkpointSessionSpec() setecv1alpha1.SandboxSpec {
	spec := minimalSpec(ckptTickerCommand...)
	spec.SandboxClassName = checkpointClassName()
	size := resource.MustParse("1Gi")
	spec.Lifecycle = &setecv1alpha1.Lifecycle{
		Mode:      setecv1alpha1.LifecycleModeSession,
		Workspace: &setecv1alpha1.WorkspaceSpec{Size: &size},
	}
	return spec
}

var tickRe = regexp.MustCompile(`TICK-(\d+)`)

// maxTick parses the highest TICK-<n> from the Pod logs.
func maxTick(t *testing.T, podName string) int {
	t.Helper()
	maxN := 0
	for _, m := range tickRe.FindAllStringSubmatch(podLogs(t, podName), -1) {
		if n, err := strconv.Atoi(m[1]); err == nil && n > maxN {
			maxN = n
		}
	}
	return maxN
}

// stampActivityNow simulates a reattach: the frontend's Attach stamps
// the last-activity annotation, which is the wake signal for an
// idle-suspended session.
func stampActivityNow(t *testing.T, key client.ObjectKey) {
	t.Helper()
	patch := fmt.Sprintf(
		`{"metadata":{"annotations":{"%s":"%s"}}}`,
		setecv1alpha1.AnnotationLastActivity, time.Now().UTC().Format(time.RFC3339))
	sb := &setecv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name}}
	if err := k8sClient.Patch(context.Background(), sb, client.RawPatch(types.MergePatchType, []byte(patch))); err != nil {
		t.Fatalf("stamp activity: %v", err)
	}
}

// TestSessionCheckpoint_SuspendIdleResume: an unattended session hits
// the class idle deadline, checkpoints, and releases its microVM;
// reattach activity resumes it with the in-guest process continuing
// (acceptance: suspend-idle → resume, setec#194).
func TestSessionCheckpoint_SuspendIdleResume(t *testing.T) {
	requireS3Checkpoints(t)
	installCheckpointClass(t, 45*time.Second)

	sb := newSandbox("e2e-ckpt-idle", checkpointSessionSpec())
	createAndCleanup(t, sb)
	key := client.ObjectKeyFromObject(sb)
	podKey := types.NamespacedName{Namespace: testNamespace, Name: sb.Name + "-vm"}

	waitForPhase(t, key, defaultWait, setecv1alpha1.SandboxPhaseRunning)
	if !waitForLogMarker(t, podKey.Name, "TICK-3", briefWait) {
		dumpDiagnostics(t, key)
		t.Fatal("ticker never started")
	}
	preSuspend := maxTick(t, podKey.Name)

	// The idle deadline suspends (not evicts) the session.
	waitForPhase(t, key, defaultWait, setecv1alpha1.SandboxPhaseSuspended)
	got, err := getSandboxE2E(key)
	if err != nil {
		t.Fatalf("get suspended sandbox: %v", err)
	}
	if got.Status.Reason != "SuspendedIdle" {
		t.Fatalf("suspend reason = %q, want SuspendedIdle", got.Status.Reason)
	}
	if got.Status.Checkpoint == nil || got.Status.Checkpoint.Ref == "" {
		t.Fatal("suspended session has no recorded checkpoint")
	}

	// Reattach (activity stamp) wakes it; the restored process must
	// CONTINUE the tick sequence, not restart it.
	stampActivityNow(t, key)
	waitForPhase(t, key, defaultWait, setecv1alpha1.SandboxPhaseRunning)
	deadline := time.Now().Add(defaultWait)
	for {
		if maxTick(t, podKey.Name) > preSuspend {
			break
		}
		if time.Now().After(deadline) {
			dumpDiagnostics(t, key)
			t.Fatalf("resumed session never ticked past pre-suspend max %d", preSuspend)
		}
		time.Sleep(defaultPoll)
	}
	if n := strings.Count(podLogs(t, podKey.Name), "TICK-1\n"); n > 1 {
		t.Fatalf("TICK-1 appeared %d times; the process restarted instead of resuming from the checkpoint", n)
	}
	got, err = getSandboxE2E(key)
	if err != nil {
		t.Fatalf("get resumed sandbox: %v", err)
	}
	if got.Status.Checkpoint == nil ||
		got.Status.Checkpoint.LastRecovery != setecv1alpha1.SessionRecoveryResumedFromCheckpoint {
		dumpDiagnostics(t, key)
		t.Fatalf("lastRecovery = %+v, want ResumedFromCheckpoint", got.Status.Checkpoint)
	}
}

// TestSessionCheckpoint_DrainResumeOnOtherNode: cordoning the session
// VM's node checkpoints the session and resumes it on a different
// node with process state intact and the workspace PVC re-attached
// (acceptance: drain → resume-on-other-node, setec#194).
func TestSessionCheckpoint_DrainResumeOnOtherNode(t *testing.T) {
	requireS3Checkpoints(t)
	// Checked BEFORE anything is created: provisioning a session VM and
	// then discovering there is nowhere to drain it to wastes a metal
	// node's worth of scheduling for nothing.
	requireTwoSandboxNodes(t, checkpointBackend)
	installCheckpointClass(t, 0)

	sb := newSandbox("e2e-ckpt-drain", checkpointSessionSpec())
	createAndCleanup(t, sb)
	key := client.ObjectKeyFromObject(sb)
	podKey := types.NamespacedName{Namespace: testNamespace, Name: sb.Name + "-vm"}

	waitForPhase(t, key, defaultWait, setecv1alpha1.SandboxPhaseRunning)
	if !waitForLogMarker(t, podKey.Name, "TICK-3", briefWait) {
		dumpDiagnostics(t, key)
		t.Fatal("ticker never started")
	}

	var firstPod corev1.Pod
	if err := k8sClient.Get(context.Background(), podKey, &firstPod); err != nil {
		t.Fatalf("get session pod: %v", err)
	}
	firstNode := firstPod.Spec.NodeName
	preSuspend := maxTick(t, podKey.Name)

	// Drain: cordon the node (the controller's Node watch triggers
	// checkpoint-on-drain), uncordoning on the way out.
	if out, err := exec.Command("kubectl", "cordon", firstNode).CombinedOutput(); err != nil {
		t.Fatalf("kubectl cordon %s: %v (%s)", firstNode, err, out)
	}
	t.Cleanup(func() {
		_ = exec.Command("kubectl", "uncordon", firstNode).Run()
	})

	// The replacement Pod must land on a DIFFERENT node and continue
	// the tick sequence.
	deadline := time.Now().Add(defaultWait)
	for {
		var pod corev1.Pod
		err := k8sClient.Get(context.Background(), podKey, &pod)
		if err == nil && pod.UID != firstPod.UID && pod.Spec.NodeName != "" && pod.DeletionTimestamp == nil {
			if pod.Spec.NodeName == firstNode {
				t.Fatalf("replacement Pod landed on the cordoned node %q", firstNode)
			}
			break
		}
		if time.Now().After(deadline) {
			dumpDiagnostics(t, key)
			t.Fatalf("no replacement Pod on another node after drain (err=%v)", err)
		}
		time.Sleep(defaultPoll)
	}
	waitForPhase(t, key, defaultWait, setecv1alpha1.SandboxPhaseRunning)

	deadline = time.Now().Add(defaultWait)
	for {
		if maxTick(t, podKey.Name) > preSuspend {
			break
		}
		if time.Now().After(deadline) {
			dumpDiagnostics(t, key)
			t.Fatalf("drained session never ticked past pre-drain max %d on the new node", preSuspend)
		}
		time.Sleep(defaultPoll)
	}
	got, err := getSandboxE2E(key)
	if err != nil {
		t.Fatalf("get resumed sandbox: %v", err)
	}
	if got.Status.Checkpoint == nil ||
		got.Status.Checkpoint.LastRecovery != setecv1alpha1.SessionRecoveryResumedFromCheckpoint {
		dumpDiagnostics(t, key)
		t.Fatalf("lastRecovery = %+v, want ResumedFromCheckpoint after drain", got.Status.Checkpoint)
	}
}

// getSandboxE2E fetches the Sandbox by key.
func getSandboxE2E(key client.ObjectKey) (*setecv1alpha1.Sandbox, error) {
	sb := &setecv1alpha1.Sandbox{}
	if err := k8sClient.Get(context.Background(), key, sb); err != nil {
		return nil, err
	}
	return sb, nil
}
