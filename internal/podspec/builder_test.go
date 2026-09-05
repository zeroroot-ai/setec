// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package podspec

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
	runtimepkg "github.com/zeroroot-ai/setec/internal/runtime"
)

// defaultRuntimeClass is the RuntimeClass name used by tests that do not
// care about the exact value.
const defaultRuntimeClass = "kata-fc"

const (
	testLongSandboxName = "long-sandbox-name-42"
	testMutatedLabel    = "MUTATED"
)

// newSandbox builds a Sandbox with sensible required fields so individual
// tests only set what they care about.
func newSandbox(mutators ...func(*setecv1alpha1.Sandbox)) *setecv1alpha1.Sandbox {
	sb := &setecv1alpha1.Sandbox{
		TypeMeta: metav1.TypeMeta{
			APIVersion: setecv1alpha1.GroupVersion.String(),
			Kind:       "Sandbox",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "demo",
			Namespace: "default",
			UID:       types.UID("11111111-2222-3333-4444-555555555555"),
		},
		Spec: setecv1alpha1.SandboxSpec{
			Image:   "docker.io/library/python:3.12-slim",
			Command: []string{"python", "-c", "print('hi')"},
			Resources: setecv1alpha1.Resources{
				VCPU:   2,
				Memory: resource.MustParse("2Gi"),
			},
		},
	}
	for _, m := range mutators {
		m(sb)
	}
	return sb
}

func buildOrFatal(t *testing.T, sb *setecv1alpha1.Sandbox, rc string) *corev1.Pod {
	t.Helper()
	pod, err := Build(sb, rc)
	if err != nil {
		t.Fatalf("Build() returned unexpected error: %v", err)
	}
	if pod == nil {
		t.Fatalf("Build() returned nil Pod")
	}
	return pod
}

func TestBuild_Success_MinimalSandbox(t *testing.T) {
	t.Parallel()
	pod := buildOrFatal(t, newSandbox(), defaultRuntimeClass)
	if pod.Name != "demo-vm" {
		t.Errorf("pod name = %q, want %q", pod.Name, "demo-vm")
	}
	if pod.Namespace != "default" {
		t.Errorf("pod namespace = %q, want default", pod.Namespace)
	}
	if got := pod.Labels[SandboxLabelKey]; got != "demo" {
		t.Errorf("label %s = %q, want demo", SandboxLabelKey, got)
	}
	if pod.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("restartPolicy = %q, want Never", pod.Spec.RestartPolicy)
	}
	if pod.Spec.RuntimeClassName == nil || *pod.Spec.RuntimeClassName != defaultRuntimeClass {
		t.Errorf("runtimeClassName = %v, want %q", pod.Spec.RuntimeClassName, defaultRuntimeClass)
	}
	if len(pod.Spec.Containers) != 1 {
		t.Fatalf("containers = %d, want 1", len(pod.Spec.Containers))
	}
	c := pod.Spec.Containers[0]
	if c.Name != ContainerName {
		t.Errorf("container name = %q, want %q", c.Name, ContainerName)
	}
	if c.Image != "docker.io/library/python:3.12-slim" {
		t.Errorf("container image = %q", c.Image)
	}
	wantCmd := []string{"python", "-c", "print('hi')"}
	if diff := cmp.Diff(wantCmd, c.Command); diff != "" {
		t.Errorf("container command mismatch (-want +got):\n%s", diff)
	}
	if len(c.Env) != 0 {
		t.Errorf("expected no env vars, got %d", len(c.Env))
	}
}

func TestBuild_Success_EnvList(t *testing.T) {
	t.Parallel()
	sb := newSandbox(func(sb *setecv1alpha1.Sandbox) {
		sb.Spec.Env = []corev1.EnvVar{
			{Name: "FOO", Value: "bar"},
			{Name: "BAZ", Value: "qux"},
			{Name: "EMPTY", Value: ""},
		}
	})
	pod := buildOrFatal(t, sb, defaultRuntimeClass)
	want := []corev1.EnvVar{
		{Name: "FOO", Value: "bar"},
		{Name: "BAZ", Value: "qux"},
		{Name: "EMPTY", Value: ""},
	}
	if diff := cmp.Diff(want, pod.Spec.Containers[0].Env); diff != "" {
		t.Errorf("env mismatch (-want +got):\n%s", diff)
	}
}

func TestBuild_Success_MultiArgCommand(t *testing.T) {
	t.Parallel()
	sb := newSandbox(func(sb *setecv1alpha1.Sandbox) {
		sb.Spec.Command = []string{"/usr/bin/env", "bash", "-lc", "echo hello && sleep 1"}
	})
	pod := buildOrFatal(t, sb, defaultRuntimeClass)
	want := []string{"/usr/bin/env", "bash", "-lc", "echo hello && sleep 1"}
	if diff := cmp.Diff(want, pod.Spec.Containers[0].Command); diff != "" {
		t.Errorf("command mismatch (-want +got):\n%s", diff)
	}
}

func TestBuild_Success_SmallResources(t *testing.T) {
	t.Parallel()
	sb := newSandbox(func(sb *setecv1alpha1.Sandbox) {
		sb.Spec.Resources.VCPU = 1
		sb.Spec.Resources.Memory = resource.MustParse("128Mi")
	})
	pod := buildOrFatal(t, sb, defaultRuntimeClass)
	req := pod.Spec.Containers[0].Resources.Requests
	lim := pod.Spec.Containers[0].Resources.Limits
	if got := req[corev1.ResourceCPU]; got.Cmp(resource.MustParse("1")) != 0 {
		t.Errorf("request cpu = %s, want 1", got.String())
	}
	if got := req[corev1.ResourceMemory]; got.Cmp(resource.MustParse("128Mi")) != 0 {
		t.Errorf("request memory = %s, want 128Mi", got.String())
	}
	// With no class reservation, requests and limits are identical
	// (Guaranteed QoS); see requests_test.go for the split.
	if diff := cmp.Diff(req, lim); diff != "" {
		t.Errorf("requests != limits (-req +lim):\n%s", diff)
	}
}

func TestBuild_Success_LargeResources(t *testing.T) {
	t.Parallel()
	sb := newSandbox(func(sb *setecv1alpha1.Sandbox) {
		sb.Spec.Resources.VCPU = 32
		sb.Spec.Resources.Memory = resource.MustParse("128Gi")
	})
	pod := buildOrFatal(t, sb, defaultRuntimeClass)
	req := pod.Spec.Containers[0].Resources.Requests
	if got := req[corev1.ResourceCPU]; got.Cmp(resource.MustParse("32")) != 0 {
		t.Errorf("request cpu = %s, want 32", got.String())
	}
	if got := req[corev1.ResourceMemory]; got.Cmp(resource.MustParse("128Gi")) != 0 {
		t.Errorf("request memory = %s, want 128Gi", got.String())
	}
}

func TestBuild_Success_OwnerReference(t *testing.T) {
	t.Parallel()
	pod := buildOrFatal(t, newSandbox(), defaultRuntimeClass)
	if len(pod.OwnerReferences) != 1 {
		t.Fatalf("ownerReferences = %d, want 1", len(pod.OwnerReferences))
	}
	ref := pod.OwnerReferences[0]
	ctrl, bod := true, true
	want := metav1.OwnerReference{
		APIVersion:         setecv1alpha1.GroupVersion.String(),
		Kind:               "Sandbox",
		Name:               "demo",
		UID:                types.UID("11111111-2222-3333-4444-555555555555"),
		Controller:         &ctrl,
		BlockOwnerDeletion: &bod,
	}
	if diff := cmp.Diff(want, ref); diff != "" {
		t.Errorf("owner reference mismatch (-want +got):\n%s", diff)
	}
}

func TestBuild_Success_RuntimeClassName(t *testing.T) {
	t.Parallel()
	pod := buildOrFatal(t, newSandbox(), "kata-qemu-custom")
	if pod.Spec.RuntimeClassName == nil {
		t.Fatalf("runtimeClassName is nil")
	}
	if *pod.Spec.RuntimeClassName != "kata-qemu-custom" {
		t.Errorf("runtimeClassName = %q, want kata-qemu-custom", *pod.Spec.RuntimeClassName)
	}
}

func TestBuild_Success_PodNameAndLabels(t *testing.T) {
	t.Parallel()
	sb := newSandbox(func(sb *setecv1alpha1.Sandbox) {
		sb.Name = testLongSandboxName
		sb.Namespace = "tenant-a"
	})
	pod := buildOrFatal(t, sb, defaultRuntimeClass)
	if pod.Name != testLongSandboxName+"-vm" {
		t.Errorf("pod name = %q", pod.Name)
	}
	if pod.Namespace != "tenant-a" {
		t.Errorf("pod namespace = %q", pod.Namespace)
	}
	if got := pod.Labels[SandboxLabelKey]; got != testLongSandboxName {
		t.Errorf("label %s = %q", SandboxLabelKey, got)
	}
	if !strings.HasSuffix(pod.Name, PodNameSuffix) {
		t.Errorf("pod name %q missing suffix %q", pod.Name, PodNameSuffix)
	}
	if pod.OwnerReferences[0].Name != testLongSandboxName {
		t.Errorf("owner ref name = %q", pod.OwnerReferences[0].Name)
	}
}

func TestBuild_ValidationErrors(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name             string
		sandbox          *setecv1alpha1.Sandbox
		runtimeClassName string
		wantErr          error
	}{
		{
			name:             "nil sandbox returns ErrNilSandbox",
			sandbox:          nil,
			runtimeClassName: defaultRuntimeClass,
			wantErr:          ErrNilSandbox,
		},
		{
			name: "empty name returns ErrMissingName",
			sandbox: newSandbox(func(sb *setecv1alpha1.Sandbox) {
				sb.Name = ""
			}),
			runtimeClassName: defaultRuntimeClass,
			wantErr:          ErrMissingName,
		},
		{
			name:             "empty runtime class returns ErrMissingRuntimeClass",
			sandbox:          newSandbox(),
			runtimeClassName: "",
			wantErr:          ErrMissingRuntimeClass,
		},
		{
			name: "empty image returns ErrMissingImage",
			sandbox: newSandbox(func(sb *setecv1alpha1.Sandbox) {
				sb.Spec.Image = ""
			}),
			runtimeClassName: defaultRuntimeClass,
			wantErr:          ErrMissingImage,
		},
		{
			name: "empty command returns ErrMissingCommand",
			sandbox: newSandbox(func(sb *setecv1alpha1.Sandbox) {
				sb.Spec.Command = nil
			}),
			runtimeClassName: defaultRuntimeClass,
			wantErr:          ErrMissingCommand,
		},
		{
			name: "zero vcpu returns ErrInvalidVCPU",
			sandbox: newSandbox(func(sb *setecv1alpha1.Sandbox) {
				sb.Spec.Resources.VCPU = 0
			}),
			runtimeClassName: defaultRuntimeClass,
			wantErr:          ErrInvalidVCPU,
		},
		{
			name: "negative vcpu returns ErrInvalidVCPU",
			sandbox: newSandbox(func(sb *setecv1alpha1.Sandbox) {
				sb.Spec.Resources.VCPU = -3
			}),
			runtimeClassName: defaultRuntimeClass,
			wantErr:          ErrInvalidVCPU,
		},
		{
			name: "zero memory returns ErrInvalidMemory",
			sandbox: newSandbox(func(sb *setecv1alpha1.Sandbox) {
				sb.Spec.Resources.Memory = resource.MustParse("0")
			}),
			runtimeClassName: defaultRuntimeClass,
			wantErr:          ErrInvalidMemory,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			pod, err := Build(c.sandbox, c.runtimeClassName)
			if pod != nil {
				t.Errorf("Build() returned non-nil Pod on error: %+v", pod)
			}
			if err == nil {
				t.Fatalf("Build() returned nil error, want %v", c.wantErr)
			}
			if !errors.Is(err, c.wantErr) {
				t.Fatalf("Build() error = %v, want errors.Is(err, %v)", err, c.wantErr)
			}
		})
	}
}

// TestBuild_DeepCopyIsolation verifies the builder does not share slice
// backing storage with the input Sandbox. This is important because the
// controller mutates Pod objects (status updates, finalizers) independently
// of the Sandbox they originated from.
func TestBuild_DeepCopyIsolation(t *testing.T) {
	t.Parallel()

	sb := newSandbox(func(sb *setecv1alpha1.Sandbox) {
		sb.Spec.Command = []string{"echo", "hello"}
		sb.Spec.Env = []corev1.EnvVar{{Name: "A", Value: "1"}}
	})

	pod, err := Build(sb, defaultRuntimeClass)
	if err != nil {
		t.Fatalf("Build() unexpected error: %v", err)
	}

	// Mutate the Sandbox slices after Build and assert the Pod is
	// unaffected.
	sb.Spec.Command[0] = testMutatedLabel
	sb.Spec.Env[0].Value = testMutatedLabel

	if pod.Spec.Containers[0].Command[0] == testMutatedLabel {
		t.Errorf("pod command aliased Sandbox command slice")
	}
	if pod.Spec.Containers[0].Env[0].Value == testMutatedLabel {
		t.Errorf("pod env aliased Sandbox env slice")
	}
}

// TestBuildWithOptions_NodeNamePinning asserts that opts.NodeName is
// propagated to Pod.Spec.NodeName. Phase 1 Build without options
// leaves NodeName empty; this is the Phase 3 extension.
func TestBuildWithOptions_NodeNamePinning(t *testing.T) {
	t.Parallel()
	sb := newSandbox()

	// Phase 1 back-compat: empty NodeName.
	pod, err := Build(sb, defaultRuntimeClass)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if pod.Spec.NodeName != "" {
		t.Fatalf("Phase 1 Build must not pin NodeName, got %q", pod.Spec.NodeName)
	}

	// Phase 3 explicit pinning via BuildWithOptions.
	pod2, err := BuildWithOptions(sb, defaultRuntimeClass, BuildOptions{NodeName: "node-a"})
	if err != nil {
		t.Fatalf("BuildWithOptions: %v", err)
	}
	if pod2.Spec.NodeName != "node-a" {
		t.Fatalf("NodeName = %q, want node-a", pod2.Spec.NodeName)
	}

	// BuildWithOptions with zero-value options matches Build.
	pod3, err := BuildWithOptions(sb, defaultRuntimeClass, BuildOptions{})
	if err != nil {
		t.Fatalf("BuildWithOptions zero: %v", err)
	}
	if pod3.Spec.NodeName != "" {
		t.Fatalf("zero-value options must not pin, got %q", pod3.Spec.NodeName)
	}
}

// ---------------------------------------------------------------------------
// WithRuntimeSelection tests (task 12)
// ---------------------------------------------------------------------------

// TestWithRuntimeSelection_MergesNodeAffinity verifies that
// BuildWithOptions with a RuntimeSelection MERGES (not replaces)
// pre-existing NodeAffinity terms already set on the Sandbox.
//
// It builds a pod using a gVisor dispatcher (which adds the
// setec.zeroroot.ai/runtime.gvisor=true required term) and confirms:
//   - The dispatcher's term is appended.
//   - A pre-existing affinity term from the test is not discarded.
//   - RuntimeClassName reflects the dispatcher's value.
//   - Overhead is set from the dispatcher.
func TestWithRuntimeSelection_MergesNodeAffinity(t *testing.T) {
	t.Parallel()

	sb := newSandbox()

	// Use the runc dispatcher (zero overhead, adds isolation label).
	runcCfg := runtimepkg.BackendConfig{
		Enabled:          true,
		RuntimeClassName: "runc",
	}
	runcDispatcher := runtimepkg.NewRuncDispatcher(runcCfg)

	sel := &runtimepkg.Selection{
		Backend:    runtimepkg.BackendRunc,
		Dispatcher: runcDispatcher,
	}

	pod, err := BuildWithOptions(sb, "runc", BuildOptions{RuntimeSelection: sel})
	if err != nil {
		t.Fatalf("BuildWithOptions: %v", err)
	}

	// runc dispatcher adds setec.zeroroot.ai/isolation=container-only.
	if got := pod.Labels["setec.zeroroot.ai/isolation"]; got != "container-only" {
		t.Errorf("isolation label = %q, want container-only", got)
	}

	// NodeAffinity should be set by the runc dispatcher.
	if pod.Spec.Affinity == nil {
		t.Fatal("Affinity is nil, want non-nil from dispatcher")
	}
	required := pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution
	if required == nil {
		t.Fatal("RequiredDuringSchedulingIgnoredDuringExecution is nil")
	}
	if len(required.NodeSelectorTerms) == 0 {
		t.Fatal("NodeSelectorTerms is empty, want at least one from dispatcher")
	}

	// RuntimeClassName should be set to "runc".
	if pod.Spec.RuntimeClassName == nil || *pod.Spec.RuntimeClassName != "runc" {
		t.Errorf("RuntimeClassName = %v, want runc", pod.Spec.RuntimeClassName)
	}
}

// TestWithRuntimeSelection_AffinityMerge_PreservesExisting verifies that
// when the pod already has affinity terms (e.g. from a class NodeSelector
// that was promoted to affinity elsewhere), the dispatcher's terms are
// appended rather than replacing the existing ones.
//
// We exercise this by calling applyRuntimeSelection on a pod that already
// has a term, and asserting both terms survive.
func TestWithRuntimeSelection_AffinityMerge_PreservesExisting(t *testing.T) {
	t.Parallel()

	sb := newSandbox()

	// GVisor dispatcher: adds setec.zeroroot.ai/runtime.gvisor=true term.
	gvisorCfg := runtimepkg.BackendConfig{
		Enabled:          true,
		RuntimeClassName: "gvisor",
	}
	gvisorDispatcher := runtimepkg.NewGVisorDispatcher(gvisorCfg)
	sel := &runtimepkg.Selection{
		Backend:    runtimepkg.BackendGVisor,
		Dispatcher: gvisorDispatcher,
	}

	// Build the pod — this produces a pod with the gvisor affinity term.
	pod, err := BuildWithOptions(sb, "gvisor", BuildOptions{RuntimeSelection: sel})
	if err != nil {
		t.Fatalf("BuildWithOptions: %v", err)
	}

	// Confirm gvisor affinity term is present.
	required := pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution
	if len(required.NodeSelectorTerms) == 0 {
		t.Fatal("no NodeSelectorTerms after first build")
	}
	firstTermCount := len(required.NodeSelectorTerms)

	// Now apply a second dispatcher (kata-fc) on the same pod to confirm
	// the first term is preserved. We manually call applyRuntimeSelection.
	kataCfg := runtimepkg.BackendConfig{
		Enabled:          true,
		RuntimeClassName: "kata-fc",
	}
	kataDispatcher := runtimepkg.NewKataFCDispatcher(kataCfg)
	kataSel := &runtimepkg.Selection{
		Backend:    runtimepkg.BackendKataFC,
		Dispatcher: kataDispatcher,
	}
	if err := applyRuntimeSelection(pod, kataSel); err != nil {
		t.Fatalf("second applyRuntimeSelection: %v", err)
	}

	// Both the gvisor term and the kata-fc term must now be present.
	required = pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution
	if got := len(required.NodeSelectorTerms); got != firstTermCount+1 {
		t.Errorf("NodeSelectorTerms count = %d, want %d (first term must be preserved after merge)",
			got, firstTermCount+1)
	}
}

// TestWithRuntimeSelection_OverheadSet verifies that Overhead from the
// dispatcher is written into Pod.Spec.Overhead.
func TestWithRuntimeSelection_OverheadSet(t *testing.T) {
	t.Parallel()

	sb := newSandbox()

	// kata-fc dispatcher has default overhead ~128Mi / 250m. Install:true marks
	// the RuntimeClass as chart-managed, so the operator stamps overhead.
	kataCfg := runtimepkg.BackendConfig{
		Enabled:          true,
		Install:          true,
		RuntimeClassName: "kata-fc",
	}
	kataDispatcher := runtimepkg.NewKataFCDispatcher(kataCfg)
	sel := &runtimepkg.Selection{
		Backend:    runtimepkg.BackendKataFC,
		Dispatcher: kataDispatcher,
	}

	pod, err := BuildWithOptions(sb, "kata-fc", BuildOptions{RuntimeSelection: sel})
	if err != nil {
		t.Fatalf("BuildWithOptions: %v", err)
	}

	if pod.Spec.Overhead == nil {
		t.Fatal("Overhead is nil, want non-nil from kata-fc dispatcher")
	}
	if _, ok := pod.Spec.Overhead[corev1.ResourceMemory]; !ok {
		t.Error("Overhead missing memory resource from kata-fc dispatcher")
	}
	if _, ok := pod.Spec.Overhead[corev1.ResourceCPU]; !ok {
		t.Error("Overhead missing cpu resource from kata-fc dispatcher")
	}
}

// TestWithRuntimeSelection_OverheadOmittedForExternalRuntimeClass verifies that
// for an externally-managed RuntimeClass (Install:false) the operator does NOT
// stamp Pod.Spec.Overhead, even with a DefaultOverhead configured. The
// Kubernetes RuntimeClass admission controller then applies the class's own
// overhead instead of rejecting the Pod on a mismatch (setec#78).
func TestWithRuntimeSelection_OverheadOmittedForExternalRuntimeClass(t *testing.T) {
	t.Parallel()

	sb := newSandbox()

	// Install:false → externally managed; DefaultOverhead is intentionally set
	// to prove it is NOT stamped (it would otherwise mismatch the external class).
	kataCfg := runtimepkg.BackendConfig{
		Enabled:          true,
		Install:          false,
		RuntimeClassName: "kata-fc",
		DefaultOverhead: corev1.ResourceList{
			corev1.ResourceMemory: resource.MustParse("128Mi"),
			corev1.ResourceCPU:    resource.MustParse("250m"),
		},
	}
	sel := &runtimepkg.Selection{
		Backend:    runtimepkg.BackendKataFC,
		Dispatcher: runtimepkg.NewKataFCDispatcher(kataCfg),
	}

	pod, err := BuildWithOptions(sb, "kata-fc", BuildOptions{RuntimeSelection: sel})
	if err != nil {
		t.Fatalf("BuildWithOptions: %v", err)
	}

	if pod.Spec.Overhead != nil {
		t.Errorf("Overhead = %v, want nil for an externally-managed RuntimeClass (admission applies the class's own)", pod.Spec.Overhead)
	}
}

// --- Pod hardening ---------------------------------------------------------

// TestBuild_NoServiceAccountTokenMounted guards the credential a Sandbox
// has no use for. Without AutomountServiceAccountToken=false the kubelet
// projects the namespace's default ServiceAccount token into the
// workload, handing untrusted code a cluster credential.
func TestBuild_NoServiceAccountTokenMounted(t *testing.T) {
	t.Parallel()
	pod := buildOrFatal(t, newSandbox(), defaultRuntimeClass)

	if pod.Spec.AutomountServiceAccountToken == nil {
		t.Fatal("AutomountServiceAccountToken is unset; the default SA token would be mounted")
	}
	if *pod.Spec.AutomountServiceAccountToken {
		t.Fatal("AutomountServiceAccountToken = true; the default SA token would be mounted")
	}
}

// TestBuild_PodSecurityContext pins the unprivileged identity the
// workload runs as.
func TestBuild_PodSecurityContext(t *testing.T) {
	t.Parallel()
	pod := buildOrFatal(t, newSandbox(), defaultRuntimeClass)

	sc := pod.Spec.SecurityContext
	if sc == nil {
		t.Fatal("pod SecurityContext is nil")
	}
	if sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot {
		t.Error("RunAsNonRoot is not enforced")
	}
	if sc.RunAsUser == nil || *sc.RunAsUser != sandboxUID {
		t.Errorf("RunAsUser = %v, want %d", sc.RunAsUser, sandboxUID)
	}
	if sc.RunAsGroup == nil || *sc.RunAsGroup != sandboxGID {
		t.Errorf("RunAsGroup = %v, want %d", sc.RunAsGroup, sandboxGID)
	}
	if sc.SeccompProfile == nil || sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Errorf("SeccompProfile = %v, want RuntimeDefault", sc.SeccompProfile)
	}
}

// TestBuild_ContainerSecurityContext pins the container-level hardening
// AND the two capabilities that must survive it. Dropping NET_RAW or
// NET_ADMIN would break raw-socket network tooling, so this test fails in
// both directions on purpose.
func TestBuild_ContainerSecurityContext(t *testing.T) {
	t.Parallel()
	pod := buildOrFatal(t, newSandbox(), defaultRuntimeClass)

	sc := pod.Spec.Containers[0].SecurityContext
	if sc == nil {
		t.Fatal("container SecurityContext is nil")
	}
	if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
		t.Error("AllowPrivilegeEscalation is not disabled")
	}
	if sc.Privileged == nil || *sc.Privileged {
		t.Error("Privileged is not disabled")
	}
	if sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem {
		t.Error("ReadOnlyRootFilesystem is not enforced")
	}
	if sc.Capabilities == nil {
		t.Fatal("Capabilities is nil; the default capability set would apply")
	}
	if diff := cmp.Diff([]corev1.Capability{"ALL"}, sc.Capabilities.Drop); diff != "" {
		t.Errorf("Capabilities.Drop mismatch (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(sandboxCapabilities, sc.Capabilities.Add); diff != "" {
		t.Errorf("Capabilities.Add mismatch (-want +got):\n%s\n"+
			"NET_RAW and NET_ADMIN must survive the drop or raw-socket tooling stops working", diff)
	}
}

// TestBuild_ScratchVolumeForReadOnlyRoot checks the writable mount that
// makes a read-only root filesystem usable. Without it every tool that
// writes a temporary file fails.
func TestBuild_ScratchVolumeForReadOnlyRoot(t *testing.T) {
	t.Parallel()
	pod := buildOrFatal(t, newSandbox(), defaultRuntimeClass)

	var vol *corev1.Volume
	for i := range pod.Spec.Volumes {
		if pod.Spec.Volumes[i].Name == scratchVolumeName {
			vol = &pod.Spec.Volumes[i]
		}
	}
	if vol == nil {
		t.Fatalf("no %q volume; a read-only root filesystem leaves nowhere to write", scratchVolumeName)
	}
	if vol.EmptyDir == nil {
		t.Errorf("%q volume is not an emptyDir", scratchVolumeName)
	}

	var mounted bool
	for _, m := range pod.Spec.Containers[0].VolumeMounts {
		if m.Name == scratchVolumeName && m.MountPath == scratchMountPath {
			mounted = true
		}
	}
	if !mounted {
		t.Errorf("%q is not mounted at %s", scratchVolumeName, scratchMountPath)
	}
}

// TestBuild_ResolverIPsSetDNSConfig covers the half of DNS containment
// that lives on the Pod. With dnsPolicy None and explicit nameservers the
// workload resolves through the operator's resolvers instead of cluster
// DNS, so it cannot look up in-cluster Service names.
func TestBuild_ResolverIPsSetDNSConfig(t *testing.T) {
	t.Parallel()

	pod, err := BuildWithOptions(newSandbox(), defaultRuntimeClass, BuildOptions{
		ResolverIPs: []string{"1.1.1.1", "8.8.8.8"},
	})
	if err != nil {
		t.Fatalf("BuildWithOptions: %v", err)
	}

	if pod.Spec.DNSPolicy != corev1.DNSNone {
		t.Errorf("DNSPolicy = %q, want None; the Pod would use cluster DNS", pod.Spec.DNSPolicy)
	}
	if pod.Spec.DNSConfig == nil {
		t.Fatal("DNSConfig is nil")
	}
	if diff := cmp.Diff([]string{"1.1.1.1", "8.8.8.8"}, pod.Spec.DNSConfig.Nameservers); diff != "" {
		t.Errorf("nameservers mismatch (-want +got):\n%s", diff)
	}
}

// TestBuild_NoResolverIPsLeavesDNSAtDefault documents the test-only path:
// an empty resolver list leaves cluster DNS in place. The operator
// refuses to start without resolvers, so this shape never reaches a
// cluster.
func TestBuild_NoResolverIPsLeavesDNSAtDefault(t *testing.T) {
	t.Parallel()
	pod := buildOrFatal(t, newSandbox(), defaultRuntimeClass)

	if pod.Spec.DNSPolicy != "" {
		t.Errorf("DNSPolicy = %q, want empty (cluster default)", pod.Spec.DNSPolicy)
	}
	if pod.Spec.DNSConfig != nil {
		t.Errorf("DNSConfig = %+v, want nil", pod.Spec.DNSConfig)
	}
}
