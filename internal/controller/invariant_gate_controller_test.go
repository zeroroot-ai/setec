// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package controller

import (
	"fmt"
	"testing"
	"time"

	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	setecgrpcv1 "github.com/zeroroot-ai/setec/api/grpc/v1"
	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
	"github.com/zeroroot-ai/setec/internal/podspec"
	"github.com/zeroroot-ai/setec/internal/snapshot/gate"
)

// TestSessionCheckpoint_GateRefusalDestroysVM drives the setec#194
// suspend/resume loop against a node-agent whose restore reports
// success WITHOUT the per-restore verifications. The invariant gate
// must refuse the resume: the VM that received the unverified
// checkpoint state is destroyed and the session recovers degraded
// from its durable workspace (RestartedFromWorkspace) — the
// unverified state is never served.
func TestSessionCheckpoint_GateRefusalDestroysVM(t *testing.T) {
	g := gomega.NewWithT(t)
	ns := newNamespace(t, "sck-gate")

	cls := newSandboxClass("sck-gate-class", withSessionCheckpoint())
	g.Expect(testClient.Create(testCtx, cls)).To(gomega.Succeed())
	t.Cleanup(func() { _ = testClient.Delete(testCtx, cls) })

	sb := newSandboxWithClass(ns, "gated", cls.Name, asSession(""))
	g.Expect(testClient.Create(testCtx, sb)).To(gomega.Succeed())
	firstPod := runSessionVM(g, t, ns, sb.Name)

	// Suspend with a healthy checkpoint.
	patchDesiredState(g, ns, sb.Name, setecv1alpha1.SandboxDesiredStateSuspended)
	g.Eventually(func() bool {
		got, err := getSandbox(testCtx, ns, sb.Name)
		return err == nil && got.Status.Checkpoint != nil && got.Status.Checkpoint.PendingRestore
	}, convergeTimeout, convergeInterval).Should(gomega.BeTrue(), "suspend must record a pending checkpoint")
	finalizeTerminatingPod(g, ns, sb.Name, firstPod.UID)

	// Degrade the node's restore response: success without any
	// docs/design/isolation.md verification — the shape a verification-suppressed
	// node-agent produces.
	testDialer.client.RestoreRes = &setecgrpcv1.RestoreSandboxResponse{Success: true}
	t.Cleanup(func() { testDialer.client.RestoreRes = nil })

	// Resume: fresh Pod, restore fires, gate refuses.
	patchDesiredState(g, ns, sb.Name, setecv1alpha1.SandboxDesiredStateRunning)
	g.Eventually(func() bool {
		p, err := getPod(testCtx, ns, sb.Name+podspec.PodNameSuffix)
		return err == nil && p.DeletionTimestamp == nil
	}, convergeTimeout, convergeInterval).Should(gomega.BeTrue(), "resume must recreate the VM Pod")
	resumedPod := runSessionVM(g, t, ns, sb.Name)

	g.Eventually(func() string {
		got, err := getSandbox(testCtx, ns, sb.Name)
		if err != nil || got.Status.Checkpoint == nil {
			return ""
		}
		return string(got.Status.Checkpoint.LastRecovery)
	}, convergeTimeout, convergeInterval).Should(
		gomega.Equal(string(setecv1alpha1.SessionRecoveryRestartedFromWorkspace)),
		"an unverified resume must degrade to restart-from-workspace, never be served")

	// The VM that received the unverified state is destroyed.
	g.Eventually(func() bool {
		p, perr := getPod(testCtx, ns, sb.Name+podspec.PodNameSuffix)
		if apierrors.IsNotFound(perr) {
			return true
		}
		return perr == nil && (p.UID != resumedPod.UID || !p.DeletionTimestamp.IsZero())
	}, convergeTimeout, convergeInterval).Should(gomega.BeTrue(),
		"the Pod that received unverified checkpoint state must be deleted")
}

// getClassCondition fetches the named condition from a SandboxClass.
func getClassCondition(name string) *metav1.Condition {
	cls := &setecv1alpha1.SandboxClass{}
	if err := testClient.Get(testCtx, types.NamespacedName{Name: name}, cls); err != nil {
		return nil
	}
	return meta.FindStatusCondition(cls.Status.Conditions, ConditionUnverifiedRestoresAllowed)
}

// TestSandboxClass_UnverifiedRestoresCondition pins the loud dev-mode
// surface on the class: Enforced by default, inert-annotation surfaced
// when the cluster dev label is absent, True only when both halves of
// the opt-out are present.
func TestSandboxClass_UnverifiedRestoresCondition(t *testing.T) {
	g := gomega.NewWithT(t)

	// 1. Plain class: condition False/Enforced.
	plain := fmt.Sprintf("gate-cond-plain-%d", time.Now().UnixNano())
	newGateTestClass(t, plain)
	g.Eventually(func() string {
		c := getClassCondition(plain)
		if c == nil {
			return ""
		}
		return string(c.Status) + "/" + c.Reason
	}, 10*time.Second, 250*time.Millisecond).Should(gomega.Equal("False/" + ReasonEnforced))

	// 2. Annotated class without the cluster dev label: still False,
	// with the inert-annotation reason surfaced. The class reconciler
	// patches status concurrently, so mutate through a re-Get + retry.
	annotated := fmt.Sprintf("gate-cond-annot-%d", time.Now().UnixNano())
	newGateTestClass(t, annotated)
	g.Eventually(func() error {
		cls := &setecv1alpha1.SandboxClass{}
		if err := testClient.Get(testCtx, types.NamespacedName{Name: annotated}, cls); err != nil {
			return err
		}
		cls.Annotations = map[string]string{gate.AllowUnverifiedRestoresAnnotation: "true"}
		return testClient.Update(testCtx, cls)
	}, 10*time.Second, 250*time.Millisecond).Should(gomega.Succeed())
	g.Eventually(func() string {
		c := getClassCondition(annotated)
		if c == nil {
			return ""
		}
		return string(c.Status) + "/" + c.Reason
	}, 10*time.Second, 250*time.Millisecond).Should(gomega.Equal("False/" + ReasonDevGateNamespaceUnlabelled))

	// 3. Label the gate namespace: the condition flips True/DevModeOptOut.
	gateNS := &corev1.Namespace{}
	g.Expect(testClient.Get(testCtx, types.NamespacedName{Name: gate.DefaultGateNamespace}, gateNS)).To(gomega.Succeed())
	if gateNS.Labels == nil {
		gateNS.Labels = map[string]string{}
	}
	gateNS.Labels[gate.DefaultAllowDevLabel] = "true"
	g.Expect(testClient.Update(testCtx, gateNS)).To(gomega.Succeed())
	t.Cleanup(func() {
		ns := &corev1.Namespace{}
		if err := testClient.Get(testCtx, types.NamespacedName{Name: gate.DefaultGateNamespace}, ns); err == nil {
			delete(ns.Labels, gate.DefaultAllowDevLabel)
			_ = testClient.Update(testCtx, ns)
		}
	})

	// Touch the class so the reconciler re-derives against the fresh
	// namespace labels (the controller watches classes, not namespaces).
	g.Eventually(func() error {
		cls := &setecv1alpha1.SandboxClass{}
		if err := testClient.Get(testCtx, types.NamespacedName{Name: annotated}, cls); err != nil {
			return err
		}
		if cls.Labels == nil {
			cls.Labels = map[string]string{}
		}
		cls.Labels["test-bump"] = fmt.Sprintf("%d", time.Now().UnixNano())
		return testClient.Update(testCtx, cls)
	}, 10*time.Second, 250*time.Millisecond).Should(gomega.Succeed())

	g.Eventually(func() string {
		c := getClassCondition(annotated)
		if c == nil {
			return ""
		}
		return string(c.Status) + "/" + c.Reason
	}, 10*time.Second, 250*time.Millisecond).Should(gomega.Equal("True/" + ReasonDevModeOptOut))
}

// newGateTestClass creates a plain SandboxClass and deletes it with the
// test.
func newGateTestClass(t *testing.T, name string) {
	t.Helper()
	cls := newSandboxClass(name)
	if err := testClient.Create(testCtx, cls); err != nil {
		t.Fatalf("create SandboxClass: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(testCtx, cls) })
}
