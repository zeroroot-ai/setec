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
	"context"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
	"github.com/zeroroot-ai/setec/internal/class"
)

// newEphemeralReapReconciler wires a SandboxReconciler over a fake client
// holding only the given Sandbox, with the ephemeral finished-TTL injected so
// the auto-destroy ramp (ADR-0006) can be driven through real reconciles
// instead of wall-clock waits.
func newEphemeralReapReconciler(
	g *WithT,
	sb *setecv1alpha1.Sandbox,
	retention time.Duration,
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
		EphemeralRetention: retention,
	}
}

// terminalEphemeral builds an ephemeral Sandbox already in a terminal phase,
// finished at the given instant. A nil finishedAt leaves LastTransitionTime
// unset, which is how the finish instant looks on a hand-built object.
func terminalEphemeral(name string, phase setecv1alpha1.SandboxPhase, finishedAt *time.Time) *setecv1alpha1.Sandbox {
	sb := newSandbox("default", name)
	sb.Status.Phase = phase
	sb.Status.Reason = "WorkloadExited"
	if finishedAt != nil {
		t := metav1.NewTime(*finishedAt)
		sb.Status.LastTransitionTime = &t
	}
	return sb
}

// TestEphemeralSandboxAutoDestroyedAfterRetention proves ADR-0006's
// "auto-destroy on exit": a terminal ephemeral Sandbox whose finished-TTL has
// elapsed is deleted by the reconciler, so a creator that never called Kill
// leaks nothing. Owner-reference GC (a real-cluster guarantee, not exercised
// by the fake client) then removes the Pod and NetworkPolicy.
func TestEphemeralSandboxAutoDestroyedAfterRetention(t *testing.T) {
	for _, phase := range []setecv1alpha1.SandboxPhase{
		setecv1alpha1.SandboxPhaseCompleted,
		setecv1alpha1.SandboxPhaseFailed,
	} {
		t.Run(string(phase), func(t *testing.T) {
			g := NewWithT(t)

			finished := time.Now().Add(-2 * time.Hour)
			sb := terminalEphemeral("reap-me", phase, &finished)

			c, r := newEphemeralReapReconciler(g, sb, 30*time.Minute)

			res, err := reconcileSandbox(r, sb)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(res.RequeueAfter).To(BeZero(), "the object is gone; nothing to requeue")

			got := &setecv1alpha1.Sandbox{}
			err = c.Get(context.Background(),
				types.NamespacedName{Namespace: sb.Namespace, Name: sb.Name}, got)
			g.Expect(err).To(HaveOccurred(),
				"a terminal ephemeral Sandbox past its finished-TTL must be auto-destroyed")
		})
	}
}

// TestEphemeralSandboxKeptWithinRetention proves the retention window is a
// grace period, not an eviction: a Sandbox that just finished survives so its
// creator can still Wait for the outcome and StreamLogs the captured output,
// and the reconciler requeues for exactly the remaining window.
func TestEphemeralSandboxKeptWithinRetention(t *testing.T) {
	g := NewWithT(t)

	finished := time.Now()
	sb := terminalEphemeral("keep-me", setecv1alpha1.SandboxPhaseCompleted, &finished)

	retention := 30 * time.Minute
	c, r := newEphemeralReapReconciler(g, sb, retention)

	res, err := reconcileSandbox(r, sb)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(res.RequeueAfter).To(BeNumerically("~", retention, time.Minute),
		"a Sandbox still inside its finished-TTL is requeued for the remaining window")

	got := &setecv1alpha1.Sandbox{}
	g.Expect(c.Get(context.Background(),
		types.NamespacedName{Namespace: sb.Namespace, Name: sb.Name}, got)).To(Succeed(),
		"a Sandbox inside its finished-TTL must not be reaped yet")
	g.Expect(got.Status.Phase).To(Equal(setecv1alpha1.SandboxPhaseCompleted))
}

// TestSessionSandboxExemptFromEphemeralReap proves the reaper is scoped to the
// ephemeral lifecycle. A session Sandbox ends by explicit teardown (Kill),
// which wipes its durable workspace via the finalizer; the finished-TTL path
// must never touch it, even when it is terminal and long past any window.
func TestSessionSandboxExemptFromEphemeralReap(t *testing.T) {
	g := NewWithT(t)

	finished := time.Now().Add(-2 * time.Hour)
	sb := terminalEphemeral("session", setecv1alpha1.SandboxPhaseCompleted, &finished)
	sb.Spec.Lifecycle = &setecv1alpha1.Lifecycle{Mode: setecv1alpha1.LifecycleModeSession}

	_, r := newEphemeralReapReconciler(g, sb, 30*time.Minute)

	_, handled, err := r.reapExpiredEphemeral(context.Background(), log.FromContext(context.Background()), sb)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(handled).To(BeFalse(),
		"a session Sandbox must fall through to the normal reconcile, never the ephemeral reaper")
}

// TestEphemeralReapDue exercises the window arithmetic directly, including the
// belt-and-braces nil guard: a Sandbox whose finish instant is unknown is
// never due, so it is deferred rather than deleted on an unseen timeline.
func TestEphemeralReapDue(t *testing.T) {
	g := NewWithT(t)
	now := time.Now()
	retention := 10 * time.Minute

	past := metav1.NewTime(now.Add(-11 * time.Minute))
	dueSb := &setecv1alpha1.Sandbox{Status: setecv1alpha1.SandboxStatus{LastTransitionTime: &past}}
	due, _ := ephemeralReapDue(dueSb, now, retention)
	g.Expect(due).To(BeTrue(), "finished 11m ago with a 10m window is due")

	recent := metav1.NewTime(now.Add(-1 * time.Minute))
	freshSb := &setecv1alpha1.Sandbox{Status: setecv1alpha1.SandboxStatus{LastTransitionTime: &recent}}
	due, remaining := ephemeralReapDue(freshSb, now, retention)
	g.Expect(due).To(BeFalse(), "finished 1m ago with a 10m window is not due")
	g.Expect(remaining).To(BeNumerically("~", 9*time.Minute, time.Second))

	nilSb := &setecv1alpha1.Sandbox{}
	due, remaining = ephemeralReapDue(nilSb, now, retention)
	g.Expect(due).To(BeFalse(), "an unknown finish instant is never due")
	g.Expect(remaining).To(Equal(retention))
}

// TestRunningEphemeralSandboxOutlivesFinishedTTL proves the finished-TTL
// reaper never touches a live Sandbox (setec#372): the window opens only
// on a terminal phase. An ephemeral Sandbox that has been Running for
// twice the retention, and every other live phase, falls through to the
// normal reconcile untouched, so a member that stays up for weeks is
// never auto-destroyed by the clock.
func TestRunningEphemeralSandboxOutlivesFinishedTTL(t *testing.T) {
	retention := 30 * time.Minute
	for _, phase := range []setecv1alpha1.SandboxPhase{
		setecv1alpha1.SandboxPhaseRunning,
		setecv1alpha1.SandboxPhasePending,
		setecv1alpha1.SandboxPhasePaused,
		setecv1alpha1.SandboxPhaseSnapshotting,
		setecv1alpha1.SandboxPhaseRestoring,
		setecv1alpha1.SandboxPhaseSuspended,
	} {
		t.Run(string(phase), func(t *testing.T) {
			g := NewWithT(t)

			// The last transition (into this phase) is 2x the TTL ago.
			transitioned := time.Now().Add(-2 * retention)
			sb := terminalEphemeral("long-lived", phase, &transitioned)
			sb.Status.Reason = ""

			c, r := newEphemeralReapReconciler(g, sb, retention)

			res, handled, err := r.reapExpiredEphemeral(context.Background(), log.FromContext(context.Background()), sb)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(handled).To(BeFalse(),
				"a live ephemeral Sandbox must fall through to the normal reconcile, never the finished-TTL reaper")
			g.Expect(res).To(Equal(ctrl.Result{}), "the reaper schedules nothing for a live Sandbox")

			got := &setecv1alpha1.Sandbox{}
			g.Expect(c.Get(context.Background(),
				types.NamespacedName{Namespace: sb.Namespace, Name: sb.Name}, got)).To(Succeed(),
				"a live Sandbox past 2x the finished-TTL must still exist")
			g.Expect(got.Status.Phase).To(Equal(phase))
		})
	}
}
