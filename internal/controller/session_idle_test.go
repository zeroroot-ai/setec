// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package controller

// Session idle-eviction scenarios (ADR-0006, setec#193): a Running
// session past its per-SandboxClass sessionIdleTimeout is evicted
// (Failed reason=IdleTimeout, Pod deleted), while a session whose
// activity annotation keeps moving — what the frontend does while a
// caller is attached — and any ephemeral Sandbox are left alone.
//
// The idle clock is floored at the Sandbox's creation timestamp, which
// the API server owns, so these scenarios use short real timeouts
// rather than backdated fixtures. That also makes them end-to-end
// honest about the deadline-driven requeue: with no client and no Pod
// events, ONLY the controller's own RequeueAfter can deliver the
// reconcile that evicts, so an eviction observed here proves the
// wall-clock scheduling works.
//
// Envtest has no kubelet, so Pod Running status is driven by the tests
// via patchPodStatus.

import (
	"testing"
	"time"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
	"github.com/zeroroot-ai/setec/internal/podspec"
	"github.com/zeroroot-ai/setec/internal/status"
)

// sessionIdleTestTimeout is the class idle threshold used by the
// scenarios: long enough that a 1s heartbeat cadence trivially outruns
// it, short enough that eviction lands well inside an Eventually.
const sessionIdleTestTimeout = 4 * time.Second

// withSessionIdleTimeout gives a SandboxClass the idle-eviction knob.
// d is fixed: every caller uses the package test timeout.
func withSessionIdleTimeout() func(*setecv1alpha1.SandboxClass) {
	const d = sessionIdleTestTimeout
	return func(c *setecv1alpha1.SandboxClass) {
		c.Spec.SessionIdleTimeout = &metav1.Duration{Duration: d}
	}
}

// stampActivity sets the Sandbox's last-activity annotation to the
// given instant, the way the frontend does on Attach and heartbeat.
func stampActivity(g Gomega, ns, name string, at time.Time) {
	g.Eventually(func() error {
		sb, err := getSandbox(testCtx, ns, name)
		if err != nil {
			return err
		}
		original := sb.DeepCopy()
		if sb.Annotations == nil {
			sb.Annotations = map[string]string{}
		}
		sb.Annotations[setecv1alpha1.AnnotationLastActivity] = at.UTC().Format(time.RFC3339)
		return testClient.Patch(testCtx, sb, client.MergeFrom(original))
	}, convergeTimeout, convergeInterval).Should(Succeed())
}

// markPodRunning drives the Pod to Running so the Sandbox derives a
// Running phase — the only phase the idle policy evaluates.
func markPodRunning(g Gomega, ns, sbName string) {
	t := metav1.NewTime(time.Now())
	patchPodStatus(g, ns, sbName+podspec.PodNameSuffix, func(p *corev1.Pod) {
		p.Status.Phase = corev1.PodRunning
		p.Status.StartTime = &t
	})
}

// phaseAndReason reads back "<phase>/<reason>" for terse assertions.
func phaseAndReason(ns, name string) string {
	current, err := getSandbox(testCtx, ns, name)
	if err != nil {
		return err.Error()
	}
	return string(current.Status.Phase) + "/" + current.Status.Reason
}

// TestSessionIdle_IdleSessionIsEvicted asserts a session no caller ever
// attached to is evicted once the class idle window elapses — driven
// purely by the controller's own deadline requeue, since nothing else
// generates events here.
func TestSessionIdle_IdleSessionIsEvicted(t *testing.T) {
	g := NewWithT(t)
	ns := newNamespace(t, "sid-evict")

	cls := newSandboxClass("sid-evict-class", withSessionIdleTimeout())
	g.Expect(testClient.Create(testCtx, cls)).To(Succeed())
	t.Cleanup(func() { _ = testClient.Delete(testCtx, cls) })

	sb := newSandboxWithClass(ns, "idler", cls.Name, asSession(""))
	g.Expect(testClient.Create(testCtx, sb)).To(Succeed())
	waitForPod(g, ns, sb.Name)
	markPodRunning(g, ns, sb.Name)

	g.Eventually(func() string {
		return phaseAndReason(ns, sb.Name)
	}, 4*sessionIdleTestTimeout, convergeInterval).Should(Equal("Failed/"+status.ReasonIdleTimeout),
		"an idle session should be evicted with reason IdleTimeout")

	// The eviction also removes the VM Pod (terminal Pods delete fully
	// in envtest).
	g.Eventually(func() bool {
		p, err := getPod(testCtx, ns, sb.Name+podspec.PodNameSuffix)
		return apierrors.IsNotFound(err) || (err == nil && p.DeletionTimestamp != nil)
	}, convergeTimeout, convergeInterval).Should(BeTrue(), "the idle session's Pod should be deleted")

	// The workspace outlives the eviction: only explicit teardown
	// (deleting the Sandbox) wipes it, so an operator can still salvage
	// session data after an idle eviction.
	pvc, err := getPVC(ns, podspec.WorkspacePVCName(sb.Name))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(pvc.DeletionTimestamp).To(BeNil())
}

// TestSessionIdle_ActiveSessionIsExempt asserts a session whose
// activity annotation keeps moving — the frontend's Attach/heartbeat
// behavior — outlives many idle windows, and is evicted only after the
// activity stops (the disconnect starting the idle clock).
func TestSessionIdle_ActiveSessionIsExempt(t *testing.T) {
	g := NewWithT(t)
	ns := newNamespace(t, "sid-active")

	cls := newSandboxClass("sid-active-class", withSessionIdleTimeout())
	g.Expect(testClient.Create(testCtx, cls)).To(Succeed())
	t.Cleanup(func() { _ = testClient.Delete(testCtx, cls) })

	sb := newSandboxWithClass(ns, "busy", cls.Name, asSession(""))
	g.Expect(testClient.Create(testCtx, sb)).To(Succeed())
	waitForPod(g, ns, sb.Name)
	markPodRunning(g, ns, sb.Name)

	g.Eventually(func() string {
		return phaseAndReason(ns, sb.Name)
	}, convergeTimeout, convergeInterval).Should(Equal("Running/"))

	// Heartbeat at 1s cadence for two full idle windows; the session
	// must stay Running throughout.
	deadline := time.Now().Add(2 * sessionIdleTestTimeout)
	for time.Now().Before(deadline) {
		stampActivity(g, ns, sb.Name, time.Now())
		g.Expect(phaseAndReason(ns, sb.Name)).To(Equal("Running/"),
			"a session with fresh activity must never be idle-evicted")
		time.Sleep(time.Second)
	}

	// The caller "disconnects": activity stops, and one idle window
	// later the session is evicted.
	g.Eventually(func() string {
		return phaseAndReason(ns, sb.Name)
	}, 4*sessionIdleTestTimeout, convergeInterval).Should(Equal("Failed/"+status.ReasonIdleTimeout),
		"once activity stops the idle clock runs out")
}

// TestSessionIdle_EphemeralIsUntouched asserts the idle policy never
// evaluates ephemeral Sandboxes, whatever their class configures.
func TestSessionIdle_EphemeralIsUntouched(t *testing.T) {
	g := NewWithT(t)
	ns := newNamespace(t, "sid-eph")

	cls := newSandboxClass("sid-eph-class", withSessionIdleTimeout())
	g.Expect(testClient.Create(testCtx, cls)).To(Succeed())
	t.Cleanup(func() { _ = testClient.Delete(testCtx, cls) })

	sb := newSandboxWithClass(ns, "one-shot", cls.Name)
	g.Expect(testClient.Create(testCtx, sb)).To(Succeed())
	waitForPod(g, ns, sb.Name)
	markPodRunning(g, ns, sb.Name)

	g.Eventually(func() string {
		return phaseAndReason(ns, sb.Name)
	}, convergeTimeout, convergeInterval).Should(Equal("Running/"))

	g.Consistently(func() string {
		return phaseAndReason(ns, sb.Name)
	}, 2*sessionIdleTestTimeout, convergeInterval).Should(Equal("Running/"),
		"the idle policy must never touch an ephemeral Sandbox")
}
