// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package controller

import (
	"context"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
	"github.com/zeroroot-ai/setec/internal/class"
)

// TestClassNotFoundExpired covers the grace-period predicate directly
// (setec#299), pinning the boundary arithmetic without waiting on a controller
// loop.
func TestClassNotFoundExpired(t *testing.T) {
	g := NewWithT(t)
	base := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)

	// Fresh: inside the window, keep waiting. This is the one legitimately
	// transient case — a Sandbox created alongside its class, before this
	// controller's cache has observed the class.
	g.Expect(classNotFoundExpired(sandboxCreatedAt(base), base.Add(time.Second), 0)).To(BeFalse(),
		"a Sandbox created a second ago must stay Pending, not fail")
	g.Expect(classNotFoundExpired(sandboxCreatedAt(base), base.Add(classNotFoundGrace-time.Second), 0)).To(BeFalse(),
		"one second before the deadline is still inside the grace window")

	// At and past the deadline: terminal.
	g.Expect(classNotFoundExpired(sandboxCreatedAt(base), base.Add(classNotFoundGrace), 0)).To(BeTrue(),
		"exactly at the deadline the class is not coming back")
	g.Expect(classNotFoundExpired(sandboxCreatedAt(base), base.Add(24*time.Hour), 0)).To(BeTrue(),
		"the staging case: 23h stuck Pending must be terminal")

	// No creation stamp (hand-built object): treated as fresh rather than
	// instantly failed, so a malformed object is never destroyed by this path.
	g.Expect(classNotFoundExpired(sandboxCreatedAt(time.Time{}), base, 0)).To(BeFalse(),
		"a Sandbox with no creationTimestamp must not be failed instantly")
}

// TestOrphanReapDue covers the reap deadline, which sits one retention window
// past the grace deadline and is measured from the same immutable clock.
func TestOrphanReapDue(t *testing.T) {
	g := NewWithT(t)
	base := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	grace, retention := 5*time.Minute, time.Hour

	due, remaining := orphanReapDue(sandboxCreatedAt(base), base.Add(grace), grace, retention)
	g.Expect(due).To(BeFalse(), "a Sandbox that only just went terminal must not be reaped")
	g.Expect(remaining).To(Equal(retention), "the whole retention window is still ahead of it")

	due, remaining = orphanReapDue(sandboxCreatedAt(base), base.Add(grace+retention-time.Second), grace, retention)
	g.Expect(due).To(BeFalse(), "one second short of the deadline the object survives")
	g.Expect(remaining).To(Equal(time.Second), "the requeue must land exactly on the deadline")

	due, _ = orphanReapDue(sandboxCreatedAt(base), base.Add(grace+retention), grace, retention)
	g.Expect(due).To(BeTrue(), "at the deadline the orphan is collected")

	due, _ = orphanReapDue(sandboxCreatedAt(base), base.Add(42*time.Hour), grace, retention)
	g.Expect(due).To(BeTrue(), "the staging case: a day-old orphan is long overdue")

	due, _ = orphanReapDue(sandboxCreatedAt(time.Time{}), base, grace, retention)
	g.Expect(due).To(BeFalse(), "a Sandbox with no creationTimestamp must never be deleted by this path")
}

// TestOrphanedSandboxLifecycle is the regression test for the staging pile-up
// in setec#299: 10 Sandboxes sat Pending/ClassNotFound, the oldest ~23h,
// because an unresolvable class requeued forever.
//
// Nothing ever created a Pod for them, so nothing was unschedulable, so no
// autoscaler reacted and the objects simply accumulated — making a genuinely
// stuck dispatch indistinguishable from CI litter.
//
// The whole three-stage ramp is driven through real Reconcile calls against a
// fake client, so each stage is proven to hand over to the next: Pending while
// the class could still show up, terminal Failed once it cannot, and deleted
// once the diagnosis window has passed. Stage 3 is the one that actually stops
// the pile growing — a terminal object that is never collected is still an
// object nobody can distinguish from a live one at a glance.
func TestOrphanedSandboxLifecycle(t *testing.T) {
	const (
		grace     = 5 * time.Minute
		retention = time.Hour
	)

	for _, tc := range []struct {
		name       string
		age        time.Duration
		wantPhase  setecv1alpha1.SandboxPhase
		wantRemain time.Duration // 0 = expect no requeue
		wantGone   bool
	}{
		{
			name:       "inside the grace window it stays Pending and re-checks",
			age:        time.Second,
			wantPhase:  setecv1alpha1.SandboxPhasePending,
			wantRemain: classNotFoundRequeue,
		},
		{
			name:       "past the grace window it fails terminally and waits to be reaped",
			age:        grace + time.Minute,
			wantPhase:  setecv1alpha1.SandboxPhaseFailed,
			wantRemain: retention - time.Minute,
		},
		{
			name:     "past the retention window it is deleted",
			age:      grace + retention + time.Minute,
			wantGone: true,
		},
		{
			name:     "the staging case: a 23h orphan is collected on the next reconcile",
			age:      23 * time.Hour,
			wantGone: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)

			// No SandboxClass object exists: classRef cannot resolve, which is
			// exactly the staging state after a per-run class was deleted.
			sb := newSandboxWithClass("default", "orphaned", "deleted-per-run-class")
			sb.CreationTimestamp = metav1.NewTime(time.Now().Add(-tc.age))
			c, r := newOrphanReconciler(g, sb, grace, retention)

			res, err := reconcileSandbox(r, sb)
			g.Expect(err).ToNot(HaveOccurred())

			got := &setecv1alpha1.Sandbox{}
			getErr := c.Get(context.Background(),
				types.NamespacedName{Namespace: sb.Namespace, Name: sb.Name}, got)

			if tc.wantGone {
				g.Expect(apierrors.IsNotFound(getErr)).To(BeTrue(),
					"an orphan past its retention window must be deleted — a terminal object that is never collected is still a growing pile")
				g.Expect(res.RequeueAfter).To(BeZero(), "nothing left to requeue for")
				return
			}

			g.Expect(getErr).ToNot(HaveOccurred())
			g.Expect(got.Status.Phase).To(Equal(tc.wantPhase))
			g.Expect(got.Status.Reason).To(Equal("ClassNotFound"))
			// The requeue is the only thing that moves this Sandbox to its next
			// stage: it has no Pod, so no other event will ever wake it.
			g.Expect(res.RequeueAfter).To(BeNumerically("~", tc.wantRemain, 10*time.Second),
				"the reconciler must come back when the next deadline falls due")

			if tc.wantPhase == setecv1alpha1.SandboxPhaseFailed {
				g.Expect(isTerminalPhase(got.Status.Phase)).To(BeTrue(),
					"Failed must be terminal so no Pod is recreated for a Sandbox that can never run")
			}
		})
	}
}

// TestTerminalSandboxSurvivesDeletedClass pins the guard that keeps the
// setec#299 reaper aimed only at garbage.
//
// A Sandbox that ran and finished holds a result its creator owns, and its
// class being deleted afterwards says nothing about it. Two ways that could go
// wrong, both covered here: patchPendingStatus writes Pending unconditionally,
// so an unguarded ClassNotFound branch drags a Completed Sandbox BACK to
// Pending — violating the terminal-phase invariant documented on
// SandboxStatus.Phase — and from there it would go on to be failed and reaped
// as though it were an orphan.
func TestTerminalSandboxSurvivesDeletedClass(t *testing.T) {
	for _, phase := range []setecv1alpha1.SandboxPhase{
		setecv1alpha1.SandboxPhaseCompleted,
		setecv1alpha1.SandboxPhaseFailed,
	} {
		t.Run(string(phase), func(t *testing.T) {
			g := NewWithT(t)

			sb := newSandboxWithClass("default", "finished", "deleted-per-run-class")
			// Far past every deadline the orphan path knows about, but its
			// finish instant is unknown (no LastTransitionTime), so the
			// ephemeral auto-destroy path defers rather than reaps.
			sb.CreationTimestamp = metav1.NewTime(time.Now().Add(-42 * time.Hour))
			sb.Status.Phase = phase
			sb.Status.Reason = "WorkloadExited"

			c, r := newOrphanReconciler(g, sb, 5*time.Minute, time.Hour)

			res, err := reconcileSandbox(r, sb)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(res.RequeueAfter).To(BeNumerically(">", time.Duration(0)),
				"a terminal ephemeral Sandbox is requeued for its own finished-TTL auto-destroy (ADR-0006), not dragged onto the ClassNotFound path")

			got := &setecv1alpha1.Sandbox{}
			g.Expect(c.Get(context.Background(),
				types.NamespacedName{Namespace: sb.Namespace, Name: sb.Name}, got)).To(Succeed(),
				"a Sandbox that already ran must not be reaped by the ClassNotFound path just because its class was cleaned up afterwards")
			g.Expect(got.Status.Phase).To(Equal(phase),
				"a terminal Sandbox must never be rolled back by the ClassNotFound path")
			g.Expect(got.Status.Reason).To(Equal("WorkloadExited"),
				"the reason recorded by the run that finished must survive")
		})
	}
}

// sandboxCreatedAt builds a bare Sandbox stamped with the given creation time.
// A zero time leaves the stamp unset, which is how a hand-built object looks.
func sandboxCreatedAt(at time.Time) *setecv1alpha1.Sandbox {
	sb := &setecv1alpha1.Sandbox{}
	if !at.IsZero() {
		sb.CreationTimestamp = metav1.NewTime(at)
	}
	return sb
}

// newOrphanReconciler wires a SandboxReconciler over a fake client holding
// only the given Sandbox — no SandboxClass, which is the orphan condition
// under test. The grace and retention windows are injected so the ramp can be
// driven through real reconciles instead of wall-clock waits.
func newOrphanReconciler(
	g *WithT,
	sb *setecv1alpha1.Sandbox,
	grace, retention time.Duration,
) (client.Client, *SandboxReconciler) {
	scheme := runtime.NewScheme()
	g.Expect(clientgoscheme.AddToScheme(scheme)).To(Succeed())
	g.Expect(setecv1alpha1.AddToScheme(scheme)).To(Succeed())

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(sb).
		WithStatusSubresource(sb).
		Build()

	return c, &SandboxReconciler{
		Client:             c,
		Scheme:             scheme,
		Recorder:           events.NewFakeRecorder(16),
		Runtimes:           testRuntimeRegistry,
		RuntimeCfg:         testRuntimeCfg,
		ClassResolver:      class.NewResolver(c),
		NetPol:             testNetPolConfig,
		ClassNotFoundGrace: grace,
		OrphanRetention:    retention,
	}
}

func reconcileSandbox(r *SandboxReconciler, sb *setecv1alpha1.Sandbox) (ctrl.Result, error) {
	return r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: sb.Namespace, Name: sb.Name},
	})
}
