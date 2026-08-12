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

// maxPauseDuration enforcement scenarios (setec#202): a Paused Sandbox
// past the class cap fails with reason PauseTimeoutExceeded and loses
// its Pod — a paused microVM holds its full memory reservation, and
// the cap bounds that residency. A checkpoint-enabled session instead
// suspends (checkpoint retained, microVM released) and resumes when
// desiredState returns to Running.
//
// Like the idle-eviction scenarios, these are end-to-end honest about
// the deadline-driven requeue: a paused VM emits no events, so ONLY
// the controller's own RequeueAfter can deliver the reconcile that
// enforces the cap.

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

// pauseTimeoutTestCap is the class pause cap used by the scenarios:
// short enough that enforcement lands well inside an Eventually, long
// enough that the pause itself completes first.
const pauseTimeoutTestCap = 3 * time.Second

// withMaxPauseDuration gives a SandboxClass the pause cap.
func withMaxPauseDuration(d time.Duration) func(*setecv1alpha1.SandboxClass) {
	return func(c *setecv1alpha1.SandboxClass) {
		c.Spec.MaxPauseDuration = &metav1.Duration{Duration: d}
	}
}

// TestPauseTimeout_PausedSandboxFails asserts an ephemeral Sandbox
// left Paused past the class maxPauseDuration transitions to
// Failed/PauseTimeoutExceeded, emits the Warning Event, and loses its
// Pod — driven purely by the controller's own deadline requeue.
func TestPauseTimeout_PausedSandboxFails(t *testing.T) {
	g := NewWithT(t)
	ns := newNamespace(t, "pt-fail")

	cls := newSandboxClass("pt-fail-class", withMaxPauseDuration(pauseTimeoutTestCap))
	g.Expect(testClient.Create(testCtx, cls)).To(Succeed())
	t.Cleanup(func() { _ = testClient.Delete(testCtx, cls) })

	sb := newSandboxWithClass(ns, "pausy", cls.Name)
	g.Expect(testClient.Create(testCtx, sb)).To(Succeed())
	runSessionVM(g, t, ns, sb.Name)

	// Pause on user request; pausedAt is stamped here.
	patchDesiredState(g, ns, sb.Name, setecv1alpha1.SandboxDesiredStatePaused)
	g.Eventually(func() string {
		return phaseAndReason(ns, sb.Name)
	}, convergeTimeout, convergeInterval).Should(Equal("Paused/UserPaused"))

	// The cap fires with no external stimulus: nothing else reconciles
	// a quietly paused Sandbox.
	g.Eventually(func() string {
		return phaseAndReason(ns, sb.Name)
	}, 4*pauseTimeoutTestCap, convergeInterval).Should(Equal("Failed/"+status.ReasonPauseTimeout),
		"a Sandbox paused past maxPauseDuration should fail with PauseTimeoutExceeded")

	// The paused microVM's Pod is released.
	g.Eventually(func() bool {
		p, err := getPod(testCtx, ns, sb.Name+podspec.PodNameSuffix)
		return apierrors.IsNotFound(err) || (err == nil && p.DeletionTimestamp != nil)
	}, convergeTimeout, convergeInterval).Should(BeTrue(), "pause-timeout enforcement must delete the Pod")

	// The Warning Event names the enforcement.
	g.Eventually(func() bool {
		events := &corev1.EventList{}
		if err := testClient.List(testCtx, events, client.InNamespace(ns)); err != nil {
			return false
		}
		for _, e := range events.Items {
			if e.Type == corev1.EventTypeWarning && e.Reason == eventReasonPauseTimeout &&
				e.InvolvedObject.Kind == "Sandbox" && e.InvolvedObject.Name == sb.Name {
				return true
			}
		}
		return false
	}, convergeTimeout, convergeInterval).Should(BeTrue(),
		"expected a PauseTimeoutExceeded Warning Event on the Sandbox")
}

// TestPauseTimeout_UnboundedWithoutCap asserts the fail-open default:
// with no maxPauseDuration on the class, a Paused Sandbox stays Paused.
func TestPauseTimeout_UnboundedWithoutCap(t *testing.T) {
	g := NewWithT(t)
	ns := newNamespace(t, "pt-unbounded")

	cls := newSandboxClass("pt-unbounded-class")
	g.Expect(testClient.Create(testCtx, cls)).To(Succeed())
	t.Cleanup(func() { _ = testClient.Delete(testCtx, cls) })

	sb := newSandboxWithClass(ns, "parked", cls.Name)
	g.Expect(testClient.Create(testCtx, sb)).To(Succeed())
	runSessionVM(g, t, ns, sb.Name)

	patchDesiredState(g, ns, sb.Name, setecv1alpha1.SandboxDesiredStatePaused)
	g.Eventually(func() string {
		return phaseAndReason(ns, sb.Name)
	}, convergeTimeout, convergeInterval).Should(Equal("Paused/UserPaused"))

	g.Consistently(func() string {
		return phaseAndReason(ns, sb.Name)
	}, 2*pauseTimeoutTestCap, convergeInterval).Should(Equal("Paused/UserPaused"),
		"without a class cap a paused Sandbox must stay Paused")
}

// TestPauseTimeout_CheckpointSessionSuspends asserts the ADR-0006
// alternative: a checkpoint-enabled session paused past the cap is
// suspended — checkpoint retained, microVM released — not failed, and
// holds Suspended while desiredState stays Paused. Flipping
// desiredState back to Running resumes it from the checkpoint.
func TestPauseTimeout_CheckpointSessionSuspends(t *testing.T) {
	g := NewWithT(t)
	ns := newNamespace(t, "pt-susp")

	cls := newSandboxClass("pt-susp-class",
		withSessionCheckpoint(),
		withMaxPauseDuration(pauseTimeoutTestCap))
	g.Expect(testClient.Create(testCtx, cls)).To(Succeed())
	t.Cleanup(func() { _ = testClient.Delete(testCtx, cls) })

	sb := newSandboxWithClass(ns, "parked", cls.Name, asSession(""))
	g.Expect(testClient.Create(testCtx, sb)).To(Succeed())
	firstPod := runSessionVM(g, t, ns, sb.Name)

	patchDesiredState(g, ns, sb.Name, setecv1alpha1.SandboxDesiredStatePaused)
	g.Eventually(func() string {
		return phaseAndReason(ns, sb.Name)
	}, convergeTimeout, convergeInterval).Should(Equal("Paused/UserPaused"))

	g.Eventually(func() string {
		return phaseAndReason(ns, sb.Name)
	}, 4*pauseTimeoutTestCap, convergeInterval).Should(Equal("Suspended/"+reasonSuspendedPauseTimeout),
		"a checkpoint-enabled session paused past maxPauseDuration suspends instead of failing")
	finalizeTerminatingPod(g, ns, sb.Name, firstPod.UID)

	// The suspend retained a restorable checkpoint and cleared the
	// pause stamp (a Suspended session holds no microVM to bound).
	got, err := getSandbox(testCtx, ns, sb.Name)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(got.Status.Checkpoint).NotTo(BeNil())
	g.Expect(got.Status.Checkpoint.Ref).NotTo(BeEmpty(), "pause-timeout suspend must record the checkpoint ref")
	g.Expect(got.Status.Checkpoint.PendingRestore).To(BeTrue())
	g.Expect(got.Status.PausedAt).To(BeNil(), "suspend must clear pausedAt")

	// While desiredState stays Paused the session holds Suspended —
	// resuming would just re-pause and re-suspend in a loop.
	g.Consistently(func() bool {
		p, err := getPod(testCtx, ns, sb.Name+podspec.PodNameSuffix)
		return apierrors.IsNotFound(err) || (err == nil && p.DeletionTimestamp != nil)
	}, 2*time.Second, convergeInterval).Should(BeTrue(),
		"a pause-timeout-suspended session must stay parked while desiredState is Paused")

	// desiredState=Running wakes it: a fresh Pod appears and restores
	// from the retained checkpoint.
	patchDesiredState(g, ns, sb.Name, setecv1alpha1.SandboxDesiredStateRunning)
	g.Eventually(func() bool {
		p, err := getPod(testCtx, ns, sb.Name+podspec.PodNameSuffix)
		return err == nil && p.DeletionTimestamp == nil
	}, convergeTimeout, convergeInterval).Should(BeTrue(), "desiredState=Running must recreate the VM Pod")
	runSessionVM(g, t, ns, sb.Name)

	g.Eventually(func() string {
		got, err := getSandbox(testCtx, ns, sb.Name)
		if err != nil || got.Status.Checkpoint == nil {
			return ""
		}
		return string(got.Status.Checkpoint.LastRecovery)
	}, convergeTimeout, convergeInterval).Should(Equal(string(setecv1alpha1.SessionRecoveryResumedFromCheckpoint)),
		"the resumed session must restore from the checkpoint the suspend retained")
}

// TestNextLifecycleDeadline_PausedPhase pins the requeue arithmetic the
// enforcement rides on: a Paused Sandbox with a class cap requeues at
// the pause deadline (floored at one second), and without a cap — or
// without a pausedAt stamp — no deadline is pending.
func TestNextLifecycleDeadline_PausedPhase(t *testing.T) {
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	pausedAt := metav1.NewTime(now.Add(-time.Minute))
	sb := &setecv1alpha1.Sandbox{}
	cap30 := newSandboxClass("cap", withMaxPauseDuration(30*time.Minute))

	st := setecv1alpha1.SandboxStatus{
		Phase:    setecv1alpha1.SandboxPhasePaused,
		PausedAt: &pausedAt,
	}
	after, ok := nextLifecycleDeadline(sb, cap30, st, now)
	if !ok || after != 29*time.Minute {
		t.Fatalf("paused with cap: got (%v, %v), want (29m, true)", after, ok)
	}

	// Deadline already passed → floored at one second, never hot.
	late := setecv1alpha1.SandboxStatus{Phase: setecv1alpha1.SandboxPhasePaused, PausedAt: &pausedAt}
	after, ok = nextLifecycleDeadline(sb, newSandboxClass("tiny", withMaxPauseDuration(time.Second)), late, now)
	if !ok || after != time.Second {
		t.Fatalf("overdue pause: got (%v, %v), want (1s, true)", after, ok)
	}

	// No cap → no deadline.
	if _, ok := nextLifecycleDeadline(sb, newSandboxClass("nocap"), st, now); ok {
		t.Fatalf("paused without cap must have no pending deadline")
	}

	// No pausedAt → no deadline.
	noStamp := setecv1alpha1.SandboxStatus{Phase: setecv1alpha1.SandboxPhasePaused}
	if _, ok := nextLifecycleDeadline(sb, cap30, noStamp, now); ok {
		t.Fatalf("paused without pausedAt must have no pending deadline")
	}
}
