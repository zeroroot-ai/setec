// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package controller

import (
	"slices"
	"testing"
	"time"

	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
)

// bindPodToNode uses the pods/binding subresource to assign a node to
// a Pod — K8s forbids direct Pod.Spec.NodeName updates but a Binding
// object is the canonical way to schedule a Pod. The fake scheduler
// this emulates is otherwise absent in envtest.
// nodeName is fixed: every caller binds to the same fixture node.
func bindPodToNode(t *testing.T, pod *corev1.Pod) {
	const nodeName = "fleet-node-1"
	t.Helper()
	binding := &corev1.Binding{
		Namespace: pod.Namespace,
		Name:      pod.Name,
		Target: corev1.ObjectReference{
			Kind: "Node",
			Name: nodeName,
		},
	}
	// Use SubResource client for /binding.
	if err := testClient.SubResource("binding").Create(testCtx, pod, binding); err != nil {
		t.Fatalf("bind pod %q to %q: %v", pod.Name, nodeName, err)
	}
}

// newPhase3Sandbox constructs a minimal Phase 3 Sandbox with room for
// the caller to customize snapshot fields.
func newPhase3Sandbox(name, ns string, mutators ...func(*setecv1alpha1.Sandbox)) *setecv1alpha1.Sandbox {
	sb := &setecv1alpha1.Sandbox{
		Name: name, Namespace: ns,
		Spec: setecv1alpha1.SandboxSpec{
			Image:   testImage,
			Command: []string{"sh"},
			Resources: setecv1alpha1.Resources{
				VCPU:   1,
				Memory: resource.MustParse("512Mi"),
			},
		},
	}
	for _, m := range mutators {
		m(sb)
	}
	return sb
}

// TestPhase3_SnapshotRefMissing asserts a Sandbox referencing a
// nonexistent Snapshot lands in Pending with the SnapshotUnavailable
// reason and never spawns a Pod.
func TestPhase3_SnapshotRefMissing(t *testing.T) {
	g := gomega.NewWithT(t)
	ns := newNamespace(t, "p3-missing")

	sb := newPhase3Sandbox("sb", ns, func(sb *setecv1alpha1.Sandbox) {
		sb.Spec.SnapshotRef = &setecv1alpha1.SandboxSnapshotRef{Name: "ghost"}
	})
	if err := testClient.Create(testCtx, sb); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}

	g.Eventually(func() string {
		got, err := getSandbox(testCtx, ns, sb.Name)
		if err != nil {
			return ""
		}
		return got.Status.Reason
	}, 10*time.Second, 250*time.Millisecond).Should(gomega.Equal("SnapshotUnavailable"))

	// Pod MUST NOT exist.
	_, err := getPod(testCtx, ns, sb.Name+"-vm")
	g.Expect(err).To(gomega.HaveOccurred(), "Pod should not be created when snapshot is missing")
}

// TestPhase3_PauseResume drives the Sandbox through pause and resume
// by flipping spec.desiredState. The backing Pod must remain
// throughout.
func TestPhase3_PauseResume(t *testing.T) {
	g := gomega.NewWithT(t)
	ns := newNamespace(t, "p3-pause")

	sb := newPhase3Sandbox("sb", ns)
	if err := testClient.Create(testCtx, sb); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}

	// Force Pod to Running so the pause path is reachable. The
	// scheduler+kubelet are absent in envtest, so we set status by
	// hand.
	g.Eventually(func() bool {
		pod, err := getPod(testCtx, ns, sb.Name+"-vm")
		return err == nil && pod != nil
	}, 10*time.Second, 250*time.Millisecond).Should(gomega.BeTrue())

	pod, err := getPod(testCtx, ns, sb.Name+"-vm")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	bindPodToNode(t, pod)
	pod, err = getPod(testCtx, ns, sb.Name+"-vm")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	pod.Status.Phase = corev1.PodRunning
	pod.Status.Conditions = podReadyConditions()
	pod.Status.StartTime = &metav1.Time{Time: time.Now()}
	g.Expect(testClient.Status().Update(testCtx, pod)).To(gomega.Succeed())

	g.Eventually(func() setecv1alpha1.SandboxPhase {
		got, err := getSandbox(testCtx, ns, sb.Name)
		if err != nil {
			return ""
		}
		return got.Status.Phase
	}, 10*time.Second, 250*time.Millisecond).Should(gomega.Equal(setecv1alpha1.SandboxPhaseRunning))

	// Flip to Paused.
	got, err := getSandbox(testCtx, ns, sb.Name)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	got.Spec.DesiredState = setecv1alpha1.SandboxDesiredStatePaused
	g.Expect(testClient.Update(testCtx, got)).To(gomega.Succeed())

	g.Eventually(func() setecv1alpha1.SandboxPhase {
		s, err := getSandbox(testCtx, ns, sb.Name)
		if err != nil {
			return ""
		}
		return s.Status.Phase
	}, 10*time.Second, 250*time.Millisecond).Should(gomega.Equal(setecv1alpha1.SandboxPhasePaused))

	// Pod must still exist while paused.
	_, err = getPod(testCtx, ns, sb.Name+"-vm")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	// Resume.
	got, err = getSandbox(testCtx, ns, sb.Name)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	got.Spec.DesiredState = setecv1alpha1.SandboxDesiredStateRunning
	g.Expect(testClient.Update(testCtx, got)).To(gomega.Succeed())

	g.Eventually(func() setecv1alpha1.SandboxPhase {
		s, err := getSandbox(testCtx, ns, sb.Name)
		if err != nil {
			return ""
		}
		return s.Status.Phase
	}, 10*time.Second, 250*time.Millisecond).Should(gomega.Equal(setecv1alpha1.SandboxPhaseRunning))
}

// TestPhase3_SnapshotCreateHappyPath drives a snapshot.create=true
// Sandbox through to a Ready Snapshot CR via the fake NodeAgent.
func TestPhase3_SnapshotCreateHappyPath(t *testing.T) {
	g := gomega.NewWithT(t)
	ns := newNamespace(t, "p3-create")

	// Seed a SandboxClass so the Sandbox has a class name to
	// propagate into the resulting Snapshot CR. The Coordinator
	// copies Sandbox.spec.sandboxClassName verbatim and relies on
	// the class validator to reject mismatches elsewhere.
	cls := &setecv1alpha1.SandboxClass{
		Name: "p3-std-" + ns,
		Spec: setecv1alpha1.SandboxClassSpec{},
	}
	g.Expect(testClient.Create(testCtx, cls)).To(gomega.Succeed())
	t.Cleanup(func() { _ = testClient.Delete(testCtx, cls) })

	sb := newPhase3Sandbox("sb", ns, func(sb *setecv1alpha1.Sandbox) {
		sb.Spec.SandboxClassName = cls.Name
		sb.Spec.Snapshot = &setecv1alpha1.SandboxSnapshotSpec{
			Create: true,
			Name:   "snap-1",
			// AfterCreate default is Running.
		}
	})
	if err := testClient.Create(testCtx, sb); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}

	// Force Pod to Running AND pin to a node so the Coordinator can
	// resolve the node-agent endpoint. envtest has no scheduler so we
	// patch Spec.NodeName and Status directly.
	g.Eventually(func() bool {
		pod, err := getPod(testCtx, ns, sb.Name+"-vm")
		return err == nil && pod != nil
	}, 10*time.Second, 250*time.Millisecond).Should(gomega.BeTrue())

	pod, err := getPod(testCtx, ns, sb.Name+"-vm")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	bindPodToNode(t, pod)
	pod, err = getPod(testCtx, ns, sb.Name+"-vm")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	pod.Status.Phase = corev1.PodRunning
	pod.Status.Conditions = podReadyConditions()
	pod.Status.StartTime = &metav1.Time{Time: time.Now()}
	g.Expect(testClient.Status().Update(testCtx, pod)).To(gomega.Succeed())

	// Wait for Snapshot CR to appear. The SnapshotReconciler flips its
	// status to Ready once it sees the finalizer invariant is
	// satisfied, so we accept either Creating or Ready — the key
	// invariant is that the CR exists.
	g.Eventually(func() bool {
		snap := &setecv1alpha1.Snapshot{}
		return testClient.Get(testCtx, types.NamespacedName{Namespace: ns, Name: "snap-1"}, snap) == nil
	}, 15*time.Second, 250*time.Millisecond).Should(gomega.BeTrue())
}

// TestSnapshotFinalizer_BlocksDeleteWhileReferenced confirms the
// SnapshotReconciler keeps its finalizer while a Sandbox references
// the Snapshot.
func TestSnapshotFinalizer_BlocksDeleteWhileReferenced(t *testing.T) {
	g := gomega.NewWithT(t)
	ns := newNamespace(t, "p3-fin")

	snap := &setecv1alpha1.Snapshot{
		Namespace: ns, Name: "snap-1",
		Spec: setecv1alpha1.SnapshotSpec{
			SandboxClass: "standard", ImageRef: "img:v1",
			StorageBackend: "local-disk", StorageRef: "snap-1", Node: "node-a",
		},
	}
	g.Expect(testClient.Create(testCtx, snap)).To(gomega.Succeed())
	// Mark Ready manually. Retry on conflict because the
	// SnapshotReconciler may be patching status.referenceCount
	// concurrently.
	g.Eventually(func() error {
		cur := &setecv1alpha1.Snapshot{}
		if err := testClient.Get(testCtx, types.NamespacedName{Namespace: ns, Name: "snap-1"}, cur); err != nil {
			return err
		}
		cur.Status.Phase = setecv1alpha1.SnapshotPhaseReady
		return testClient.Status().Update(testCtx, cur)
	}, 10*time.Second, 250*time.Millisecond).Should(gomega.Succeed())

	// Create a Sandbox referencing it so ReferenceCount > 0.
	sb := newPhase3Sandbox("ref", ns, func(sb *setecv1alpha1.Sandbox) {
		sb.Spec.SnapshotRef = &setecv1alpha1.SandboxSnapshotRef{Name: "snap-1"}
	})
	g.Expect(testClient.Create(testCtx, sb)).To(gomega.Succeed())

	// Reference-count propagation is multi-hop (Sandbox create -> cache
	// sync -> indexer update -> SnapshotReconciler tick -> status
	// patch). A 30-second window covers worst-case envtest cache
	// resync.
	g.Eventually(func() int32 {
		got := &setecv1alpha1.Snapshot{}
		if err := testClient.Get(testCtx, types.NamespacedName{Namespace: ns, Name: "snap-1"}, got); err != nil {
			return -1
		}
		return got.Status.ReferenceCount
	}, 30*time.Second, 500*time.Millisecond).Should(gomega.Equal(int32(1)))

	g.Eventually(func() bool {
		got := &setecv1alpha1.Snapshot{}
		if err := testClient.Get(testCtx, types.NamespacedName{Namespace: ns, Name: "snap-1"}, got); err != nil {
			return false
		}
		return slices.Contains(got.Finalizers, setecv1alpha1.SnapshotInUseFinalizer)
	}, 10*time.Second, 250*time.Millisecond).Should(gomega.BeTrue(), "expected finalizer to be present")

	// Deletion is blocked while refCount>0.
	g.Expect(testClient.Delete(testCtx, snap)).To(gomega.Succeed())
	time.Sleep(2 * time.Second)
	got := &setecv1alpha1.Snapshot{}
	err := testClient.Get(testCtx, types.NamespacedName{Namespace: ns, Name: "snap-1"}, got)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(got.DeletionTimestamp).NotTo(gomega.BeNil())
	g.Expect(got.Finalizers).To(gomega.ContainElement(setecv1alpha1.SnapshotInUseFinalizer))
}

// TestSnapshotFinalizer_AllowsDeleteWhenFree confirms that once no
// Sandbox references the Snapshot, the finalizer is removed and the
// CR disappears.
func TestSnapshotFinalizer_AllowsDeleteWhenFree(t *testing.T) {
	g := gomega.NewWithT(t)
	ns := newNamespace(t, "p3-free")

	snap := &setecv1alpha1.Snapshot{
		Namespace: ns, Name: "solo",
		Spec: setecv1alpha1.SnapshotSpec{
			SandboxClass: "standard", ImageRef: "img:v1",
			StorageBackend: "local-disk", StorageRef: "solo", Node: "node-a",
		},
	}
	g.Expect(testClient.Create(testCtx, snap)).To(gomega.Succeed())

	// Wait for the finalizer.
	g.Eventually(func() bool {
		got := &setecv1alpha1.Snapshot{}
		_ = testClient.Get(testCtx, types.NamespacedName{Namespace: ns, Name: "solo"}, got)
		return slices.Contains(got.Finalizers, setecv1alpha1.SnapshotInUseFinalizer)
	}, 10*time.Second, 250*time.Millisecond).Should(gomega.BeTrue())

	// Delete and confirm removal.
	g.Expect(testClient.Delete(testCtx, snap)).To(gomega.Succeed())
	g.Eventually(func() bool {
		got := &setecv1alpha1.Snapshot{}
		err := testClient.Get(testCtx, types.NamespacedName{Namespace: ns, Name: "solo"}, got)
		return err != nil
	}, 15*time.Second, 250*time.Millisecond).Should(gomega.BeTrue(), "Snapshot should be fully deleted")
}

// TestSnapshotTTL_TriggersDelete creates a Snapshot with a tight TTL
// and confirms the reconciler deletes it after expiry.
func TestSnapshotTTL_TriggersDelete(t *testing.T) {
	g := gomega.NewWithT(t)
	ns := newNamespace(t, "p3-ttl")

	// TTL minimum in the webhook is 1 minute; the reconciler does not
	// enforce the minimum so we pick 2s for a fast test. The webhook
	// isn't wired into the envtest manager so this is admissible.
	snap := &setecv1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:         ns,
			Name:              "ephemeral",
			CreationTimestamp: metav1.NewTime(time.Now().Add(-3 * time.Second)), // not actually settable; see below
		},
		Spec: setecv1alpha1.SnapshotSpec{
			SandboxClass: "standard", ImageRef: "img:v1",
			StorageBackend: "local-disk", StorageRef: "ephemeral", Node: "node-a",
			TTL: &metav1.Duration{Duration: 1 * time.Second},
		},
	}
	g.Expect(testClient.Create(testCtx, snap)).To(gomega.Succeed())

	// Wait past TTL + reconcile tick.
	g.Eventually(func() bool {
		got := &setecv1alpha1.Snapshot{}
		err := testClient.Get(testCtx, types.NamespacedName{Namespace: ns, Name: "ephemeral"}, got)
		return err != nil // fully deleted
	}, 90*time.Second, 1*time.Second).Should(gomega.BeTrue(), "Snapshot should be deleted by TTL")
}

// TestSnapshotPhase_TerminatingWhileFinalizerHeld is the Terminating
// third of setec#129.
//
// SnapshotPhaseTerminating had no assignment anywhere in the repo, so a
// Snapshot whose deletion was requested and then blocked — by a live
// Sandbox reference, or by a backend erase that keeps failing — still
// reported Ready. From .status.phase alone it was indistinguishable from
// a snapshot nobody had asked to delete, which is the one case an
// operator needs to see.
//
// The reference is what makes the window observable: refCount > 0 holds
// the in-use finalizer, so the CR stays alive in the Terminating phase
// instead of disappearing before anything can read it.
func TestSnapshotPhase_TerminatingWhileFinalizerHeld(t *testing.T) {
	g := gomega.NewWithT(t)
	ns := newNamespace(t, "p3-term")

	snap := &setecv1alpha1.Snapshot{
		Namespace: ns, Name: "term-1",
		Spec: setecv1alpha1.SnapshotSpec{
			SandboxClass: "standard", ImageRef: "img:v1",
			StorageBackend: "local-disk", StorageRef: "term-1", Node: "node-a",
		},
	}
	g.Expect(testClient.Create(testCtx, snap)).To(gomega.Succeed())

	// Reach Ready first, so the assertion below cannot pass on a phase
	// that merely started out empty.
	g.Eventually(func() error {
		cur := &setecv1alpha1.Snapshot{}
		if err := testClient.Get(testCtx, types.NamespacedName{Namespace: ns, Name: "term-1"}, cur); err != nil {
			return err
		}
		cur.Status.Phase = setecv1alpha1.SnapshotPhaseReady
		return testClient.Status().Update(testCtx, cur)
	}, 10*time.Second, 250*time.Millisecond).Should(gomega.Succeed())

	// A Sandbox reference keeps the finalizer, so deletion blocks and the
	// Terminating phase stays readable.
	sb := newPhase3Sandbox("term-ref", ns, func(sb *setecv1alpha1.Sandbox) {
		sb.Spec.SnapshotRef = &setecv1alpha1.SandboxSnapshotRef{Name: "term-1"}
	})
	g.Expect(testClient.Create(testCtx, sb)).To(gomega.Succeed())

	g.Eventually(func() bool {
		got := &setecv1alpha1.Snapshot{}
		if err := testClient.Get(testCtx, types.NamespacedName{Namespace: ns, Name: "term-1"}, got); err != nil {
			return false
		}
		return slices.Contains(got.Finalizers, setecv1alpha1.SnapshotInUseFinalizer)
	}, 30*time.Second, 250*time.Millisecond).Should(gomega.BeTrue(), "expected the in-use finalizer")

	g.Expect(testClient.Delete(testCtx, snap)).To(gomega.Succeed())

	g.Eventually(func() setecv1alpha1.SnapshotPhase {
		got := &setecv1alpha1.Snapshot{}
		if err := testClient.Get(testCtx, types.NamespacedName{Namespace: ns, Name: "term-1"}, got); err != nil {
			return ""
		}
		return got.Status.Phase
	}, 30*time.Second, 250*time.Millisecond).Should(gomega.Equal(setecv1alpha1.SnapshotPhaseTerminating),
		"a Snapshot with a deletionTimestamp and a held finalizer must report Terminating")

	got := &setecv1alpha1.Snapshot{}
	g.Expect(testClient.Get(testCtx, types.NamespacedName{Namespace: ns, Name: "term-1"}, got)).To(gomega.Succeed())
	g.Expect(got.Status.Reason).NotTo(gomega.BeEmpty(), "Terminating with no reason tells an operator nothing")
}

// TestSnapshotCreate_FailedSnapshotDoesNotRunAfterCreate is the safety
// consequence of creating the Snapshot CR before the storage write
// (setec#129).
//
// The snapshot-create branch used to read "a Snapshot with this name
// exists" as "the snapshot was taken" and go straight to the afterCreate
// intent. That was true while the CR only appeared after a successful
// write. It is not true any more, and with afterCreate=Terminated the old
// reading deletes the Sandbox whose state was never saved — the exact
// data-loss shape the phases exist to prevent.
//
// The Snapshot is seeded in phase Failed rather than produced by a failing
// node-agent, so the assertion does not depend on the shared test dialer
// and cannot pass because of an unrelated RPC outcome.
func TestSnapshotCreate_FailedSnapshotDoesNotRunAfterCreate(t *testing.T) {
	g := gomega.NewWithT(t)
	ns := newNamespace(t, "p3-failskip")

	failed := &setecv1alpha1.Snapshot{
		Namespace: ns, Name: "snap-failed",
		Spec: setecv1alpha1.SnapshotSpec{
			SandboxClass: "standard", ImageRef: "img:v1",
			StorageBackend: "local-disk", Node: "node-a",
			// No storageRef: the write never completed.
		},
	}
	g.Expect(testClient.Create(testCtx, failed)).To(gomega.Succeed())
	g.Eventually(func() error {
		cur := &setecv1alpha1.Snapshot{}
		if err := testClient.Get(testCtx, types.NamespacedName{Namespace: ns, Name: "snap-failed"}, cur); err != nil {
			return err
		}
		cur.Status.Phase = setecv1alpha1.SnapshotPhaseFailed
		cur.Status.Reason = "InsufficientStorage"
		return testClient.Status().Update(testCtx, cur)
	}, 10*time.Second, 250*time.Millisecond).Should(gomega.Succeed())

	sb := newPhase3Sandbox("sb", ns, func(sb *setecv1alpha1.Sandbox) {
		sb.Spec.Snapshot = &setecv1alpha1.SandboxSnapshotSpec{
			Create:      true,
			Name:        "snap-failed",
			AfterCreate: setecv1alpha1.SandboxSnapshotAfterCreateTerminated,
		}
	})
	g.Expect(testClient.Create(testCtx, sb)).To(gomega.Succeed())

	// Drive the Sandbox to Running so the snapshot-create branch is
	// reached at all; envtest has no scheduler.
	g.Eventually(func() bool {
		pod, err := getPod(testCtx, ns, sb.Name+"-vm")
		return err == nil && pod != nil
	}, 15*time.Second, 250*time.Millisecond).Should(gomega.BeTrue())
	pod, err := getPod(testCtx, ns, sb.Name+"-vm")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	bindPodToNode(t, pod)
	pod, err = getPod(testCtx, ns, sb.Name+"-vm")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	pod.Status.Phase = corev1.PodRunning
	pod.Status.Conditions = podReadyConditions()
	pod.Status.StartTime = &metav1.Time{Time: time.Now()}
	g.Expect(testClient.Status().Update(testCtx, pod)).To(gomega.Succeed())

	// The Sandbox must report the failure and keep running. Consistently:
	// a single read could catch the instant before the reconciler acts.
	g.Eventually(func() string {
		got := &setecv1alpha1.Sandbox{}
		if err := testClient.Get(testCtx, types.NamespacedName{Namespace: ns, Name: sb.Name}, got); err != nil {
			return "gone: " + err.Error()
		}
		return got.Status.Reason
	}, 30*time.Second, 250*time.Millisecond).Should(gomega.Equal("SnapshotCreateFailed"))

	g.Consistently(func() bool {
		got := &setecv1alpha1.Sandbox{}
		err := testClient.Get(testCtx, types.NamespacedName{Namespace: ns, Name: sb.Name}, got)
		return err == nil && got.DeletionTimestamp.IsZero()
	}, 5*time.Second, 500*time.Millisecond).Should(gomega.BeTrue(),
		"afterCreate=Terminated must not delete the Sandbox when the snapshot failed")
}
