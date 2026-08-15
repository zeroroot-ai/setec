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

// runtime_selection_test.go exercises the multi-backend runtime selection path
// added by task 10. Three scenarios test selectRuntime directly using a
// controller-runtime fake client so they are independent of the shared
// envtest environment wired in suite_test.go.
//
//  1. Legacy path: nil Runtimes/RuntimeCfg → synthesized kata-fc Selection.
//  2. Fallback: class wants kata-qemu (no capable node), fallback to gvisor
//     (node has gvisor label) → Selection.Backend=gvisor, FellBack=true.
//  3. No capable node: class wants runc, no node advertises it → a
//     Provisional Selection, Sandbox Pending with Reason=AwaitingCapableNode,
//     the Pod created anyway, and Provisional clearing on its own once a
//     capable node joins.
//  4. Unregistered backend: nothing in the chain has a Dispatcher → the
//     terminal ErrNoEligibleRuntime with Reason=RuntimeNotEnabled.
package controller

import (
	"context"
	"errors"
	"testing"

	"github.com/go-logr/logr"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
	classpkg "github.com/zeroroot-ai/setec/internal/class"
	"github.com/zeroroot-ai/setec/internal/controller/testutil"
	runtimepkg "github.com/zeroroot-ai/setec/internal/runtime"
)

// newRSScheme builds a minimal scheme for the runtime selection unit tests.
func newRSScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(s))
	utilruntime.Must(setecv1alpha1.AddToScheme(s))
	return s
}

// newRSReconciler builds a SandboxReconciler with the supplied Registry and
// RuntimeConfig backed by a fake client seeded with the given objects.
func newRSReconciler(
	t *testing.T,
	reg *runtimepkg.Registry,
	cfg *runtimepkg.RuntimeConfig,
	objs ...client.Object,
) (*SandboxReconciler, client.Client) {
	t.Helper()
	s := newRSScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(objs...).
		// WithStatusSubresource makes the fake client enforce status as a
		// sub-resource so r.Status().Patch works correctly in selectRuntime.
		WithStatusSubresource(&setecv1alpha1.Sandbox{}).
		Build()

	r := &SandboxReconciler{
		Client:        c,
		Scheme:        s,
		Recorder:      testutil.NewFakeEventsRecorder(32),
		ClassResolver: classpkg.NewResolver(c),
		Runtimes:      reg,
		RuntimeCfg:    cfg,
	}
	return r, c
}

// newSandboxForRS builds a minimal Sandbox with the given class name.
func newSandboxForRS(className string) *setecv1alpha1.Sandbox {
	return &setecv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sb", Namespace: "default"},
		Spec: setecv1alpha1.SandboxSpec{
			Image:            "img:v1",
			Command:          []string{"sh"},
			SandboxClassName: className,
			Resources: setecv1alpha1.Resources{
				VCPU:   1,
				Memory: resource.MustParse("128Mi"),
			},
		},
	}
}

// newNodeWithLabels builds a Node object with the given labels.
func newNodeWithLabels(name string, labels map[string]string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
	}
}

// newSandboxClassForRS builds a SandboxClass with an optional Runtime spec.
func newSandboxClassForRS(name, backend string, fallback []string) *setecv1alpha1.SandboxClass {
	cls := &setecv1alpha1.SandboxClass{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: setecv1alpha1.SandboxClassSpec{
			VMM: setecv1alpha1.VMMFirecracker,
			MaxResources: &setecv1alpha1.Resources{
				VCPU:   4,
				Memory: resource.MustParse("4Gi"),
			},
		},
	}
	if backend != "" {
		cls.Spec.Runtime = &setecv1alpha1.SandboxClassRuntime{
			Backend:  backend,
			Fallback: fallback,
		}
	}
	return cls
}

// emptyOverheadConfig returns a BackendConfig with the supplied class name
// and an explicitly-empty DefaultOverhead so dispatchers return nil overhead
// (which satisfies envtest pod admission requirements).
func emptyOverheadConfig(runtimeClassName string) runtimepkg.BackendConfig {
	return runtimepkg.BackendConfig{
		Enabled:          true,
		RuntimeClassName: runtimeClassName,
		DefaultOverhead:  corev1.ResourceList{},
	}
}

// ---------------------------------------------------------------------------
// Scenario A: Legacy path — nil Runtimes/RuntimeCfg.
// ---------------------------------------------------------------------------

// TestSelectRuntime_Legacy verifies that when Runtimes is nil, selectRuntime
// synthesizes a Selection for the kata-fc backend using the class
// RuntimeClassName (or empty string when the class also has none).
func TestSelectRuntime_Legacy(t *testing.T) {
	g := NewWithT(t)

	cls := newSandboxClassForRS("legacy-class", "", nil)
	sb := newSandboxForRS(cls.Name)
	r, _ := newRSReconciler(t, nil, nil, cls, sb)

	sel, err := r.selectRuntime(context.Background(), sb, cls)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(sel).NotTo(BeNil())
	// Legacy path always returns kata-fc backend.
	g.Expect(sel.Backend).To(Equal(runtimepkg.BackendKataFC))
	g.Expect(sel.Dispatcher).NotTo(BeNil())
	g.Expect(sel.FellBack).To(BeFalse())
}

// TestSelectRuntime_Legacy_WithClassRCName verifies that the legacy path
// propagates the class's RuntimeClassName into the synthesized dispatcher.
func TestSelectRuntime_Legacy_WithClassRCName(t *testing.T) {
	g := NewWithT(t)

	cls := &setecv1alpha1.SandboxClass{
		ObjectMeta: metav1.ObjectMeta{Name: "typed-class"},
		Spec: setecv1alpha1.SandboxClassSpec{
			VMM:              setecv1alpha1.VMMFirecracker,
			RuntimeClassName: "my-kata",
			MaxResources:     &setecv1alpha1.Resources{VCPU: 2, Memory: resource.MustParse("1Gi")},
		},
	}
	sb := newSandboxForRS(cls.Name)
	r, _ := newRSReconciler(t, nil, nil, cls, sb)

	sel, err := r.selectRuntime(context.Background(), sb, cls)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(sel.Dispatcher.RuntimeClassName()).To(Equal("my-kata"))
}

// ---------------------------------------------------------------------------
// Scenario B: Fallback — class wants kata-qemu, node only has gvisor label.
// ---------------------------------------------------------------------------

// TestSelectRuntime_Fallback verifies that when the primary backend (kata-qemu)
// has no capable node but the fallback (gvisor) does, Select returns a
// Selection with Backend=gvisor and FellBack=true, and the reconciler
// writes status.runtime.chosen via Status().Patch.
func TestSelectRuntime_Fallback(t *testing.T) {
	g := NewWithT(t)

	cfg := &runtimepkg.RuntimeConfig{
		Runtimes: map[string]runtimepkg.BackendConfig{
			runtimepkg.BackendKataQEMU: emptyOverheadConfig("kata-qemu"),
			runtimepkg.BackendGVisor:   emptyOverheadConfig("gvisor"),
		},
		Defaults: runtimepkg.DefaultsConfig{
			Runtime: runtimepkg.RuntimeDefaults{Backend: runtimepkg.BackendKataQEMU},
		},
	}
	reg := runtimepkg.NewRegistry()
	reg.Register(runtimepkg.NewKataQEMUDispatcher(cfg.Runtimes[runtimepkg.BackendKataQEMU]))
	reg.Register(runtimepkg.NewGVisorDispatcher(cfg.Runtimes[runtimepkg.BackendGVisor]))

	// Only a gvisor-capable node; no kata-qemu node.
	gvisorNode := newNodeWithLabels("gvisor-node", map[string]string{
		"setec.zeroroot.ai/runtime.gvisor": "true",
	})

	cls := newSandboxClassForRS("fallback-class", runtimepkg.BackendKataQEMU, []string{runtimepkg.BackendGVisor})
	sb := newSandboxForRS(cls.Name)

	r, c := newRSReconciler(t, reg, cfg, cls, sb, gvisorNode)

	sel, err := r.selectRuntime(context.Background(), sb, cls)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(sel).NotTo(BeNil())

	// Should have fallen back from kata-qemu to gvisor.
	g.Expect(sel.Backend).To(Equal(runtimepkg.BackendGVisor))
	g.Expect(sel.FellBack).To(BeTrue())
	g.Expect(sel.FromBackend).To(Equal(runtimepkg.BackendKataQEMU))
	g.Expect(sel.Dispatcher.RuntimeClassName()).To(Equal("gvisor"))

	// selectRuntime should have written status.runtime.chosen via Status().Patch.
	var updated setecv1alpha1.Sandbox
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(sb), &updated)).To(Succeed())
	g.Expect(updated.Status.Runtime).NotTo(BeNil())
	g.Expect(updated.Status.Runtime.Chosen).To(Equal(runtimepkg.BackendGVisor))
}

// ---------------------------------------------------------------------------
// Scenario C: Exhaustion — no capable nodes → Sandbox goes to Failed.
// ---------------------------------------------------------------------------

// TestSelectRuntime_NoCapableNodeIsProvisional verifies that when no capable
// node exists for the requested backend (runc) and there is no fallback,
// selectRuntime returns a usable Selection marked Provisional rather than an
// error, and holds the Sandbox in Pending with Reason=AwaitingCapableNode.
//
// A usable Selection, not an error, because the caller must go on to create
// the Pod. setec#230 made this Pending instead of Failed, which let the
// Sandbox survive the wait; it did not end the wait, because the reconcile
// still returned before createPod and an autoscaler only ever provisions in
// response to an unschedulable Pod (setec#300).
func TestSelectRuntime_NoCapableNodeIsProvisional(t *testing.T) {
	g := NewWithT(t)

	cfg := &runtimepkg.RuntimeConfig{
		Runtimes: map[string]runtimepkg.BackendConfig{
			runtimepkg.BackendRunc: emptyOverheadConfig("runc"),
		},
		Defaults: runtimepkg.DefaultsConfig{
			Runtime: runtimepkg.RuntimeDefaults{Backend: runtimepkg.BackendRunc},
		},
	}
	reg := runtimepkg.NewRegistry()
	reg.Register(runtimepkg.NewRuncDispatcher(cfg.Runtimes[runtimepkg.BackendRunc]))

	// No node carries setec.zeroroot.ai/runtime.runc=true.
	unrelatedNode := newNodeWithLabels("kata-node", map[string]string{
		"setec.zeroroot.ai/runtime.kata-fc": "true",
	})

	cls := newSandboxClassForRS("exhaust-class", runtimepkg.BackendRunc, nil)
	sb := newSandboxForRS(cls.Name)

	r, c := newRSReconciler(t, reg, cfg, cls, sb, unrelatedNode)

	sel, err := r.selectRuntime(context.Background(), sb, cls)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(sel).ToNot(BeNil())
	g.Expect(sel.Provisional).To(BeTrue())
	g.Expect(sel.Backend).To(Equal(runtimepkg.BackendRunc))
	// The Selection must be complete enough to build a Pod from — returning
	// one that the caller cannot use would defeat the purpose.
	g.Expect(sel.Dispatcher).ToNot(BeNil())

	// Sandbox waits in Pending with AwaitingCapableNode — never terminal.
	var updated setecv1alpha1.Sandbox
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(sb), &updated)).To(Succeed())
	g.Expect(updated.Status.Phase).To(Equal(setecv1alpha1.SandboxPhasePending))
	g.Expect(updated.Status.Reason).To(Equal(eventReasonAwaitingCapableNode))
}

// TestSelectRuntime_UnregisteredBackendIsTerminal is the other half of the
// distinction. No candidate in the chain has a registered Dispatcher, so
// there is no Pod spec to build and no node that could ever change that.
// Only this case returns ErrNoEligibleRuntime, and only this case stops
// before Pod creation.
func TestSelectRuntime_UnregisteredBackendIsTerminal(t *testing.T) {
	g := NewWithT(t)

	cfg := &runtimepkg.RuntimeConfig{
		Runtimes: map[string]runtimepkg.BackendConfig{
			runtimepkg.BackendRunc: emptyOverheadConfig("runc"),
		},
		Defaults: runtimepkg.DefaultsConfig{
			Runtime: runtimepkg.RuntimeDefaults{Backend: runtimepkg.BackendRunc},
		},
	}
	// Registry deliberately left without a gvisor dispatcher.
	reg := runtimepkg.NewRegistry()
	reg.Register(runtimepkg.NewRuncDispatcher(cfg.Runtimes[runtimepkg.BackendRunc]))

	// A node advertising gvisor changes nothing: the operator has no
	// dispatcher for it, so the capability is unusable.
	gvisorNode := newNodeWithLabels("gvisor-node", map[string]string{
		"setec.zeroroot.ai/runtime.gvisor": "true",
	})

	cls := newSandboxClassForRS("unwired-class", runtimepkg.BackendGVisor, nil)
	sb := newSandboxForRS(cls.Name)

	r, c := newRSReconciler(t, reg, cfg, cls, sb, gvisorNode)

	sel, err := r.selectRuntime(context.Background(), sb, cls)
	g.Expect(sel).To(BeNil())
	g.Expect(err).To(HaveOccurred())
	g.Expect(errors.Is(err, runtimepkg.ErrNoEligibleRuntime)).To(BeTrue(),
		"expected ErrNoEligibleRuntime, got: %v", err)

	var updated setecv1alpha1.Sandbox
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(sb), &updated)).To(Succeed())
	g.Expect(updated.Status.Phase).To(Equal(setecv1alpha1.SandboxPhasePending))
	g.Expect(updated.Status.Reason).To(Equal(eventReasonRuntimeNotEnabled))
}

// TestSelectRuntime_ProvisionalClearsWhenANodeAppears is the scale-from-zero
// case end to end: the Sandbox placed provisionally stops being provisional
// once a capable node joins, with no user action and no new Sandbox.
func TestSelectRuntime_ProvisionalClearsWhenANodeAppears(t *testing.T) {
	g := NewWithT(t)

	cfg := &runtimepkg.RuntimeConfig{
		Runtimes: map[string]runtimepkg.BackendConfig{
			runtimepkg.BackendRunc: emptyOverheadConfig("runc"),
		},
		Defaults: runtimepkg.DefaultsConfig{
			Runtime: runtimepkg.RuntimeDefaults{Backend: runtimepkg.BackendRunc},
		},
	}
	reg := runtimepkg.NewRegistry()
	reg.Register(runtimepkg.NewRuncDispatcher(cfg.Runtimes[runtimepkg.BackendRunc]))

	cls := newSandboxClassForRS("scale-from-zero-class", runtimepkg.BackendRunc, nil)
	sb := newSandboxForRS(cls.Name)

	// Start with an empty pool: no node carries any capability label.
	r, c := newRSReconciler(t, reg, cfg, cls, sb)

	sel, err := r.selectRuntime(context.Background(), sb, cls)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(sel).ToNot(BeNil())
	g.Expect(sel.Provisional).To(BeTrue())

	var pending setecv1alpha1.Sandbox
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(sb), &pending)).To(Succeed())
	g.Expect(pending.Status.Phase).To(Equal(setecv1alpha1.SandboxPhasePending))
	g.Expect(pending.Status.Reason).To(Equal(eventReasonAwaitingCapableNode))

	// The autoscaler provisions a capable node — which is what the Pod
	// created off the provisional Selection asked it to do.
	capable := newNodeWithLabels("runc-node", map[string]string{
		"setec.zeroroot.ai/runtime.runc": "true",
	})
	g.Expect(c.Create(context.Background(), capable)).To(Succeed())

	sel, err = r.selectRuntime(context.Background(), sb, cls)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(sel).ToNot(BeNil())
	g.Expect(sel.Backend).To(Equal(runtimepkg.BackendRunc))
	g.Expect(sel.Provisional).To(BeFalse())
}

// ---------------------------------------------------------------------------
// Scenario D: Local defaulting — class with nil Runtime uses config default.
// ---------------------------------------------------------------------------

// TestSelectRuntime_NilRuntimeDefaultsToConfig verifies that when a
// SandboxClass has no Runtime struct, selectRuntime applies the cluster default
// from RuntimeCfg.Defaults.Runtime.Backend and resolves correctly.
func TestSelectRuntime_NilRuntimeDefaultsToConfig(t *testing.T) {
	g := NewWithT(t)

	cfg := &runtimepkg.RuntimeConfig{
		Runtimes: map[string]runtimepkg.BackendConfig{
			runtimepkg.BackendKataFC: emptyOverheadConfig("kata-fc"),
		},
		Defaults: runtimepkg.DefaultsConfig{
			Runtime: runtimepkg.RuntimeDefaults{Backend: runtimepkg.BackendKataFC},
		},
	}
	reg := runtimepkg.NewRegistry()
	reg.Register(runtimepkg.NewKataFCDispatcher(cfg.Runtimes[runtimepkg.BackendKataFC]))

	kataNode := newNodeWithLabels("kata-node", map[string]string{
		"setec.zeroroot.ai/runtime.kata-fc": "true",
	})

	// cls has no Runtime field.
	cls := newSandboxClassForRS("no-runtime-class", "", nil)
	sb := newSandboxForRS(cls.Name)

	r, _ := newRSReconciler(t, reg, cfg, cls, sb, kataNode)

	sel, err := r.selectRuntime(context.Background(), sb, cls)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(sel.Backend).To(Equal(runtimepkg.BackendKataFC))
	g.Expect(sel.FellBack).To(BeFalse())
}

// ---------------------------------------------------------------------------
// Scenario E: scale-from-zero — the Pod is created with NO capable node.
// ---------------------------------------------------------------------------

// TestHandleMissingPod_CreatesPodWithNoCapableNode is the behavioural centre
// of setec#300. With an empty node pool the reconciler must still create the
// Pod, because an unschedulable Pod is the only thing a cluster autoscaler
// acts on. Before this fix handleMissingPod returned on ErrNoEligibleRuntime
// and no Pod was ever written, so Karpenter had nothing to react to and the
// pool stayed at zero forever.
//
// The assertions also enumerate the Pod's HARD scheduling constraints, which
// is what decides whether an autoscaler can satisfy it. Karpenter denies a
// pod whose required affinity names a custom label key its NodePool does not
// declare ("label %q does not have known values",
// kubernetes-sigs/karpenter pkg/scheduling/requirements.go Compatible), and
// skips a NodePool whose taint the pod does not tolerate. So the set below is
// exactly the set a sandbox-host NodePool template has to declare:
//
//   - setec.zeroroot.ai/runtime.<backend>=true — from the Dispatcher's
//     required node affinity (and, in a real cluster, injected again as a
//     nodeSelector by RuntimeClass admission).
//   - kubernetes.io/os, kubernetes.io/arch — well-known, always allowed.
//   - the SandboxClass's own nodeSelector and tolerations — how an operator
//     steers Sandboxes onto a dedicated, tainted pool.
func TestHandleMissingPod_CreatesPodWithNoCapableNode(t *testing.T) {
	g := NewWithT(t)

	cfg := &runtimepkg.RuntimeConfig{
		Runtimes: map[string]runtimepkg.BackendConfig{
			runtimepkg.BackendRunc: emptyOverheadConfig("runc"),
		},
		Defaults: runtimepkg.DefaultsConfig{
			Runtime: runtimepkg.RuntimeDefaults{Backend: runtimepkg.BackendRunc},
		},
	}
	reg := runtimepkg.NewRegistry()
	reg.Register(runtimepkg.NewRuncDispatcher(cfg.Runtimes[runtimepkg.BackendRunc]))

	hostToleration := corev1.Toleration{
		Key:      "setec.zeroroot.ai/sandbox-host",
		Operator: corev1.TolerationOpEqual,
		Value:    "true",
		Effect:   corev1.TaintEffectNoSchedule,
	}
	cls := newSandboxClassForRS("sfz-class", runtimepkg.BackendRunc, nil)
	cls.Spec.NodeSelector = map[string]string{"setec.zeroroot.ai/sandbox-host": "true"}
	cls.Spec.Tolerations = []corev1.Toleration{hostToleration}

	sb := newSandboxForRS(cls.Name)

	// Deliberately NO Node objects: the pool is at zero, which is the
	// starting state this test exists for.
	r, c := newRSReconciler(t, reg, cfg, cls, sb)

	_, err := r.handleMissingPod(context.Background(), logr.Discard(), sb, cls, "")
	g.Expect(err).ToNot(HaveOccurred())

	// The Pod exists. This is the assertion the old code could not pass.
	var pods corev1.PodList
	g.Expect(c.List(context.Background(), &pods, client.InNamespace(sb.Namespace))).To(Succeed())
	g.Expect(pods.Items).To(HaveLen(1), "expected the reconciler to create a Pod with no capable node present")
	pod := pods.Items[0]

	// Class-declared pool steering survived onto the Pod. Without the
	// toleration Karpenter never even considers a tainted NodePool.
	g.Expect(pod.Spec.NodeSelector).To(HaveKeyWithValue("setec.zeroroot.ai/sandbox-host", "true"))
	g.Expect(pod.Spec.Tolerations).To(ContainElement(hostToleration))

	// The backend capability requirement is present and required (not
	// preferred): a node pool that means to host this Sandbox must declare
	// this exact key, with this exact value.
	g.Expect(pod.Spec.Affinity).ToNot(BeNil())
	g.Expect(pod.Spec.Affinity.NodeAffinity).ToNot(BeNil())
	required := pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution
	g.Expect(required).ToNot(BeNil())
	g.Expect(required.NodeSelectorTerms).ToNot(BeEmpty())

	keys := map[string][]string{}
	for _, term := range required.NodeSelectorTerms {
		for _, expr := range term.MatchExpressions {
			g.Expect(expr.Operator).To(Equal(corev1.NodeSelectorOpIn),
				"a non-In operator changes what a NodePool must declare; revisit the Karpenter note above")
			keys[expr.Key] = expr.Values
		}
	}
	g.Expect(keys).To(HaveKeyWithValue("setec.zeroroot.ai/runtime.runc", []string{"true"}))
	g.Expect(keys).To(HaveKeyWithValue("kubernetes.io/os", []string{"linux"}))
	g.Expect(keys).To(HaveKeyWithValue("kubernetes.io/arch", []string{"amd64"}))

	// And the Sandbox says why it is waiting.
	var updated setecv1alpha1.Sandbox
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(sb), &updated)).To(Succeed())
	g.Expect(updated.Status.Phase).To(Equal(setecv1alpha1.SandboxPhasePending))
}
