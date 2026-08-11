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

package controller

// Session-checkpoint scenarios (setec#194): suspend on explicit
// request, suspend-on-idle instead of eviction, resume-from-checkpoint
// on the fresh VM, checkpoint-on-drain via node cordon, degraded
// restart-from-workspace as a distinct condition, and KEK Secret
// lifecycle. The node-agent is the suite's scripted fake; envtest has
// no kubelet/scheduler so Pod status and node binding are driven by
// the tests.

import (
	"testing"
	"time"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
	"github.com/zeroroot-ai/setec/internal/podspec"
)

// withSessionCheckpoint enables the checkpoint policy on a class.
func withSessionCheckpoint(interval time.Duration) func(*setecv1alpha1.SandboxClass) {
	return func(c *setecv1alpha1.SandboxClass) {
		sc := &setecv1alpha1.SessionCheckpointSpec{Backend: "s3"}
		if interval > 0 {
			sc.Interval = &metav1.Duration{Duration: interval}
		}
		c.Spec.SessionCheckpoint = sc
	}
}

// getKEKSecret fetches the per-session KEK Secret.
func getKEKSecret(ns, sbName string) (*corev1.Secret, error) {
	s := &corev1.Secret{}
	err := testClient.Get(testCtx, types.NamespacedName{Namespace: ns, Name: sbName + sessionKEKSuffix}, s)
	return s, err
}

// patchDesiredState flips spec.desiredState.
func patchDesiredState(g Gomega, ns, name string, state setecv1alpha1.SandboxDesiredState) {
	g.Eventually(func() error {
		sb, err := getSandbox(testCtx, ns, name)
		if err != nil {
			return err
		}
		original := sb.DeepCopy()
		sb.Spec.DesiredState = state
		return testClient.Patch(testCtx, sb, client.MergeFrom(original))
	}, convergeTimeout, convergeInterval).Should(Succeed())
}

// finalizeTerminatingPod lets a deleted-but-lingering Pod finish
// dying: envtest has no kubelet, so a Running Pod under graceful
// deletion never finalizes on its own. A grace-period-zero delete
// completes it — standing in for the kubelet's teardown confirmation
// on a real node.
func finalizeTerminatingPod(g Gomega, ns, sbName string, oldUID types.UID) {
	g.Eventually(func() bool {
		pod, err := getPod(testCtx, ns, sbName+podspec.PodNameSuffix)
		if apierrors.IsNotFound(err) {
			return true
		}
		if err != nil {
			return false
		}
		if pod.UID != oldUID {
			// The controller already replaced it; the old one is gone.
			return true
		}
		if pod.DeletionTimestamp == nil {
			return false
		}
		_ = testClient.Delete(testCtx, pod, client.GracePeriodSeconds(0))
		return false
	}, convergeTimeout, convergeInterval).Should(BeTrue(), "terminating Pod should finalize")
}

// runSessionVM waits for the Sandbox's Pod, binds it to the suite
// node, and marks it Running.
func runSessionVM(g Gomega, t *testing.T, ns, sbName string) *corev1.Pod {
	pod := waitForPod(g, ns, sbName)
	bindPodToNode(t, pod, "kata-node-1")
	markPodRunning(g, ns, sbName)
	return pod
}

// TestSessionCheckpoint_SuspendAndResume drives the full L2 loop:
// desiredState=Suspended checkpoints the VM, releases the Pod, and
// parks the session; desiredState=Running brings up a fresh VM that
// restores from the checkpoint (consuming it) and reports
// ResumedFromCheckpoint.
func TestSessionCheckpoint_SuspendAndResume(t *testing.T) {
	g := NewWithT(t)
	ns := newNamespace(t, "sck-sr")

	cls := newSandboxClass("sck-sr-class", withSessionCheckpoint(0))
	g.Expect(testClient.Create(testCtx, cls)).To(Succeed())
	t.Cleanup(func() { _ = testClient.Delete(testCtx, cls) })

	sb := newSandboxWithClass(ns, "susp", cls.Name, asSession(""))
	g.Expect(testClient.Create(testCtx, sb)).To(Succeed())
	firstPod := runSessionVM(g, t, ns, sb.Name)

	// The per-session KEK Secret exists before any checkpoint could.
	g.Eventually(func() error {
		_, err := getKEKSecret(ns, sb.Name)
		return err
	}, convergeTimeout, convergeInterval).Should(Succeed(), "session KEK Secret should be created")

	// Suspend.
	patchDesiredState(g, ns, sb.Name, setecv1alpha1.SandboxDesiredStateSuspended)
	g.Eventually(func() string {
		return phaseAndReason(ns, sb.Name)
	}, convergeTimeout, convergeInterval).Should(Equal("Suspended/" + reasonUserSuspended))

	got, err := getSandbox(testCtx, ns, sb.Name)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(got.Status.Checkpoint).NotTo(BeNil())
	g.Expect(got.Status.Checkpoint.Ref).NotTo(BeEmpty(), "suspend must record the checkpoint ref")
	g.Expect(got.Status.Checkpoint.PendingRestore).To(BeTrue())
	g.Expect(got.Status.Checkpoint.Backend).To(Equal("s3"))

	// The microVM is released (envtest needs help finalizing it).
	g.Eventually(func() bool {
		p, err := getPod(testCtx, ns, sb.Name+podspec.PodNameSuffix)
		return apierrors.IsNotFound(err) || (err == nil && p.DeletionTimestamp != nil)
	}, convergeTimeout, convergeInterval).Should(BeTrue(), "suspend must release the VM Pod")
	finalizeTerminatingPod(g, ns, sb.Name, firstPod.UID)

	// The workspace PVC survives the suspend.
	pvc, err := getPVC(ns, podspec.WorkspacePVCName(sb.Name))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(pvc.DeletionTimestamp).To(BeNil())

	// Resume: a fresh Pod appears, and once its VM runs the restore
	// fires and consumes the checkpoint.
	patchDesiredState(g, ns, sb.Name, setecv1alpha1.SandboxDesiredStateRunning)
	g.Eventually(func() bool {
		p, err := getPod(testCtx, ns, sb.Name+podspec.PodNameSuffix)
		return err == nil && p.DeletionTimestamp == nil
	}, convergeTimeout, convergeInterval).Should(BeTrue(), "resume must recreate the VM Pod")
	runSessionVM(g, t, ns, sb.Name)

	g.Eventually(func() string {
		got, err := getSandbox(testCtx, ns, sb.Name)
		if err != nil || got.Status.Checkpoint == nil {
			return ""
		}
		return string(got.Status.Checkpoint.LastRecovery)
	}, convergeTimeout, convergeInterval).Should(Equal(string(setecv1alpha1.SessionRecoveryResumedFromCheckpoint)))

	got, err = getSandbox(testCtx, ns, sb.Name)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(got.Status.Checkpoint.Ref).To(BeEmpty(), "a restored checkpoint is consumed (single-restore invariant)")
	g.Expect(got.Status.Checkpoint.PendingRestore).To(BeFalse())
	g.Expect(got.Status.Phase).To(Equal(setecv1alpha1.SandboxPhaseRunning))
}

// TestSessionCheckpoint_IdleSuspendsInsteadOfEvicting asserts the
// suspend machinery consumes setec#193's idle signal: with
// sessionCheckpoint enabled, the idle deadline suspends (phase
// Suspended, reason SuspendedIdle) instead of failing with
// IdleTimeout, and fresh activity — a reattach — wakes the session
// back up.
func TestSessionCheckpoint_IdleSuspendsInsteadOfEvicting(t *testing.T) {
	g := NewWithT(t)
	ns := newNamespace(t, "sck-idle")

	cls := newSandboxClass("sck-idle-class",
		withSessionIdleTimeout(sessionIdleTestTimeout),
		withSessionCheckpoint(0))
	g.Expect(testClient.Create(testCtx, cls)).To(Succeed())
	t.Cleanup(func() { _ = testClient.Delete(testCtx, cls) })

	sb := newSandboxWithClass(ns, "idler", cls.Name, asSession(""))
	g.Expect(testClient.Create(testCtx, sb)).To(Succeed())
	idlePod := runSessionVM(g, t, ns, sb.Name)

	g.Eventually(func() string {
		return phaseAndReason(ns, sb.Name)
	}, 4*sessionIdleTestTimeout, convergeInterval).Should(Equal("Suspended/"+reasonSuspendedIdle),
		"a checkpoint-enabled idle session suspends instead of failing IdleTimeout")
	finalizeTerminatingPod(g, ns, sb.Name, idlePod.UID)

	// Fresh activity (what Attach stamps) resumes the session.
	stampActivity(g, ns, sb.Name, time.Now())
	g.Eventually(func() bool {
		p, err := getPod(testCtx, ns, sb.Name+podspec.PodNameSuffix)
		return err == nil && p.DeletionTimestamp == nil
	}, convergeTimeout, convergeInterval).Should(BeTrue(), "reattach activity must wake the suspended session")
}

// TestSessionCheckpoint_CheckpointOnDrain cordons the node under a
// running session: the controller checkpoints, releases the Pod
// (reason CheckpointOnDrain), and immediately schedules the
// replacement — which restores from the checkpoint once its VM runs,
// i.e. the session resumes on whatever node the scheduler picks with
// process state intact.
func TestSessionCheckpoint_CheckpointOnDrain(t *testing.T) {
	g := NewWithT(t)
	ns := newNamespace(t, "sck-drain")

	cls := newSandboxClass("sck-drain-class", withSessionCheckpoint(0))
	g.Expect(testClient.Create(testCtx, cls)).To(Succeed())
	t.Cleanup(func() { _ = testClient.Delete(testCtx, cls) })

	sb := newSandboxWithClass(ns, "drained", cls.Name, asSession(""))
	g.Expect(testClient.Create(testCtx, sb)).To(Succeed())
	firstPod := runSessionVM(g, t, ns, sb.Name)
	firstUID := firstPod.UID

	// Cordon the node (kubectl cordon equivalent). Uncordon on the way
	// out so sibling scenarios are unaffected.
	setNodeUnschedulable := func(v bool) {
		g.Eventually(func() error {
			node := &corev1.Node{}
			if err := testClient.Get(testCtx, types.NamespacedName{Name: "kata-node-1"}, node); err != nil {
				return err
			}
			original := node.DeepCopy()
			node.Spec.Unschedulable = v
			return testClient.Patch(testCtx, node, client.MergeFrom(original))
		}, convergeTimeout, convergeInterval).Should(Succeed())
	}
	setNodeUnschedulable(true)
	t.Cleanup(func() { setNodeUnschedulable(false) })

	// Wait for the drain suspend to release the first Pod, then help
	// envtest finalize it so the replacement can appear.
	g.Eventually(func() bool {
		p, err := getPod(testCtx, ns, sb.Name+podspec.PodNameSuffix)
		return apierrors.IsNotFound(err) || (err == nil && p.DeletionTimestamp != nil)
	}, 30*time.Second, convergeInterval).Should(BeTrue(), "drain must release the first Pod")
	finalizeTerminatingPod(g, ns, sb.Name, firstUID)

	// The drain suspend fires, then auto-resume replaces the Pod. The
	// suspended state is transient here, so assert on its artifacts:
	// a new Pod UID plus a recorded checkpoint.
	g.Eventually(func() bool {
		p, err := getPod(testCtx, ns, sb.Name+podspec.PodNameSuffix)
		return err == nil && p.UID != firstUID && p.DeletionTimestamp == nil
	}, 30*time.Second, convergeInterval).Should(BeTrue(), "drain must produce a replacement Pod")

	// Un-cordon (the "other node" in a one-node envtest), run the new
	// VM, and watch the restore land.
	setNodeUnschedulable(false)
	runSessionVM(g, t, ns, sb.Name)
	g.Eventually(func() string {
		got, err := getSandbox(testCtx, ns, sb.Name)
		if err != nil || got.Status.Checkpoint == nil {
			return ""
		}
		return string(got.Status.Checkpoint.LastRecovery)
	}, convergeTimeout, convergeInterval).Should(Equal(string(setecv1alpha1.SessionRecoveryResumedFromCheckpoint)),
		"the drained session must resume from its checkpoint")
}

// TestSessionCheckpoint_VMLossWithoutCheckpointIsDistinct asserts the
// degraded path: a checkpoint-enabled session losing its VM before any
// checkpoint exists restarts from the durable workspace and says so
// via the distinct RestartedFromWorkspace condition — data safety is
// the workspace's job, and the degradation is never silent.
func TestSessionCheckpoint_VMLossWithoutCheckpointIsDistinct(t *testing.T) {
	g := NewWithT(t)
	ns := newNamespace(t, "sck-loss")

	cls := newSandboxClass("sck-loss-class", withSessionCheckpoint(0))
	g.Expect(testClient.Create(testCtx, cls)).To(Succeed())
	t.Cleanup(func() { _ = testClient.Delete(testCtx, cls) })

	sb := newSandboxWithClass(ns, "lost", cls.Name, asSession(""))
	g.Expect(testClient.Create(testCtx, sb)).To(Succeed())
	runSessionVM(g, t, ns, sb.Name)

	// Kill the VM (simulates the node dying between checkpoints).
	patchPodStatus(g, ns, sb.Name+podspec.PodNameSuffix, func(p *corev1.Pod) {
		p.Status.Phase = corev1.PodFailed
	})

	g.Eventually(func() string {
		got, err := getSandbox(testCtx, ns, sb.Name)
		if err != nil || got.Status.Checkpoint == nil {
			return ""
		}
		return string(got.Status.Checkpoint.LastRecovery)
	}, convergeTimeout, convergeInterval).Should(Equal(string(setecv1alpha1.SessionRecoveryRestartedFromWorkspace)),
		"VM loss without a checkpoint must surface the distinct degraded condition")

	// And the session still restarts (workspace-backed replacement Pod).
	g.Eventually(func() bool {
		p, err := getPod(testCtx, ns, sb.Name+podspec.PodNameSuffix)
		return err == nil && p.DeletionTimestamp == nil && p.Status.Phase != corev1.PodFailed
	}, convergeTimeout, convergeInterval).Should(BeTrue(), "the session must restart against its workspace")
}

// TestSessionCheckpoint_TeardownDeletesKEK asserts session teardown
// crypto-erases the checkpoints: the per-session KEK Secret is deleted
// before the Sandbox finalizer releases.
func TestSessionCheckpoint_TeardownDeletesKEK(t *testing.T) {
	g := NewWithT(t)
	ns := newNamespace(t, "sck-td")

	cls := newSandboxClass("sck-td-class", withSessionCheckpoint(0))
	g.Expect(testClient.Create(testCtx, cls)).To(Succeed())
	t.Cleanup(func() { _ = testClient.Delete(testCtx, cls) })

	sb := newSandboxWithClass(ns, "ender", cls.Name, asSession(""))
	g.Expect(testClient.Create(testCtx, sb)).To(Succeed())
	runSessionVM(g, t, ns, sb.Name)
	g.Eventually(func() error {
		_, err := getKEKSecret(ns, sb.Name)
		return err
	}, convergeTimeout, convergeInterval).Should(Succeed())

	g.Expect(testClient.Delete(testCtx, sb)).To(Succeed())
	g.Eventually(func() bool {
		_, err := getKEKSecret(ns, sb.Name)
		return apierrors.IsNotFound(err)
	}, convergeTimeout, convergeInterval).Should(BeTrue(),
		"teardown must delete the session KEK Secret (crypto-erase of all checkpoints)")
}
