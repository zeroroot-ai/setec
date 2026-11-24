// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package controller

// The finishedSessionRetention of a class (docs/design/lifecycles.md): a
// session that reached Completed or Failed is deleted after that time, and
// its workspace claim goes with it. A class with no retention keeps its
// finished sessions.

import (
	"context"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
	"github.com/zeroroot-ai/setec/internal/podspec"
	"github.com/zeroroot-ai/setec/internal/status"
)

// finishedSession builds a session Sandbox that reached phase at finishedAt.
func finishedSession(phase setecv1alpha1.SandboxPhase, finishedAt *time.Time) *setecv1alpha1.Sandbox {
	sb := terminalEphemeral("ended", phase, finishedAt)
	sb.Spec.Lifecycle = &setecv1alpha1.Lifecycle{Mode: setecv1alpha1.LifecycleModeSession}
	sb.Status.Reason = status.ReasonIdleTimeout
	return sb
}

func classWithRetention(d *metav1.Duration) *setecv1alpha1.SandboxClass {
	return newSandboxClass("retain", func(c *setecv1alpha1.SandboxClass) {
		c.Spec.FinishedSessionRetention = d
	})
}

// TestReapFinishedSession covers each decision of the reap.
func TestReapFinishedSession(t *testing.T) {
	hour := &metav1.Duration{Duration: time.Hour}
	long := time.Now().Add(-2 * time.Hour)
	short := time.Now().Add(-10 * time.Minute)

	type reapCase struct {
		name       string
		sb         *setecv1alpha1.Sandbox
		cls        *setecv1alpha1.SandboxClass
		wantReaped bool
		wantRemain time.Duration
	}
	live := []setecv1alpha1.SandboxPhase{
		setecv1alpha1.SandboxPhaseRunning,
		setecv1alpha1.SandboxPhasePending,
		setecv1alpha1.SandboxPhasePaused,
		setecv1alpha1.SandboxPhaseSuspended,
		setecv1alpha1.SandboxPhaseSnapshotting,
		setecv1alpha1.SandboxPhaseRestoring,
	}
	tests := make([]reapCase, 0, 8+len(live))
	tests = append(tests,
		reapCase{
			name:       "a Failed session past the retention is deleted",
			sb:         finishedSession(setecv1alpha1.SandboxPhaseFailed, &long),
			cls:        classWithRetention(hour),
			wantReaped: true,
		},
		reapCase{
			name:       "a Completed session past the retention is deleted",
			sb:         finishedSession(setecv1alpha1.SandboxPhaseCompleted, &long),
			cls:        classWithRetention(hour),
			wantReaped: true,
		},
		reapCase{
			name:       "a session inside the retention stays and the time that remains comes back",
			sb:         finishedSession(setecv1alpha1.SandboxPhaseFailed, &short),
			cls:        classWithRetention(hour),
			wantRemain: 50 * time.Minute,
		},
		reapCase{
			name: "a class with no retention keeps a finished session",
			sb:   finishedSession(setecv1alpha1.SandboxPhaseFailed, &long),
			cls:  classWithRetention(nil),
		},
		reapCase{
			name: "a retention of zero keeps a finished session",
			sb:   finishedSession(setecv1alpha1.SandboxPhaseFailed, &long),
			cls:  classWithRetention(&metav1.Duration{}),
		},
		reapCase{
			name: "no class keeps a finished session",
			sb:   finishedSession(setecv1alpha1.SandboxPhaseFailed, &long),
		},
		reapCase{
			name:       "a session with no finish time is never deleted",
			sb:         finishedSession(setecv1alpha1.SandboxPhaseFailed, nil),
			cls:        classWithRetention(hour),
			wantRemain: time.Hour,
		},
		reapCase{
			name: "an ephemeral Sandbox is not in scope",
			sb:   terminalEphemeral("ended", setecv1alpha1.SandboxPhaseFailed, &long),
			cls:  classWithRetention(hour),
		},
	)
	for _, phase := range live {
		tests = append(tests, reapCase{
			name: "a " + string(phase) + " session is never deleted",
			sb:   finishedSession(phase, &long),
			cls:  classWithRetention(hour),
		})
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			c, r := newEphemeralReapReconciler(g, tc.sb, time.Minute)
			ctx := context.Background()

			remaining, reaped, err := r.reapFinishedSession(ctx, log.FromContext(ctx), tc.sb, tc.cls)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(reaped).To(Equal(tc.wantReaped))
			if tc.wantRemain == 0 {
				g.Expect(remaining).To(BeZero())
			} else {
				g.Expect(remaining).To(BeNumerically("~", tc.wantRemain, time.Minute))
			}

			_, err = getSandboxFrom(ctx, c, tc.sb)
			if tc.wantReaped {
				g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "the Sandbox must be gone")
			} else {
				g.Expect(err).NotTo(HaveOccurred(), "the Sandbox must stay")
			}
		})
	}
}

// TestRequeueNoLaterThan covers the merge of the retention window into the
// result of a reconcile.
func TestRequeueNoLaterThan(t *testing.T) {
	g := NewWithT(t)
	g.Expect(requeueNoLaterThan(resultAfter(0), 0).RequeueAfter).To(BeZero())
	g.Expect(requeueNoLaterThan(resultAfter(0), time.Minute).RequeueAfter).To(Equal(time.Minute))
	g.Expect(requeueNoLaterThan(resultAfter(time.Second), time.Minute).RequeueAfter).To(Equal(time.Second))
	g.Expect(requeueNoLaterThan(resultAfter(time.Hour), time.Minute).RequeueAfter).To(Equal(time.Minute))
	g.Expect(requeueNoLaterThan(resultAfter(time.Hour), 0).RequeueAfter).To(Equal(time.Hour))
}

// TestFinishedSessionRetention_IdleSessionLosesItsWorkspace runs the whole
// path against the API server: the idle time ends the session, and after the
// retention the operator deletes the Sandbox and its workspace claim. Only
// the requeue of the controller can close the window, because a finished
// Sandbox makes no events.
func TestFinishedSessionRetention_IdleSessionLosesItsWorkspace(t *testing.T) {
	g := NewWithT(t)
	ns := newNamespace(t, "fsr-reap")

	cls := newSandboxClass("fsr-reap-class", withSessionIdleTimeout(), func(c *setecv1alpha1.SandboxClass) {
		c.Spec.FinishedSessionRetention = &metav1.Duration{Duration: 3 * time.Second}
	})
	g.Expect(testClient.Create(testCtx, cls)).To(Succeed())
	t.Cleanup(func() { _ = testClient.Delete(testCtx, cls) })

	sb := newSandboxWithClass(ns, "idler", cls.Name, asSession(""))
	g.Expect(testClient.Create(testCtx, sb)).To(Succeed())
	waitForPod(g, ns, sb.Name)
	markPodRunning(g, ns, sb.Name)
	pvcName := podspec.WorkspacePVCName(sb.Name)
	_, err := getPVC(ns, pvcName)
	g.Expect(err).NotTo(HaveOccurred())

	g.Eventually(func() bool {
		_, err := getSandbox(testCtx, ns, sb.Name)
		return apierrors.IsNotFound(err)
	}, 6*sessionIdleTestTimeout, convergeInterval).Should(BeTrue(),
		"the operator must delete a finished session after the retention")

	// envtest cannot finish the delete of a claim: the pvc-protection
	// finalizer has no controller here.
	pvc, err := getPVC(ns, pvcName)
	if !apierrors.IsNotFound(err) {
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(pvc.DeletionTimestamp).NotTo(BeNil(), "the workspace claim must go with the session")
	}
}

// TestFinishedSessionRetention_UnsetKeepsTheSession shows that a class with
// no retention keeps a finished session and its workspace claim.
func TestFinishedSessionRetention_UnsetKeepsTheSession(t *testing.T) {
	g := NewWithT(t)
	ns := newNamespace(t, "fsr-keep")

	cls := newSandboxClass("fsr-keep-class", withSessionIdleTimeout())
	g.Expect(testClient.Create(testCtx, cls)).To(Succeed())
	t.Cleanup(func() { _ = testClient.Delete(testCtx, cls) })

	sb := newSandboxWithClass(ns, "idler", cls.Name, asSession(""))
	g.Expect(testClient.Create(testCtx, sb)).To(Succeed())
	waitForPod(g, ns, sb.Name)
	markPodRunning(g, ns, sb.Name)

	g.Eventually(func() string {
		return phaseAndReason(ns, sb.Name)
	}, 4*sessionIdleTestTimeout, convergeInterval).Should(Equal("Failed/" + status.ReasonIdleTimeout))

	g.Consistently(func() string {
		return phaseAndReason(ns, sb.Name)
	}, 5*time.Second, convergeInterval).Should(Equal("Failed/" + status.ReasonIdleTimeout))
	pvc, err := getPVC(ns, podspec.WorkspacePVCName(sb.Name))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(pvc.DeletionTimestamp).To(BeNil())
}

func resultAfter(d time.Duration) ctrl.Result { return ctrl.Result{RequeueAfter: d} }

func getSandboxFrom(ctx context.Context, c client.Client, sb *setecv1alpha1.Sandbox) (*setecv1alpha1.Sandbox, error) {
	got := &setecv1alpha1.Sandbox{}
	err := c.Get(ctx, types.NamespacedName{Namespace: sb.Namespace, Name: sb.Name}, got)
	return got, err
}
