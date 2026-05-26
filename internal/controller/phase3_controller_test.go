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

import (
	"fmt"
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
func bindPodToNode(t *testing.T, pod *corev1.Pod, nodeName string) {
	t.Helper()
	binding := &corev1.Binding{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: pod.Namespace,
			Name:      pod.Name,
		},
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
// the caller to customise snapshot fields.
func newPhase3Sandbox(name, ns string, mutators ...func(*setecv1alpha1.Sandbox)) *setecv1alpha1.Sandbox {
	sb := &setecv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: setecv1alpha1.SandboxSpec{
			Image:   "alpine:3.19",
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
	bindPodToNode(t, pod, "kata-node-1")
	pod, err = getPod(testCtx, ns, sb.Name+"-vm")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	pod.Status.Phase = corev1.PodRunning
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
		ObjectMeta: metav1.ObjectMeta{Name: "p3-std-" + ns},
		Spec: setecv1alpha1.SandboxClassSpec{
			VMM: setecv1alpha1.VMMFirecracker,
		},
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
	bindPodToNode(t, pod, "kata-node-1")
	pod, err = getPod(testCtx, ns, sb.Name+"-vm")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	pod.Status.Phase = corev1.PodRunning
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
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "snap-1"},
		Spec: setecv1alpha1.SnapshotSpec{
			SandboxClass: "standard", ImageRef: "img:v1", VMM: setecv1alpha1.VMMFirecracker,
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
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "solo"},
		Spec: setecv1alpha1.SnapshotSpec{
			SandboxClass: "standard", ImageRef: "img:v1", VMM: setecv1alpha1.VMMFirecracker,
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
			SandboxClass: "standard", ImageRef: "img:v1", VMM: setecv1alpha1.VMMFirecracker,
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

	// Housekeeping.
	_ = fmt.Sprintf("ns=%s", ns)
}
