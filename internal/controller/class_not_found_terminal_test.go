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
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
	"github.com/zeroroot-ai/setec/internal/class"
)

// TestClassNotFoundExpired covers the grace-period predicate directly
// (setec#299). The reconcile path is exercised by the envtest scenario below;
// this pins the boundary arithmetic without waiting on a controller loop.
func TestClassNotFoundExpired(t *testing.T) {
	g := NewWithT(t)
	base := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)

	withCreation := func(at time.Time) *setecv1alpha1.Sandbox {
		sb := &setecv1alpha1.Sandbox{}
		if !at.IsZero() {
			sb.CreationTimestamp = metav1.NewTime(at)
		}
		return sb
	}

	// Fresh: inside the window, keep waiting. This is the one legitimately
	// transient case — a Sandbox created alongside its class, before this
	// controller's cache has observed the class.
	g.Expect(classNotFoundExpired(withCreation(base), base.Add(time.Second), 0)).To(BeFalse(),
		"a Sandbox created a second ago must stay Pending, not fail")
	g.Expect(classNotFoundExpired(withCreation(base), base.Add(classNotFoundGrace-time.Second), 0)).To(BeFalse(),
		"one second before the deadline is still inside the grace window")

	// At and past the deadline: terminal.
	g.Expect(classNotFoundExpired(withCreation(base), base.Add(classNotFoundGrace), 0)).To(BeTrue(),
		"exactly at the deadline the class is not coming back")
	g.Expect(classNotFoundExpired(withCreation(base), base.Add(24*time.Hour), 0)).To(BeTrue(),
		"the staging case: 23h stuck Pending must be terminal")

	// No creation stamp (hand-built object): treated as fresh rather than
	// instantly failed, so a malformed object is never destroyed by this path.
	g.Expect(classNotFoundExpired(withCreation(time.Time{}), base, 0)).To(BeFalse(),
		"a Sandbox with no creationTimestamp must not be failed instantly")
}

// TestClassNotFoundTerminalTransition is the regression test for the staging
// pile-up in setec#299: 10 Sandboxes sat Pending/ClassNotFound, the oldest for
// ~23h, because an unresolvable class requeued forever.
//
// Nothing ever created a Pod for them, so nothing was unschedulable, so no
// autoscaler reacted and the objects simply accumulated — making a genuinely
// stuck dispatch indistinguishable from CI litter.
//
// Driven through a real Reconcile against a fake client so both halves of the
// contract are proven without racing the suite's manager-driven reconciler:
// inside the grace window the Sandbox stays Pending (a class created moments
// later still wins), past it the Sandbox reaches terminal Failed and stops
// requeueing.
func TestClassNotFoundTerminalTransition(t *testing.T) {
	for _, tc := range []struct {
		name      string
		age       time.Duration
		wantPhase setecv1alpha1.SandboxPhase
		wantAgain bool // expect a requeue
	}{
		{
			name:      "inside the grace window stays Pending and requeues",
			age:       time.Second,
			wantPhase: setecv1alpha1.SandboxPhasePending,
			wantAgain: true,
		},
		{
			name:      "past the grace window fails terminally and stops requeueing",
			age:       2 * classNotFoundGrace,
			wantPhase: setecv1alpha1.SandboxPhaseFailed,
			wantAgain: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)

			sb := newSandboxWithClass("default", "orphaned", "deleted-per-run-class")
			sb.CreationTimestamp = metav1.NewTime(time.Now().Add(-tc.age))

			scheme := runtime.NewScheme()
			g.Expect(clientgoscheme.AddToScheme(scheme)).To(Succeed())
			g.Expect(setecv1alpha1.AddToScheme(scheme)).To(Succeed())

			// No SandboxClass object exists: classRef cannot resolve, which is
			// exactly the staging state after a per-run class was deleted.
			c := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(sb).
				WithStatusSubresource(sb).
				Build()

			r := &SandboxReconciler{
				Client:        c,
				Scheme:        scheme,
				Recorder:      events.NewFakeRecorder(16),
				Runtimes:      testRuntimeRegistry,
				RuntimeCfg:    testRuntimeCfg,
				ClassResolver: class.NewResolver(c),
				NetPol:        testNetPolConfig,
			}

			res, err := r.Reconcile(context.Background(), ctrl.Request{
				NamespacedName: types.NamespacedName{Namespace: sb.Namespace, Name: sb.Name},
			})
			g.Expect(err).ToNot(HaveOccurred())

			got := &setecv1alpha1.Sandbox{}
			g.Expect(c.Get(context.Background(),
				types.NamespacedName{Namespace: sb.Namespace, Name: sb.Name}, got)).To(Succeed())

			g.Expect(got.Status.Phase).To(Equal(tc.wantPhase))
			g.Expect(got.Status.Reason).To(Equal("ClassNotFound"))

			if tc.wantAgain {
				g.Expect(res.RequeueAfter).To(BeNumerically(">", 0),
					"inside the grace window the reconciler must come back to re-check")
			} else {
				g.Expect(res.RequeueAfter).To(BeZero(),
					"a terminal Sandbox must stop requeueing — the forever-Pending pile-up is the bug")
				g.Expect(isTerminalPhase(got.Status.Phase)).To(BeTrue(),
					"Failed must be a terminal phase so no Pod is recreated")
			}
		})
	}
}
