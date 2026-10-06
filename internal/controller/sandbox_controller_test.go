// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

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
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

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

// testImage is the image of the suite Sandboxes. A launcher Sandbox needs a
// digest: its disk belongs to one digest.
const testImage = "docker.io/library/python@sha256:02108f5d322dd89f1c9e552442c25acb0543dfdbc455693a5599624f20d9155d"

// newSandbox constructs a minimal-but-valid Sandbox with the supplied name
// and namespace. Optional overrides let individual scenarios add lifecycle
// timeouts without duplicating the boilerplate.
func newSandbox(ns, name string, mods ...func(*setecv1alpha1.Sandbox)) *setecv1alpha1.Sandbox {
	sb := &setecv1alpha1.Sandbox{
		Name:      name,
		Namespace: ns,
		Spec: setecv1alpha1.SandboxSpec{
			Image:   testImage,
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
// behavior. Envtest has no kubelet, so we are the only driver of Pod state.
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
// reference, as a launcher Pod with no RuntimeClass. This exercises the
// happy path of podspec.BuildLauncher + the controller's Create invocation.
func TestScenario1_PodCreation(t *testing.T) {
	g := NewWithT(t)
	ns := newNamespace(t, "s1")

	sb := newSandbox(ns, "happy")
	g.Expect(testClient.Create(testCtx, sb)).To(Succeed())

	pod := waitForPod(g, ns, sb.Name)

	// Pod name is derived from the Sandbox name with the -vm suffix.
	g.Expect(pod.Name).To(Equal(sb.Name + podspec.PodNameSuffix))

	// The one backend: a launcher Pod, with no RuntimeClass.
	g.Expect(pod.Spec.RuntimeClassName).To(BeNil())
	g.Expect(pod.Spec.Containers[0].Name).To(Equal(podspec.LauncherContainerName))

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
		p.Status.Conditions = podReadyConditions()
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
			Name: podspec.LauncherContainerName,
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
			Name: podspec.LauncherContainerName,
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
		p.Status.Conditions = podReadyConditions()
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

// errorf is a tiny wrapper around fmt.Errorf so Eventually closures read
// a little cleaner. It intentionally does not wrap %w; these errors are
// only compared for non-nilness inside gomega.
func errorf(format string, args ...any) error {
	return fmt.Errorf(format, args...)
}

// TestSamples_TheAPIServerAcceptsEachSample creates each Sandbox under
// config/samples against the real CRD schema (setec#173). A sample that the
// schema refuses is a broken first step for an operator who copies it.
func TestSamples_TheAPIServerAcceptsEachSample(t *testing.T) {
	g := NewWithT(t)
	ns := newNamespace(t, "samples")

	dir := filepath.Join("..", "..", "config", "samples")
	paths, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	g.Expect(err).NotTo(HaveOccurred())
	// A floor. A glob that matched no file would pass with no sample read.
	g.Expect(len(paths)).To(BeNumerically(">=", 2), "samples under %s", dir)

	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			g := NewWithT(t)
			data, err := os.ReadFile(path) //nolint:gosec // the path comes from a glob of the repo's own samples
			g.Expect(err).NotTo(HaveOccurred())

			sb := &setecv1alpha1.Sandbox{}
			g.Expect(yaml.UnmarshalStrict(data, sb)).To(Succeed(), "the sample names a field that the type does not have")
			g.Expect(sb.APIVersion).To(Equal(setecv1alpha1.GroupVersion.String()))
			g.Expect(sb.Kind).To(Equal("Sandbox"))

			sb.Namespace = ns
			g.Expect(testClient.Create(testCtx, sb)).To(Succeed())
			_ = testClient.Delete(testCtx, sb)
		})
	}
}

// TestAPI_MemoryCeiling proves the 64Gi memory ceiling of ADR-0146 against
// the real CRD schema (setec#172). The API server refuses more, and accepts
// the ceiling itself and a scratch value.
func TestAPI_MemoryCeiling(t *testing.T) {
	g := NewWithT(t)
	ns := newNamespace(t, "memceiling")
	mk := func(name, memory string) *setecv1alpha1.Sandbox {
		scratch := resource.MustParse("2Gi")
		sb := &setecv1alpha1.Sandbox{
			Spec: setecv1alpha1.SandboxSpec{
				Image:   "busybox",
				Command: []string{"true"},
				Resources: setecv1alpha1.Resources{
					VCPU: 1, Memory: resource.MustParse(memory), Scratch: &scratch,
				},
			},
		}
		sb.Name, sb.Namespace = name, ns
		return sb
	}
	atCeiling := mk("at-ceiling", "64Gi")
	g.Expect(testClient.Create(testCtx, atCeiling)).To(Succeed())
	_ = testClient.Delete(testCtx, atCeiling)

	err := testClient.Create(testCtx, mk("above-ceiling", "65Gi"))
	g.Expect(apierrors.IsInvalid(err)).To(BeTrue(), "65Gi must be refused as invalid, got %v", err)
	g.Expect(err.Error()).To(ContainSubstring("memory must not exceed 64Gi"))
}
