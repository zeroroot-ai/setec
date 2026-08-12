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
	"testing"
	"time"

	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	setecgrpcv1 "github.com/zeroroot-ai/setec/api/grpc/v1"
	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
	"github.com/zeroroot-ai/setec/internal/podspec"
	"github.com/zeroroot-ai/setec/internal/snapshot/gate"
	"github.com/zeroroot-ai/setec/internal/status"
)

// TestWarmStart_GateRejectionDestroysSandbox drives a pre-warm
// eligible Sandbox against a node-agent that reports a "successful"
// restore MISSING its per-restore verifications (the shape produced by
// a node opted out via --entropy-reseed=off / --restore-uniquify=off,
// setec#191). The ADR-0005 invariant gate must reject: outcome
// Rejected, Sandbox Failed with the typed InvariantGateViolation
// reason, and the Pod holding the unverified state destroyed. Cold
// boot is deliberately NOT the fallback here — the VM already received
// the restored state.
func TestWarmStart_GateRejectionDestroysSandbox(t *testing.T) {
	g := gomega.NewWithT(t)
	ns := newNamespace(t, "ws-gate")

	const image = "ghcr.io/org/prewarm:v9"
	clsName := fmt.Sprintf("prewarm-gate-%d", time.Now().UnixNano())
	newPreWarmSandboxClass(t, clsName, image)

	// Node-side success without a single verification signal — the
	// exact shape a verification-suppressed node-agent produces.
	testDialer.client.ClaimRes = &setecgrpcv1.ClaimPoolEntryResponse{
		Claimed: true, Success: true, EntryId: "pool-entry-x",
	}
	t.Cleanup(func() { testDialer.client.ClaimRes = nil })

	sb := &setecv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sb", Namespace: ns},
		Spec: setecv1alpha1.SandboxSpec{
			SandboxClassName: clsName,
			Image:            image,
			Command:          []string{"sh"},
			Resources: setecv1alpha1.Resources{
				VCPU:   1,
				Memory: resource.MustParse("512Mi"),
			},
		},
	}
	g.Expect(testClient.Create(testCtx, sb)).To(gomega.Succeed())
	driveSandboxPodRunning(t, g, ns, sb.Name)

	// The gate refuses: terminal Failed with the typed reason and a
	// Rejected warm-start outcome.
	g.Eventually(func() string {
		got, err := getSandbox(testCtx, ns, sb.Name)
		if err != nil {
			return ""
		}
		return string(got.Status.Phase) + "/" + got.Status.Reason
	}, 10*time.Second, 250*time.Millisecond).Should(
		gomega.Equal(string(setecv1alpha1.SandboxPhaseFailed) + "/" + status.ReasonInvariantGateViolation))

	got, err := getSandbox(testCtx, ns, sb.Name)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(got.Status.WarmStart).NotTo(gomega.BeNil())
	g.Expect(got.Status.WarmStart.Outcome).To(gomega.Equal(setecv1alpha1.SandboxWarmStartRejected))
	g.Expect(got.Status.WarmStart.EntryID).To(gomega.Equal("pool-entry-x"))

	// The Pod that received the unverified restored state is destroyed.
	g.Eventually(func() bool {
		pod, perr := getPod(testCtx, ns, sb.Name+"-vm")
		if apierrors.IsNotFound(perr) {
			return true
		}
		return perr == nil && pod != nil && !pod.DeletionTimestamp.IsZero()
	}, 10*time.Second, 250*time.Millisecond).Should(gomega.BeTrue(),
		"the Pod holding unverified restored state must be deleted")
}

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
	// ADR-0005 verification — the shape a verification-suppressed
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
	newPreWarmSandboxClass(t, plain, "ghcr.io/org/x:v1")
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
	newPreWarmSandboxClass(t, annotated, "ghcr.io/org/x:v1")
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
