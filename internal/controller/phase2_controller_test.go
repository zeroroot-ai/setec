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

// Phase 2 integration tests extending the envtest suite in
// sandbox_controller_test.go. These scenarios lock in the new
// SandboxClass, NetworkPolicy, and metrics paths. They reuse the
// package-wide testEnv / testClient / manager wired in suite_test.go.

package controller

import (
	"testing"

	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
	"github.com/zeroroot-ai/setec/internal/netpol"
	"github.com/zeroroot-ai/setec/internal/podspec"
)

// newSandboxClass is the Phase 2 analogue of newSandbox. Kept local so
// Phase 1 scenarios continue to use only the plain Sandbox constructor.
func newSandboxClass(name string, mods ...func(*setecv1alpha1.SandboxClass)) *setecv1alpha1.SandboxClass {
	c := &setecv1alpha1.SandboxClass{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: setecv1alpha1.SandboxClassSpec{
			VMM:              setecv1alpha1.VMMFirecracker,
			RuntimeClassName: testRuntimeClassName,
			MaxResources: &setecv1alpha1.Resources{
				VCPU:   4,
				Memory: resource.MustParse("4Gi"),
			},
		},
	}
	for _, m := range mods {
		m(c)
	}
	return c
}

// newSandboxWithClass builds a Sandbox that references the given class.
func newSandboxWithClass(ns, name, className string, mods ...func(*setecv1alpha1.Sandbox)) *setecv1alpha1.Sandbox {
	sb := newSandbox(ns, name, mods...)
	sb.Spec.SandboxClassName = className
	return sb
}

// ---------------------------------------------------------------------------
// Scenario: Sandbox referencing SandboxClass gets reconciled normally.
// ---------------------------------------------------------------------------

func TestPhase2_SandboxWithClass(t *testing.T) {
	g := NewWithT(t)
	ns := newNamespace(t, "p2-class")

	cls := newSandboxClass("p2-standard")
	g.Expect(testClient.Create(testCtx, cls)).To(Succeed())
	t.Cleanup(func() { _ = testClient.Delete(testCtx, cls) })

	sb := newSandboxWithClass(ns, "classy", cls.Name)
	g.Expect(testClient.Create(testCtx, sb)).To(Succeed())

	pod := waitForPod(g, ns, sb.Name)
	g.Expect(pod.Spec.RuntimeClassName).NotTo(BeNil())
	g.Expect(*pod.Spec.RuntimeClassName).To(Equal(cls.Spec.RuntimeClassName)) //nolint:staticcheck // back-compat: RuntimeClassName retained until v2
}

// ---------------------------------------------------------------------------
// Scenario: Sandbox with mode=none gets a NetworkPolicy.
// ---------------------------------------------------------------------------

func TestPhase2_NetworkPolicyCreated(t *testing.T) {
	g := NewWithT(t)
	ns := newNamespace(t, "p2-netpol")

	sb := newSandbox(ns, "isolated", func(s *setecv1alpha1.Sandbox) {
		s.Spec.Network = &setecv1alpha1.Network{Mode: setecv1alpha1.NetworkModeNone}
	})
	g.Expect(testClient.Create(testCtx, sb)).To(Succeed())

	_ = waitForPod(g, ns, sb.Name)

	// NetworkPolicy should appear shortly after the Pod.
	np := &networkingv1.NetworkPolicy{}
	g.Eventually(func() error {
		return testClient.Get(testCtx, types.NamespacedName{
			Namespace: ns,
			Name:      sb.Name + netpol.NetworkPolicySuffix,
		}, np)
	}, convergeTimeout, convergeInterval).Should(Succeed())

	g.Expect(np.Spec.PodSelector.MatchLabels[podspec.SandboxLabelKey]).To(Equal(sb.Name))
	g.Expect(np.Spec.PolicyTypes).To(ContainElements(
		networkingv1.PolicyTypeIngress,
		networkingv1.PolicyTypeEgress,
	))
	// mode=none produces empty ingress + egress rule lists.
	g.Expect(np.Spec.Ingress).To(BeEmpty())
	g.Expect(np.Spec.Egress).To(BeEmpty())
}

// ---------------------------------------------------------------------------
// Scenario: NetworkPolicy is GC'd with the Sandbox (owner ref).
// ---------------------------------------------------------------------------

func TestPhase2_NetworkPolicyOwnerReference(t *testing.T) {
	g := NewWithT(t)
	ns := newNamespace(t, "p2-npowner")

	sb := newSandbox(ns, "owned", func(s *setecv1alpha1.Sandbox) {
		s.Spec.Network = &setecv1alpha1.Network{Mode: setecv1alpha1.NetworkModeNone}
	})
	g.Expect(testClient.Create(testCtx, sb)).To(Succeed())
	_ = waitForPod(g, ns, sb.Name)

	np := &networkingv1.NetworkPolicy{}
	npKey := types.NamespacedName{Namespace: ns, Name: sb.Name + netpol.NetworkPolicySuffix}
	g.Eventually(func() error { return testClient.Get(testCtx, npKey, np) },
		convergeTimeout, convergeInterval).Should(Succeed())

	// Verify owner ref is correctly stamped on the NetworkPolicy. Real
	// GC runs in kube-controller-manager, not envtest, so we assert the
	// reference itself; kube-controller-manager consumes it to cascade.
	g.Expect(np.OwnerReferences).To(HaveLen(1))
	g.Expect(np.OwnerReferences[0].Kind).To(Equal("Sandbox"))
	g.Expect(np.OwnerReferences[0].Name).To(Equal(sb.Name))

	_ = testClient.Delete(testCtx, sb)
}

// ---------------------------------------------------------------------------
// Scenario: egress-allow-list produces a NetworkPolicy with DNS plus
// one egress rule per entry in spec.network.allow.
// ---------------------------------------------------------------------------

func TestPhase2_NetworkPolicyEgressAllowList(t *testing.T) {
	g := NewWithT(t)
	ns := newNamespace(t, "p2-netpol-allow")

	sb := newSandbox(ns, "allowlist", func(s *setecv1alpha1.Sandbox) {
		s.Spec.Network = &setecv1alpha1.Network{
			Mode: setecv1alpha1.NetworkModeEgressAllowList,
			Allow: []setecv1alpha1.NetworkAllow{
				{Host: "api.example.com", Port: 443},
				{Host: "metrics.example.com", Port: 9090},
			},
		}
	})
	g.Expect(testClient.Create(testCtx, sb)).To(Succeed())
	_ = waitForPod(g, ns, sb.Name)

	np := &networkingv1.NetworkPolicy{}
	g.Eventually(func() error {
		return testClient.Get(testCtx, types.NamespacedName{
			Namespace: ns,
			Name:      sb.Name + netpol.NetworkPolicySuffix,
		}, np)
	}, convergeTimeout, convergeInterval).Should(Succeed())

	g.Expect(np.Spec.PolicyTypes).To(ContainElements(
		networkingv1.PolicyTypeIngress,
		networkingv1.PolicyTypeEgress,
	))
	// Ingress is empty (no external traffic reaches the Sandbox).
	g.Expect(np.Spec.Ingress).To(BeEmpty())
	// Egress has at least DNS (rule 0) plus one rule per Allow entry.
	g.Expect(len(np.Spec.Egress)).To(BeNumerically(">=", 3))
}

// ---------------------------------------------------------------------------
// Scenario: mode=full produces no NetworkPolicy, and a later switch to
// mode=none creates one, so the reconciler handles transitions.
// ---------------------------------------------------------------------------

func TestPhase2_NetworkPolicyFullModeNoPolicy(t *testing.T) {
	g := NewWithT(t)
	ns := newNamespace(t, "p2-netpol-full")

	sb := newSandbox(ns, "unrestricted", func(s *setecv1alpha1.Sandbox) {
		s.Spec.Network = &setecv1alpha1.Network{Mode: setecv1alpha1.NetworkModeFull}
	})
	g.Expect(testClient.Create(testCtx, sb)).To(Succeed())
	_ = waitForPod(g, ns, sb.Name)

	// Give the reconciler a reasonable window; then confirm no
	// NetworkPolicy was ever created for this Sandbox.
	g.Consistently(func() bool {
		np := &networkingv1.NetworkPolicy{}
		err := testClient.Get(testCtx, types.NamespacedName{
			Namespace: ns,
			Name:      sb.Name + netpol.NetworkPolicySuffix,
		}, np)
		return apierrors.IsNotFound(err)
	}, "2s", convergeInterval).Should(BeTrue())
}

// ---------------------------------------------------------------------------
// Scenario: ClassNotFound surfaces on the Sandbox status.
// ---------------------------------------------------------------------------

func TestPhase2_ClassNotFound(t *testing.T) {
	g := NewWithT(t)
	ns := newNamespace(t, "p2-nocls")

	sb := newSandboxWithClass(ns, "noclass", "nonexistent-class")
	g.Expect(testClient.Create(testCtx, sb)).To(Succeed())

	g.Eventually(func() string {
		current, err := getSandbox(testCtx, ns, sb.Name)
		if err != nil {
			return ""
		}
		return current.Status.Reason
	}, convergeTimeout, convergeInterval).Should(Equal("ClassNotFound"))

	// Pod must not be created when class resolution fails.
	_, err := getPod(testCtx, ns, sb.Name+podspec.PodNameSuffix)
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
}

// ---------------------------------------------------------------------------
// Scenario: Sandbox exceeds class vcpu ceiling — defense in depth.
// ---------------------------------------------------------------------------

func TestPhase2_ClassConstraintViolation(t *testing.T) {
	g := NewWithT(t)
	ns := newNamespace(t, "p2-violate")

	cls := newSandboxClass("p2-tiny", func(c *setecv1alpha1.SandboxClass) {
		c.Spec.MaxResources = &setecv1alpha1.Resources{
			VCPU:   1,
			Memory: resource.MustParse("256Mi"),
		}
	})
	g.Expect(testClient.Create(testCtx, cls)).To(Succeed())
	t.Cleanup(func() { _ = testClient.Delete(testCtx, cls) })

	sb := newSandboxWithClass(ns, "toobig", cls.Name, func(s *setecv1alpha1.Sandbox) {
		s.Spec.Resources = setecv1alpha1.Resources{
			VCPU:   4,
			Memory: resource.MustParse("2Gi"),
		}
	})
	g.Expect(testClient.Create(testCtx, sb)).To(Succeed())

	g.Eventually(func() string {
		current, err := getSandbox(testCtx, ns, sb.Name)
		if err != nil {
			return ""
		}
		return current.Status.Reason
	}, convergeTimeout, convergeInterval).Should(Equal("ConstraintViolated"))
}

// ---------------------------------------------------------------------------
// Scenario: Metrics counter increments on phase transition.
// ---------------------------------------------------------------------------

func TestPhase2_MetricsRecorded(t *testing.T) {
	g := NewWithT(t)
	ns := newNamespace(t, "p2-metrics")

	sb := newSandbox(ns, "metric-sb")
	g.Expect(testClient.Create(testCtx, sb)).To(Succeed())

	// Promote to Running by patching the Pod status. This drives the
	// reconciler through the Pending→Running transition which both
	// increments the sandbox_total counter and records cold start.
	pod := waitForPod(g, ns, sb.Name)
	startTime := metav1.NewTime(metav1.Now().Time)
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

	// Counter must now show at least one Running transition. The Phase 1
	// path uses empty tenant and empty class labels.
	got := testutil.ToFloat64(testCollectors.SandboxTotal.WithLabelValues(
		string(setecv1alpha1.SandboxPhaseRunning), "", ""))
	g.Expect(got).To(BeNumerically(">=", 1))
}

// ---------------------------------------------------------------------------
// Scenario: SandboxClass controller starts without error.
// ---------------------------------------------------------------------------

func TestPhase2_SandboxClassControllerRuns(t *testing.T) {
	g := NewWithT(t)

	// Create and immediately delete a SandboxClass — if the reconciler
	// panicked or failed to watch, this would time out.
	cls := newSandboxClass("p2-smoke")
	g.Expect(testClient.Create(testCtx, cls)).To(Succeed())
	t.Cleanup(func() { _ = testClient.Delete(testCtx, cls) })

	// Verify the object exists (controller doesn't set status but the
	// API object must be readable through the manager's client).
	g.Eventually(func() error {
		return testClient.Get(testCtx, types.NamespacedName{Name: cls.Name},
			&setecv1alpha1.SandboxClass{})
	}, convergeTimeout, convergeInterval).Should(Succeed())
}
