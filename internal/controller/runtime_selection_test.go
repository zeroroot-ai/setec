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
//  3. Exhaustion: class wants runc, no capable node → Sandbox patched to
//     Failed with Reason=NoEligibleNode, ErrNoEligibleRuntime returned.
package controller

import (
	"context"
	"errors"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	setecv1alpha1 "github.com/zero-day-ai/setec/api/v1alpha1"
	classpkg "github.com/zero-day-ai/setec/internal/class"
	"github.com/zero-day-ai/setec/internal/controller/testutil"
	runtimepkg "github.com/zero-day-ai/setec/internal/runtime"
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
		"setec.zero-day.ai/runtime.gvisor": "true",
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

// TestSelectRuntime_Exhaustion verifies that when no capable node exists for
// the requested backend (runc) and there is no fallback, selectRuntime
// transitions the Sandbox to Failed with Reason=NoEligibleNode and returns
// ErrNoEligibleRuntime.
func TestSelectRuntime_Exhaustion(t *testing.T) {
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

	// No node carries setec.zero-day.ai/runtime.runc=true.
	unrelatedNode := newNodeWithLabels("kata-node", map[string]string{
		"setec.zero-day.ai/runtime.kata-fc": "true",
	})

	cls := newSandboxClassForRS("exhaust-class", runtimepkg.BackendRunc, nil)
	sb := newSandboxForRS(cls.Name)

	r, c := newRSReconciler(t, reg, cfg, cls, sb, unrelatedNode)

	sel, err := r.selectRuntime(context.Background(), sb, cls)
	g.Expect(sel).To(BeNil())
	g.Expect(err).To(HaveOccurred())
	g.Expect(errors.Is(err, runtimepkg.ErrNoEligibleRuntime)).To(BeTrue(),
		"expected ErrNoEligibleRuntime, got: %v", err)

	// Sandbox should have been transitioned to Failed with NoEligibleNode.
	var updated setecv1alpha1.Sandbox
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(sb), &updated)).To(Succeed())
	g.Expect(updated.Status.Phase).To(Equal(setecv1alpha1.SandboxPhaseFailed))
	g.Expect(updated.Status.Reason).To(Equal("NoEligibleNode"))
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
		"setec.zero-day.ai/runtime.kata-fc": "true",
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
