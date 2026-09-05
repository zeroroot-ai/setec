// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

// Phase 2 integration tests extending the envtest suite in
// sandbox_controller_test.go. These scenarios lock in the new
// SandboxClass, NetworkPolicy, and metrics paths. They reuse the
// package-wide testEnv / testClient / manager wired in suite_test.go.

package controller

import (
	"testing"
	"time"

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
// Scenario: SandboxClass.Spec.Tolerations propagates onto the Sandbox Pod.
// ---------------------------------------------------------------------------

// TestPhase2_SandboxClassTolerations locks in setec#115: a SandboxClass
// carrying Tolerations must produce a Pod that tolerates the matching
// taint, mirroring how NodeSelector is merged in createPod. Without this a
// Sandbox can never schedule onto a tainted NodePool (e.g. a Karpenter
// pool reserved for sandbox-host nodes).
func TestPhase2_SandboxClassTolerations(t *testing.T) {
	g := NewWithT(t)
	ns := newNamespace(t, "p2-tolerations")

	wantToleration := corev1.Toleration{
		Key:      "setec.zeroroot.ai/sandbox-host",
		Operator: corev1.TolerationOpEqual,
		Value:    "true",
		Effect:   corev1.TaintEffectNoSchedule,
	}

	cls := newSandboxClass("p2-tainted", func(c *setecv1alpha1.SandboxClass) {
		c.Spec.Tolerations = []corev1.Toleration{wantToleration}
	})
	g.Expect(testClient.Create(testCtx, cls)).To(Succeed())
	t.Cleanup(func() { _ = testClient.Delete(testCtx, cls) })

	sb := newSandboxWithClass(ns, "tolerant", cls.Name)
	g.Expect(testClient.Create(testCtx, sb)).To(Succeed())

	pod := waitForPod(g, ns, sb.Name)
	g.Expect(pod.Spec.Tolerations).To(ContainElement(wantToleration))
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
	// Egress has DNS (rule 0) plus one rule per Allow entry.
	g.Expect(len(np.Spec.Egress)).To(BeNumerically(">=", 3))

	// Each allow rule names the addresses its declared host resolves to,
	// not 0.0.0.0/0 on the declared port (setec#130). Asserting the peers
	// is what distinguishes an allow-list from a port filter.
	peers := make([]string, 0, len(np.Spec.Egress[1:]))
	for _, rule := range np.Spec.Egress[1:] {
		for _, peer := range rule.To {
			g.Expect(peer.IPBlock).ToNot(BeNil())
			g.Expect(peer.IPBlock.CIDR).ToNot(Equal(netpol.AllCIDR),
				"an allow-list entry must name its destination, not all of public address space")
			peers = append(peers, peer.IPBlock.CIDR)
		}
	}
	g.Expect(peers).To(ConsistOf("203.0.113.10/32", "198.51.100.7/32"))

	// The declared names stay on the annotations as the human-readable
	// record of what was asked for.
	g.Expect(np.Annotations).To(HaveKeyWithValue("setec.zeroroot.ai/allow-443", "api.example.com"))
	g.Expect(np.Annotations).To(HaveKeyWithValue("setec.zeroroot.ai/allow-9090", "metrics.example.com"))
	g.Expect(np.Annotations).ToNot(HaveKey(netpol.AnnotationUnresolved))
}

// ---------------------------------------------------------------------------
// Scenario: an allow-list entry whose host does not resolve is dropped
// from the policy rather than widened to all of public address space
// (setec#130).
// ---------------------------------------------------------------------------

func TestPhase2_NetworkPolicyUnresolvableHostIsDropped(t *testing.T) {
	g := NewWithT(t)
	ns := newNamespace(t, "p2-netpol-unresolved")

	sb := newSandbox(ns, "unresolvable", func(s *setecv1alpha1.Sandbox) {
		s.Spec.Network = &setecv1alpha1.Network{
			Mode: setecv1alpha1.NetworkModeEgressAllowList,
			Allow: []setecv1alpha1.NetworkAllow{
				{Host: "nowhere.invalid", Port: 443},
			},
		}
	})
	g.Expect(testClient.Create(testCtx, sb)).To(Succeed())

	np := &networkingv1.NetworkPolicy{}
	g.Eventually(func() error {
		return testClient.Get(testCtx, types.NamespacedName{
			Namespace: ns,
			Name:      sb.Name + netpol.NetworkPolicySuffix,
		}, np)
	}, convergeTimeout, convergeInterval).Should(Succeed())

	// DNS only: the unresolvable destination contributes no rule, and in
	// particular no 0.0.0.0/0 rule on port 443.
	g.Expect(np.Spec.Egress).To(HaveLen(1))
	g.Expect(np.Annotations).To(HaveKeyWithValue(
		netpol.AnnotationUnresolved, "nowhere.invalid:443"))
}

// ---------------------------------------------------------------------------
// Scenario: a Sandbox that declares no network block still gets a policy.
//
// This replaces the former "mode=full produces no NetworkPolicy" case.
// Omitting spec.network no longer means "unpoliced"; it resolves to
// deny-all, and the Pod does not exist until the policy does.
// ---------------------------------------------------------------------------

func TestPhase2_NetworkPolicyAlwaysGenerated(t *testing.T) {
	g := NewWithT(t)
	ns := newNamespace(t, "p2-netpol-default")

	sb := newSandbox(ns, "unstated", func(s *setecv1alpha1.Sandbox) {
		s.Spec.Network = nil
	})
	g.Expect(testClient.Create(testCtx, sb)).To(Succeed())

	np := &networkingv1.NetworkPolicy{}
	g.Eventually(func() error {
		return testClient.Get(testCtx, types.NamespacedName{
			Namespace: ns,
			Name:      sb.Name + netpol.NetworkPolicySuffix,
		}, np)
	}, convergeTimeout, convergeInterval).Should(Succeed())

	g.Expect(np.Spec.PolicyTypes).To(ConsistOf(
		networkingv1.PolicyTypeIngress,
		networkingv1.PolicyTypeEgress,
	))
	g.Expect(np.Spec.Egress).To(BeEmpty(), "an unstated posture must deny egress, not permit it")
	g.Expect(np.Spec.Ingress).To(BeEmpty())

	// The Pod is created only after the policy exists.
	_ = waitForPod(g, ns, sb.Name)
}

// ---------------------------------------------------------------------------
// Scenario: the policy exists before the Pod does.
//
// Ordering is the whole point: a Pod that is admitted before its policy is
// written can send traffic in the gap between the two writes.
// ---------------------------------------------------------------------------

func TestPhase2_NetworkPolicyPrecedesPod(t *testing.T) {
	g := NewWithT(t)
	ns := newNamespace(t, "p2-netpol-order")

	sb := newSandbox(ns, "ordered", func(s *setecv1alpha1.Sandbox) {
		s.Spec.Network = &setecv1alpha1.Network{Mode: setecv1alpha1.NetworkModeExternalOnly}
	})
	g.Expect(testClient.Create(testCtx, sb)).To(Succeed())

	// Whenever the Pod is observable, the policy must already be there.
	// Polling both and asserting the implication catches an inverted
	// order without depending on reconcile timing.
	g.Eventually(func() bool {
		if _, err := getPod(testCtx, ns, sb.Name+podspec.PodNameSuffix); err != nil {
			return false
		}
		np := &networkingv1.NetworkPolicy{}
		return testClient.Get(testCtx, types.NamespacedName{
			Namespace: ns,
			Name:      sb.Name + netpol.NetworkPolicySuffix,
		}, np) == nil
	}, convergeTimeout, convergeInterval).Should(BeTrue())

	g.Consistently(func() bool {
		_, podErr := getPod(testCtx, ns, sb.Name+podspec.PodNameSuffix)
		if podErr != nil {
			// No Pod yet is fine; the invariant only binds once it exists.
			return true
		}
		np := &networkingv1.NetworkPolicy{}
		return testClient.Get(testCtx, types.NamespacedName{
			Namespace: ns,
			Name:      sb.Name + netpol.NetworkPolicySuffix,
		}, np) == nil
	}, "2s", convergeInterval).Should(BeTrue(), "a Pod existed without its NetworkPolicy")
}

// ---------------------------------------------------------------------------
// Scenario: external-only reaches public address space but not reserved
// ranges. This is the posture a scanning workload runs under, so the test
// asserts both halves — the permissive half as much as the restrictive one.
// ---------------------------------------------------------------------------

func TestPhase2_NetworkPolicyExternalOnlyShape(t *testing.T) {
	g := NewWithT(t)
	ns := newNamespace(t, "p2-netpol-ext")

	sb := newSandbox(ns, "scanner", func(s *setecv1alpha1.Sandbox) {
		s.Spec.Network = &setecv1alpha1.Network{Mode: setecv1alpha1.NetworkModeExternalOnly}
	})
	g.Expect(testClient.Create(testCtx, sb)).To(Succeed())

	np := &networkingv1.NetworkPolicy{}
	g.Eventually(func() error {
		return testClient.Get(testCtx, types.NamespacedName{
			Namespace: ns,
			Name:      sb.Name + netpol.NetworkPolicySuffix,
		}, np)
	}, convergeTimeout, convergeInterval).Should(Succeed())

	var public *networkingv1.IPBlock
	for _, rule := range np.Spec.Egress {
		for _, peer := range rule.To {
			if peer.IPBlock != nil && peer.IPBlock.CIDR == netpol.AllCIDR {
				public = peer.IPBlock
				g.Expect(rule.Ports).To(BeEmpty(), "arbitrary destination ports must stay reachable")
			}
		}
	}
	g.Expect(public).ToNot(BeNil(), "external targets must remain reachable")
	g.Expect(public.Except).To(ConsistOf(testNetPolConfig.ReservedCIDRs))
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

// ---------------------------------------------------------------------------
// Scenario: the namespace-wide baseline deny.
//
// Every other policy the operator writes selects on the
// setec.zeroroot.ai/sandbox label, so it confines Pods the operator built.
// A Pod created in the namespace by any other route wears no such label,
// is selected by no policy, and is therefore unrestricted. These tests
// assert the one policy that does not depend on that labelling.
// ---------------------------------------------------------------------------

func TestPhase2_NamespaceBaselineSelectsEveryPod(t *testing.T) {
	g := NewWithT(t)
	ns := newNamespace(t, "p2-baseline")

	sb := newSandbox(ns, "any", func(s *setecv1alpha1.Sandbox) {
		s.Spec.Network = &setecv1alpha1.Network{Mode: setecv1alpha1.NetworkModeExternalOnly}
	})
	g.Expect(testClient.Create(testCtx, sb)).To(Succeed())

	baseline := &networkingv1.NetworkPolicy{}
	g.Eventually(func() error {
		return testClient.Get(testCtx, types.NamespacedName{
			Namespace: ns, Name: netpol.BaselineName,
		}, baseline)
	}, convergeTimeout, convergeInterval).Should(Succeed())

	g.Expect(baseline.Spec.PodSelector.MatchLabels).To(BeEmpty(),
		"the baseline must select every Pod, not only labelled Sandbox Pods")
	g.Expect(baseline.Spec.PodSelector.MatchExpressions).To(BeEmpty(),
		"the baseline must select every Pod, not only labelled Sandbox Pods")
	g.Expect(baseline.Spec.Egress).To(BeEmpty())
	g.Expect(baseline.Spec.Ingress).To(BeEmpty())
	g.Expect(baseline.Spec.PolicyTypes).To(ConsistOf(
		networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress))

	// It must not be owned by the Sandbox: a namespace-scoped policy that
	// is garbage-collected with one Sandbox reopens the hole between runs.
	g.Expect(baseline.OwnerReferences).To(BeEmpty(),
		"the baseline must outlive any individual Sandbox")
}

func TestPhase2_NamespaceBaselinePrecedesPod(t *testing.T) {
	g := NewWithT(t)
	ns := newNamespace(t, "p2-baseline-order")

	sb := newSandbox(ns, "ordered", func(s *setecv1alpha1.Sandbox) {
		s.Spec.Network = &setecv1alpha1.Network{Mode: setecv1alpha1.NetworkModeExternalOnly}
	})
	g.Expect(testClient.Create(testCtx, sb)).To(Succeed())

	g.Consistently(func() bool {
		if _, err := getPod(testCtx, ns, sb.Name+podspec.PodNameSuffix); err != nil {
			return true // no Pod yet; the invariant only binds once it exists
		}
		np := &networkingv1.NetworkPolicy{}
		return testClient.Get(testCtx, types.NamespacedName{
			Namespace: ns, Name: netpol.BaselineName,
		}, np) == nil
	}, "3s", convergeInterval).Should(BeTrue(),
		"a Pod existed in the namespace before the namespace-wide baseline deny")
}

func TestPhase2_NamespaceBaselineRestoredWhenWidened(t *testing.T) {
	g := NewWithT(t)
	ns := newNamespace(t, "p2-baseline-restore")

	sb := newSandbox(ns, "restore", func(s *setecv1alpha1.Sandbox) {
		s.Spec.Network = &setecv1alpha1.Network{Mode: setecv1alpha1.NetworkModeExternalOnly}
	})
	g.Expect(testClient.Create(testCtx, sb)).To(Succeed())

	baseline := &networkingv1.NetworkPolicy{}
	key := types.NamespacedName{Namespace: ns, Name: netpol.BaselineName}
	g.Eventually(func() error {
		return testClient.Get(testCtx, key, baseline)
	}, convergeTimeout, convergeInterval).Should(Succeed())

	// Narrow the selector so it stops covering unlabelled Pods — the
	// exact mutation that would silently reopen the hole.
	baseline.Spec.PodSelector = metav1.LabelSelector{
		MatchLabels: map[string]string{podspec.SandboxLabelKey: sb.Name},
	}
	g.Expect(testClient.Update(testCtx, baseline)).To(Succeed())

	// Nudge a reconcile and assert the operator puts it back.
	g.Eventually(func() bool {
		nudge := &setecv1alpha1.Sandbox{}
		if err := testClient.Get(testCtx, types.NamespacedName{Namespace: ns, Name: sb.Name}, nudge); err != nil {
			return false
		}
		if nudge.Annotations == nil {
			nudge.Annotations = map[string]string{}
		}
		nudge.Annotations["setec.zeroroot.ai/test-nudge"] = time.Now().Format(time.RFC3339Nano)
		_ = testClient.Update(testCtx, nudge)

		current := &networkingv1.NetworkPolicy{}
		if err := testClient.Get(testCtx, key, current); err != nil {
			return false
		}
		return len(current.Spec.PodSelector.MatchLabels) == 0 &&
			len(current.Spec.PodSelector.MatchExpressions) == 0
	}, convergeTimeout, convergeInterval).Should(BeTrue(),
		"a narrowed baseline selector was not reconciled back to podSelector: {}")
}

// TestPhase2_SandboxClassInstallsWithoutVMM guards the install path. The
// chart ships SandboxClasses that state their isolation through
// spec.runtime.backend and set no spec.vmm, because vmm is deprecated.
// While the CRD marked vmm required the API server rejected every one of
// them with "spec.vmm: Required value" — no class installed, no class
// resolved, and every Sandbox silently fell back to deny-all. This asserts
// against a real API server that such a class is admitted.
func TestPhase2_SandboxClassInstallsWithoutVMM(t *testing.T) {
	g := NewWithT(t)

	cls := &setecv1alpha1.SandboxClass{
		ObjectMeta: metav1.ObjectMeta{Name: "no-vmm-class"},
		Spec: setecv1alpha1.SandboxClassSpec{
			Runtime:            &setecv1alpha1.SandboxClassRuntime{Backend: "kata-fc"},
			DefaultNetworkMode: setecv1alpha1.NetworkModeExternalOnly,
		},
	}
	g.Expect(testClient.Create(testCtx, cls)).To(Succeed(),
		"a SandboxClass that declares spec.runtime.backend and no spec.vmm must be admitted")
	t.Cleanup(func() { _ = testClient.Delete(testCtx, cls) })

	fetched := &setecv1alpha1.SandboxClass{}
	g.Expect(testClient.Get(testCtx, types.NamespacedName{Name: cls.Name}, fetched)).To(Succeed())
	g.Expect(string(fetched.Spec.VMM)).To(BeEmpty())
}
