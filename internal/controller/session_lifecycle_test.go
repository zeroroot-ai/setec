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

// Session-lifecycle scenarios (ADR-0006/0007): the durable workspace
// PVC is created before the Pod, an exited session VM is restarted
// rather than finished, and explicit teardown wipes the workspace.
//
// Envtest runs kube-apiserver + etcd only: no kubelet, no GC, no CSI.
// PVCs keep their pvc-protection finalizer forever here (no controller
// to strip it), so the teardown test asserts deletion was ACCEPTED —
// DeletionTimestamp set — which is the same condition the controller's
// teardown treats as "wipe under way". Terminal Pods, by contrast, do
// finish deleting in envtest, so the restart scenario observes a real
// recreation.

import (
	"testing"
	"time"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
	"github.com/zeroroot-ai/setec/internal/podspec"
)

// asSession marks a Sandbox as session-lifecycle with an optional
// workspace size.
func asSession(size string) func(*setecv1alpha1.Sandbox) {
	return func(sb *setecv1alpha1.Sandbox) {
		lc := &setecv1alpha1.Lifecycle{Mode: setecv1alpha1.LifecycleModeSession}
		if size != "" {
			lc.Workspace = &setecv1alpha1.WorkspaceSpec{}
			q := resource.MustParse(size)
			lc.Workspace.Size = &q
		}
		sb.Spec.Lifecycle = lc
	}
}

func getPVC(ns, name string) (*corev1.PersistentVolumeClaim, error) {
	pvc := &corev1.PersistentVolumeClaim{}
	err := testClient.Get(testCtx, types.NamespacedName{Namespace: ns, Name: name}, pvc)
	return pvc, err
}

// TestSessionLifecycle_WorkspacePVCCreated asserts a session Sandbox
// gets its deterministic workspace claim (RWO, requested size,
// owner-referenced) and that its Pod mounts it.
func TestSessionLifecycle_WorkspacePVCCreated(t *testing.T) {
	g := NewWithT(t)
	ns := newNamespace(t, "slc-pvc")

	sb := newSandbox(ns, "sess", asSession("2Gi"))
	g.Expect(testClient.Create(testCtx, sb)).To(Succeed())

	pod := waitForPod(g, ns, sb.Name)

	pvcName := podspec.WorkspacePVCName(sb.Name)
	pvc, err := getPVC(ns, pvcName)
	g.Expect(err).NotTo(HaveOccurred(), "workspace PVC should exist once the Pod does")

	g.Expect(pvc.Spec.AccessModes).To(ConsistOf(corev1.ReadWriteOnce))
	g.Expect(pvc.Spec.Resources.Requests[corev1.ResourceStorage]).To(Equal(resource.MustParse("2Gi")))
	g.Expect(pvc.Labels).To(HaveKeyWithValue(podspec.SandboxLabelKey, sb.Name))
	g.Expect(pvc.OwnerReferences).To(HaveLen(1))
	g.Expect(pvc.OwnerReferences[0].Name).To(Equal(sb.Name))

	// The Pod mounts the claim at /workspace.
	found := false
	for _, v := range pod.Spec.Volumes {
		if v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == pvcName {
			found = true
		}
	}
	g.Expect(found).To(BeTrue(), "session Pod should mount the workspace PVC")

	// The Sandbox carries the workspace finalizer so teardown is
	// deterministic.
	g.Eventually(func() []string {
		current, err := getSandbox(testCtx, ns, sb.Name)
		if err != nil {
			return nil
		}
		return current.Finalizers
	}, convergeTimeout, convergeInterval).Should(ContainElement("setec.zeroroot.ai/workspace-teardown"))
}

// TestSessionLifecycle_EphemeralGetsNoPVC asserts the default lifecycle
// creates no workspace claim and takes no finalizer — today's behavior,
// unchanged.
func TestSessionLifecycle_EphemeralGetsNoPVC(t *testing.T) {
	g := NewWithT(t)
	ns := newNamespace(t, "slc-eph")

	sb := newSandbox(ns, "eph")
	g.Expect(testClient.Create(testCtx, sb)).To(Succeed())
	waitForPod(g, ns, sb.Name)

	_, err := getPVC(ns, podspec.WorkspacePVCName(sb.Name))
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "ephemeral Sandbox must not get a workspace PVC")

	current, err := getSandbox(testCtx, ns, sb.Name)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(current.Finalizers).To(BeEmpty())
}

// TestSessionLifecycle_VMExitRestartsPod asserts a session VM that
// exits is deleted and recreated against the same workspace claim
// instead of finishing the Sandbox.
func TestSessionLifecycle_VMExitRestartsPod(t *testing.T) {
	g := NewWithT(t)
	ns := newNamespace(t, "slc-restart")

	sb := newSandbox(ns, "restarter", asSession(""))
	g.Expect(testClient.Create(testCtx, sb)).To(Succeed())
	pod := waitForPod(g, ns, sb.Name)
	firstUID := pod.UID

	// Simulate the workload exiting cleanly.
	startTime := metav1.NewTime(time.Now().Add(-1 * time.Minute))
	patchPodStatus(g, ns, pod.Name, func(p *corev1.Pod) {
		p.Status.Phase = corev1.PodSucceeded
		p.Status.StartTime = &startTime
	})

	// The controller deletes the exited Pod (terminal Pods finish
	// deleting even in envtest) and recreates it: a fresh Pod with a
	// new UID appears under the same deterministic name.
	g.Eventually(func() bool {
		p, err := getPod(testCtx, ns, sb.Name+podspec.PodNameSuffix)
		return err == nil && p.UID != firstUID && p.DeletionTimestamp == nil
	}, convergeTimeout, convergeInterval).Should(BeTrue(), "a fresh session Pod should be recreated")

	// The Sandbox never finished: a session ends only on explicit
	// teardown, so the workload exiting must not yield a terminal phase.
	current, err := getSandbox(testCtx, ns, sb.Name)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(current.Status.Phase).NotTo(BeElementOf(
		setecv1alpha1.SandboxPhaseCompleted, setecv1alpha1.SandboxPhaseFailed))

	// The workspace claim survived the restart.
	pvc, err := getPVC(ns, podspec.WorkspacePVCName(sb.Name))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(pvc.DeletionTimestamp).To(BeNil())
}

// TestSessionLifecycle_TeardownDeletesPVC asserts explicit teardown
// (deleting the Sandbox) deletes the workspace claim and releases the
// Sandbox.
func TestSessionLifecycle_TeardownDeletesPVC(t *testing.T) {
	g := NewWithT(t)
	ns := newNamespace(t, "slc-teardown")

	sb := newSandbox(ns, "enders", asSession(""))
	g.Expect(testClient.Create(testCtx, sb)).To(Succeed())
	waitForPod(g, ns, sb.Name)
	pvcName := podspec.WorkspacePVCName(sb.Name)
	_, err := getPVC(ns, pvcName)
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(testClient.Delete(testCtx, sb)).To(Succeed())

	// The workspace claim's deletion must be accepted (envtest cannot
	// finish it: the pvc-protection finalizer has no controller here).
	g.Eventually(func() bool {
		pvc, err := getPVC(ns, pvcName)
		if apierrors.IsNotFound(err) {
			return true
		}
		return err == nil && pvc.DeletionTimestamp != nil
	}, convergeTimeout, convergeInterval).Should(BeTrue(), "workspace PVC should be deleted at session end")

	// The finalizer must release the Sandbox once the wipe is under way.
	g.Eventually(func() bool {
		_, err := getSandbox(testCtx, ns, sb.Name)
		return apierrors.IsNotFound(err)
	}, convergeTimeout, convergeInterval).Should(BeTrue(), "Sandbox should be fully deleted after workspace teardown")
}
