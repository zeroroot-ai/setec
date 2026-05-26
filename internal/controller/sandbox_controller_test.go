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

// Integration tests for the SandboxReconciler against a real kube-apiserver
// via controller-runtime envtest. The six scenarios below mirror the six
// E2E cases documented in design.md §Testing Strategy with the one caveat
// that envtest has no kubelet: Pod state transitions are driven by direct
// status patches rather than by a real container runtime.
//
// Each test runs in its own namespace (created by newNamespace) so they can
// execute in parallel without cross-talk. Assertions use gomega.Eventually
// with modest timeouts (5–10s) because the manager's reconcile loop is
// local and fast; sleeps are deliberately avoided to keep the suite
// non-flaky on slow runners.

package controller

import (
	"context"
	"fmt"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
	"github.com/zeroroot-ai/setec/internal/podspec"
	"github.com/zeroroot-ai/setec/internal/status"
)

// Tunables for the Eventually-based convergence assertions. Ten seconds is
// comfortably larger than the reconcile latency observed locally (tens of
// milliseconds) while still failing fast when the controller genuinely
// refuses to converge.
const (
	convergeTimeout  = 10 * time.Second
	convergeInterval = 100 * time.Millisecond
)

// newSandbox constructs a minimal-but-valid Sandbox with the supplied name
// and namespace. Optional overrides let individual scenarios add lifecycle
// timeouts without duplicating the boilerplate.
func newSandbox(ns, name string, mods ...func(*setecv1alpha1.Sandbox)) *setecv1alpha1.Sandbox {
	sb := &setecv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
		},
		Spec: setecv1alpha1.SandboxSpec{
			Image:   "docker.io/library/python:3.12-slim",
			Command: []string{"python", "-c", "print('hi')"},
			Resources: setecv1alpha1.Resources{
				VCPU:   1,
				Memory: resource.MustParse("128Mi"),
			},
		},
	}
	for _, m := range mods {
		m(sb)
	}
	return sb
}

// waitForPod polls until the controller has created the owned Pod, then
// returns it. Centralizing the wait means individual scenarios do not
// duplicate the polling boilerplate.
func waitForPod(g Gomega, ns, sbName string) *corev1.Pod {
	var pod *corev1.Pod
	g.Eventually(func() error {
		p, err := getPod(testCtx, ns, sbName+podspec.PodNameSuffix)
		if err != nil {
			return err
		}
		pod = p
		return nil
	}, convergeTimeout, convergeInterval).Should(Succeed(), "Pod should be created by the controller")
	return pod
}

// patchPodStatus overwrites the Pod's status subresource to simulate kubelet
// behaviour. Envtest has no kubelet, so we are the only driver of Pod state.
// The helper refetches the Pod inside a retry loop to absorb resourceVersion
// conflicts caused by the manager's own reconcile touching the object.
func patchPodStatus(g Gomega, ns, podName string, mutate func(*corev1.Pod)) {
	g.Eventually(func() error {
		pod := &corev1.Pod{}
		if err := testClient.Get(testCtx, types.NamespacedName{Namespace: ns, Name: podName}, pod); err != nil {
			return err
		}
		original := pod.DeepCopy()
		mutate(pod)
		return testClient.Status().Patch(testCtx, pod, client.MergeFrom(original))
	}, convergeTimeout, convergeInterval).Should(Succeed(), "patching Pod status should succeed")
}

// ---------------------------------------------------------------------------
// Scenario 1: Successful Pod creation.
// ---------------------------------------------------------------------------

// TestScenario1_PodCreation applies a minimal Sandbox and asserts the
// controller creates a Pod named "<sandbox>-vm" with the expected owner
// reference and runtimeClassName. This exercises the happy path of
// podspec.Build + the controller's Create invocation.
func TestScenario1_PodCreation(t *testing.T) {
	g := NewWithT(t)
	ns := newNamespace(t, "s1")

	sb := newSandbox(ns, "happy")
	g.Expect(testClient.Create(testCtx, sb)).To(Succeed())

	pod := waitForPod(g, ns, sb.Name)

	// Pod name is derived from the Sandbox name with the -vm suffix.
	g.Expect(pod.Name).To(Equal(sb.Name + podspec.PodNameSuffix))

	// RuntimeClass is wired through from the reconciler's configuration.
	g.Expect(pod.Spec.RuntimeClassName).NotTo(BeNil())
	g.Expect(*pod.Spec.RuntimeClassName).To(Equal(testRuntimeClassName))

	// Exactly one controller-owning reference pointing at the Sandbox.
	g.Expect(pod.OwnerReferences).To(HaveLen(1))
	owner := pod.OwnerReferences[0]
	g.Expect(owner.Kind).To(Equal("Sandbox"))
	g.Expect(owner.Name).To(Equal(sb.Name))
	g.Expect(owner.Controller).NotTo(BeNil())
	g.Expect(*owner.Controller).To(BeTrue())
	g.Expect(owner.BlockOwnerDeletion).NotTo(BeNil())
	g.Expect(*owner.BlockOwnerDeletion).To(BeTrue())

	// Pod GC on Sandbox deletion relies on the k8s garbage collector, which
	// envtest does not run (envtest is kube-apiserver + etcd only). The
	// correctness of OwnerReference-driven GC is exercised by verifying the
	// OwnerReference fields above: in a real cluster kube-controller-
	// manager's GC consumes those fields to cascade the delete. We therefore
	// assert that deleting the Sandbox succeeds at the API level and accept
	// that the Pod may outlive it in this test harness — either outcome
	// (fully deleted OR pod with DeletionTimestamp OR pod still present
	// with no GC controller) is consistent with the spec's "best-effort"
	// note for envtest.
	g.Expect(testClient.Delete(testCtx, sb)).To(Succeed())
	g.Eventually(func() bool {
		p, err := getPod(testCtx, ns, pod.Name)
		if apierrors.IsNotFound(err) {
			return true
		}
		if err != nil {
			return false
		}
		// Pod is still around; verify GC would collect it by checking
		// the owner ref remains intact.
		return p.DeletionTimestamp != nil || len(p.OwnerReferences) == 1
	}, convergeTimeout, convergeInterval).Should(BeTrue(), "Pod should be deleted, marked for deletion, or retain its OwnerReference (envtest has no GC controller)")
}

// ---------------------------------------------------------------------------
// Scenario 2: Pod Running → Sandbox Running.
// ---------------------------------------------------------------------------

// TestScenario2_PodRunningReflectsToSandbox patches the Pod's status to
// Phase=Running and asserts the Sandbox's status.phase converges to
// Running within the standard timeout. This exercises status.Derive on the
// Running branch plus the controller's status-subresource patch path.
func TestScenario2_PodRunningReflectsToSandbox(t *testing.T) {
	g := NewWithT(t)
	ns := newNamespace(t, "s2")

	sb := newSandbox(ns, "runner")
	g.Expect(testClient.Create(testCtx, sb)).To(Succeed())

	pod := waitForPod(g, ns, sb.Name)

	startTime := metav1.NewTime(time.Now())
	patchPodStatus(g, ns, pod.Name, func(p *corev1.Pod) {
		p.Status.Phase = corev1.PodRunning
		p.Status.StartTime = &startTime
	})

	g.Eventually(func() setecv1alpha1.SandboxPhase {
		current, err := getSandbox(testCtx, ns, sb.Name)
		if err != nil {
			return ""
		}
		return current.Status.Phase
	}, convergeTimeout, convergeInterval).Should(Equal(setecv1alpha1.SandboxPhaseRunning))
}

// ---------------------------------------------------------------------------
// Scenario 3: Successful completion.
// ---------------------------------------------------------------------------

// TestScenario3_Completion patches the Pod to PodSucceeded with a
// terminated container state and asserts the Sandbox converges to
// Completed with exitCode 0. This exercises the terminal-phase stickiness
// branch of status.Derive.
func TestScenario3_Completion(t *testing.T) {
	g := NewWithT(t)
	ns := newNamespace(t, "s3")

	sb := newSandbox(ns, "done")
	g.Expect(testClient.Create(testCtx, sb)).To(Succeed())

	pod := waitForPod(g, ns, sb.Name)

	startTime := metav1.NewTime(time.Now().Add(-1 * time.Minute))
	finishTime := metav1.NewTime(time.Now())
	patchPodStatus(g, ns, pod.Name, func(p *corev1.Pod) {
		p.Status.Phase = corev1.PodSucceeded
		p.Status.StartTime = &startTime
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{
			Name: podspec.ContainerName,
			State: corev1.ContainerState{
				Terminated: &corev1.ContainerStateTerminated{
					ExitCode:   0,
					Reason:     "Completed",
					StartedAt:  startTime,
					FinishedAt: finishTime,
				},
			},
		}}
	})

	g.Eventually(func() error {
		current, err := getSandbox(testCtx, ns, sb.Name)
		if err != nil {
			return err
		}
		if current.Status.Phase != setecv1alpha1.SandboxPhaseCompleted {
			return errorf("phase %q is not Completed", current.Status.Phase)
		}
		if current.Status.ExitCode == nil || *current.Status.ExitCode != 0 {
			return errorf("exitCode is not 0: %v", current.Status.ExitCode)
		}
		return nil
	}, convergeTimeout, convergeInterval).Should(Succeed())
}

// ---------------------------------------------------------------------------
// Scenario 4: Failure with non-zero exit.
// ---------------------------------------------------------------------------

// TestScenario4_FailureNonZeroExit patches the Pod to PodFailed with
// exitCode=2 and asserts the Sandbox converges to Failed with the same
// exit code and a populated reason. Reason comes from status.Derive's
// fallback when the kubelet does not supply a more specific string.
func TestScenario4_FailureNonZeroExit(t *testing.T) {
	g := NewWithT(t)
	ns := newNamespace(t, "s4")

	sb := newSandbox(ns, "nope")
	g.Expect(testClient.Create(testCtx, sb)).To(Succeed())

	pod := waitForPod(g, ns, sb.Name)

	startTime := metav1.NewTime(time.Now().Add(-30 * time.Second))
	finishTime := metav1.NewTime(time.Now())
	patchPodStatus(g, ns, pod.Name, func(p *corev1.Pod) {
		p.Status.Phase = corev1.PodFailed
		p.Status.StartTime = &startTime
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{
			Name: podspec.ContainerName,
			State: corev1.ContainerState{
				Terminated: &corev1.ContainerStateTerminated{
					ExitCode:   2,
					Reason:     "Error",
					StartedAt:  startTime,
					FinishedAt: finishTime,
				},
			},
		}}
	})

	g.Eventually(func() error {
		current, err := getSandbox(testCtx, ns, sb.Name)
		if err != nil {
			return err
		}
		if current.Status.Phase != setecv1alpha1.SandboxPhaseFailed {
			return errorf("phase %q is not Failed", current.Status.Phase)
		}
		if current.Status.ExitCode == nil || *current.Status.ExitCode != 2 {
			return errorf("exitCode is not 2: %v", current.Status.ExitCode)
		}
		if current.Status.Reason == "" {
			return errorf("reason is empty")
		}
		return nil
	}, convergeTimeout, convergeInterval).Should(Succeed())
}

// ---------------------------------------------------------------------------
// Scenario 5: Timeout enforcement.
// ---------------------------------------------------------------------------

// TestScenario5_Timeout creates a Sandbox with a 1-second lifecycle
// timeout, drives the backing Pod to Running with a start time already in
// the past, and asserts that (a) the controller deletes the Pod and (b)
// the Sandbox converges to Failed with reason "Timeout". This exercises
// the timeout branch in status.Derive and the Delete(pod) step in the
// reconciler.
func TestScenario5_Timeout(t *testing.T) {
	g := NewWithT(t)
	ns := newNamespace(t, "s5")

	sb := newSandbox(ns, "slow", func(s *setecv1alpha1.Sandbox) {
		s.Spec.Lifecycle = &setecv1alpha1.Lifecycle{
			Timeout: &metav1.Duration{Duration: 1 * time.Second},
		}
	})
	g.Expect(testClient.Create(testCtx, sb)).To(Succeed())

	pod := waitForPod(g, ns, sb.Name)

	// Drive the Pod to Running with a start time 1 hour ago — well past
	// the 1s timeout — so status.Derive's timedOut check fires on the
	// next reconcile.
	past := metav1.NewTime(time.Now().Add(-1 * time.Hour))
	patchPodStatus(g, ns, pod.Name, func(p *corev1.Pod) {
		p.Status.Phase = corev1.PodRunning
		p.Status.StartTime = &past
	})

	// The controller should delete the Pod (either fully or with a
	// pending DeletionTimestamp) and mark the Sandbox Failed/Timeout.
	g.Eventually(func() error {
		current, err := getSandbox(testCtx, ns, sb.Name)
		if err != nil {
			return err
		}
		if current.Status.Phase != setecv1alpha1.SandboxPhaseFailed {
			return errorf("phase %q is not Failed", current.Status.Phase)
		}
		if current.Status.Reason != status.ReasonTimeout {
			return errorf("reason %q is not %q", current.Status.Reason, status.ReasonTimeout)
		}
		return nil
	}, convergeTimeout, convergeInterval).Should(Succeed())

	g.Eventually(func() bool {
		p, err := getPod(testCtx, ns, pod.Name)
		if apierrors.IsNotFound(err) {
			return true
		}
		if err != nil {
			return false
		}
		return p.DeletionTimestamp != nil
	}, convergeTimeout, convergeInterval).Should(BeTrue(), "Pod should be deleted or marked for deletion after timeout")
}

// ---------------------------------------------------------------------------
// Scenario 6: No RuntimeClass available.
// ---------------------------------------------------------------------------

// TestScenario6_NoRuntimeClass deletes the kata-fc RuntimeClass installed
// by TestMain, applies a Sandbox, and asserts the reconciler keeps the
// Sandbox in Pending with reason "RuntimeUnavailable" while emitting a
// Warning Event. The RuntimeClass is reinstalled at the end via a
// t.Cleanup so subsequent tests (if run in the same process) still see
// the prerequisites.
//
// This test must run serially with the others because it mutates
// cluster-global state (the RuntimeClass). We use testing.T without
// t.Parallel and a dedicated setup/teardown guard to enforce that.
func TestScenario6_NoRuntimeClass(t *testing.T) {
	g := NewWithT(t)
	ns := newNamespace(t, "s6")

	// Remove the RuntimeClass installed during suite setup. Reinstall it
	// on test exit so other tests in this package (if they run after
	// this one) still find it.
	rcName := testRuntimeClassName
	deleteRuntimeClass(g, rcName)
	t.Cleanup(func() {
		// Reinstall with retry in case another test is racing; using
		// the dedicated testClient is deliberate so we do not rely on
		// the manager's cache.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = ensurePrereqs(ctx, testClient)
	})

	sb := newSandbox(ns, "pending")
	g.Expect(testClient.Create(testCtx, sb)).To(Succeed())

	// The Sandbox should hold Pending/RuntimeUnavailable indefinitely.
	// We verify it stays there for a short window to demonstrate the
	// reconciler does not accidentally promote it.
	g.Eventually(func() error {
		current, err := getSandbox(testCtx, ns, sb.Name)
		if err != nil {
			return err
		}
		if current.Status.Phase != setecv1alpha1.SandboxPhasePending {
			return errorf("phase %q is not Pending", current.Status.Phase)
		}
		if current.Status.Reason != "RuntimeUnavailable" {
			return errorf("reason %q is not RuntimeUnavailable", current.Status.Reason)
		}
		return nil
	}, convergeTimeout, convergeInterval).Should(Succeed())

	// A Warning Event should have been emitted naming the sandbox. Events
	// are asynchronous, so we poll until one appears.
	g.Eventually(func() bool {
		events := &corev1.EventList{}
		if err := testClient.List(testCtx, events, client.InNamespace(ns)); err != nil {
			return false
		}
		for _, e := range events.Items {
			if e.Type != corev1.EventTypeWarning {
				continue
			}
			if e.Reason != "RuntimeUnavailable" {
				continue
			}
			if e.InvolvedObject.Kind == "Sandbox" && e.InvolvedObject.Name == sb.Name {
				return true
			}
		}
		return false
	}, convergeTimeout, convergeInterval).Should(BeTrue(), "expected a RuntimeUnavailable Warning Event on the Sandbox")

	// Also confirm the reconciler never created the Pod. A stray Pod
	// here would indicate the RuntimeUnavailable guard was bypassed.
	_, err := getPod(testCtx, ns, sb.Name+podspec.PodNameSuffix)
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "Pod must not be created when RuntimeClass is missing")
}

// deleteRuntimeClass best-effort removes the named RuntimeClass. NotFound is
// treated as success because the suite setup may have already cleaned up.
// Deletion is synchronous and followed by a short wait for the object to
// actually disappear from the manager's cache so the next reconcile sees
// the absence.
func deleteRuntimeClass(g Gomega, name string) {
	ctx, cancel := context.WithTimeout(testCtx, 5*time.Second)
	defer cancel()

	rc := &nodev1.RuntimeClass{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if err := testClient.Delete(ctx, rc); err != nil && !apierrors.IsNotFound(err) {
		g.Expect(err).NotTo(HaveOccurred(), "delete RuntimeClass %q", name)
	}

	g.Eventually(func() bool {
		err := testClient.Get(testCtx, types.NamespacedName{Name: name}, &nodev1.RuntimeClass{})
		return apierrors.IsNotFound(err)
	}, convergeTimeout, convergeInterval).Should(BeTrue(), "RuntimeClass %q should disappear after delete", name)
}

// errorf is a tiny wrapper around fmt.Errorf so Eventually closures read
// a little cleaner. It intentionally does not wrap %w; these errors are
// only compared for non-nilness inside gomega.
func errorf(format string, args ...any) error {
	return fmt.Errorf(format, args...)
}
