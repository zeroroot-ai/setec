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

// Package podspec contains the pure translator that turns a Sandbox custom
// resource into the corev1.Pod the controller will create. The translator is
// deliberately side-effect free so that every mapping rule can be verified via
// table-driven unit tests without a running Kubernetes API server.
package podspec

import (
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
	runtimepkg "github.com/zeroroot-ai/setec/internal/runtime"
)

const (
	// PodNameSuffix is appended to the Sandbox name to derive the Pod name
	// (e.g. Sandbox "foo" → Pod "foo-vm").
	PodNameSuffix = "-vm"

	// SandboxLabelKey is the label applied to the owned Pod whose value is
	// the owning Sandbox's name. Callers (e.g. the controller) use this label
	// for owner-ref indexing and to filter events.
	SandboxLabelKey = "setec.zeroroot.ai/sandbox"

	// ContainerName is the name of the single workload container inside the
	// Pod. Kept as a constant so tests and the status reconciler can agree.
	ContainerName = "workload"

	// sandboxKind is the literal kind used in the generated OwnerReference.
	// The v1alpha1 types intentionally do not register a String()-like
	// helper, so we centralize the literal here.
	sandboxKind = "Sandbox"
)

// Errors returned by Build for structural problems the OpenAPI schema cannot
// express (e.g. a caller hand-constructs a Sandbox in Go and skips the API
// server's validation entirely).
var (
	// ErrNilSandbox is returned when Build is invoked with a nil Sandbox.
	ErrNilSandbox = errors.New("podspec: sandbox is nil")

	// ErrMissingName is returned when Sandbox.metadata.name is empty.
	ErrMissingName = errors.New("podspec: sandbox.metadata.name is required")

	// ErrMissingImage is returned when Sandbox.spec.image is empty.
	ErrMissingImage = errors.New("podspec: sandbox.spec.image is required")

	// ErrMissingCommand is returned when Sandbox.spec.command is empty.
	ErrMissingCommand = errors.New("podspec: sandbox.spec.command is required and must have at least one entry")

	// ErrInvalidVCPU is returned when Sandbox.spec.resources.vcpu is less
	// than 1. The CRD validation caps the upper bound; we only double-check
	// the structural floor here.
	ErrInvalidVCPU = errors.New("podspec: sandbox.spec.resources.vcpu must be >= 1")

	// ErrInvalidMemory is returned when Sandbox.spec.resources.memory is
	// zero or negative.
	ErrInvalidMemory = errors.New("podspec: sandbox.spec.resources.memory must be > 0")

	// ErrMissingRuntimeClass is returned when Build is invoked with an empty
	// runtimeClassName. A Sandbox Pod without a runtime class would fall
	// through to the default container runtime, defeating the whole point
	// of Setec.
	ErrMissingRuntimeClass = errors.New("podspec: runtimeClassName is required")
)

// BuildOptions carries optional build-time knobs that are additive to
// the Phase 1 Build signature. Nil / zero-valued fields preserve
// Phase 1/2 behaviour.
type BuildOptions struct {
	// NodeName, when non-empty, is written into Pod.Spec.NodeName so
	// the scheduler pins the Pod to a specific node. Used by the
	// snapshot-restore flow which must land on the node holding the
	// snapshot state files.
	NodeName string

	// RuntimeSelection, when non-nil, overrides the runtimeClassName
	// argument and additionally injects NodeAffinity, Overhead, and any
	// dispatcher-specific pod mutations.  Applied as the last step in
	// BuildWithOptions so dispatchers see the fully-assembled pod.
	RuntimeSelection *runtimepkg.Selection
}

// Build transforms a Sandbox custom resource into the corev1.Pod the
// controller must create. The function is pure: it performs no I/O, makes no
// Kubernetes API calls, and does not read or mutate any global state.
//
// runtimeClassName is passed as an argument rather than hard-coded so that a
// cluster operator can rename the RuntimeClass (e.g. "kata-fc" → "kata-qemu")
// without a code change.
//
// The returned Pod has:
//   - metadata.name = "<sandbox-name>-vm"
//   - metadata.namespace mirrors the Sandbox namespace
//   - metadata.labels includes setec.zeroroot.ai/sandbox=<sandbox-name>
//   - metadata.ownerReferences contains a single controller-owning reference
//     back to the Sandbox with BlockOwnerDeletion=true
//   - spec.runtimeClassName = runtimeClassName
//   - spec.restartPolicy = Never (Sandboxes are single-shot)
//   - spec.containers has exactly one entry named "workload" whose image,
//     command, env, and resources mirror the Sandbox spec
//
// Build returns a wrapped error if the Sandbox is structurally invalid in
// ways the OpenAPI schema cannot express. Callers should propagate the error;
// the controller records it as an Event and requeues.
//
// Build is preserved with its Phase 1 signature for back-compat.
// Phase 3 callers that need node pinning go through BuildWithOptions.
func Build(sb *setecv1alpha1.Sandbox, runtimeClassName string) (*corev1.Pod, error) {
	return BuildWithOptions(sb, runtimeClassName, BuildOptions{})
}

// BuildWithOptions is the extended Phase 3 entry point. Build is a
// thin wrapper that passes the zero-value options, so existing
// Phase 1/2 callers are unaffected.
//
// When opts.RuntimeSelection is set it is applied LAST so the dispatcher's
// MutatePod sees the fully-constructed pod. The runtimeClassName argument is
// used as the initial runtime class; RuntimeSelection.Dispatcher.RuntimeClassName()
// overrides it when non-empty.
func BuildWithOptions(sb *setecv1alpha1.Sandbox, runtimeClassName string, opts BuildOptions) (*corev1.Pod, error) {
	// When a RuntimeSelection is provided and its Dispatcher returns a non-empty
	// RuntimeClassName, that value takes precedence over the runtimeClassName arg.
	effectiveRCName := runtimeClassName
	if opts.RuntimeSelection != nil {
		if rcn := opts.RuntimeSelection.Dispatcher.RuntimeClassName(); rcn != "" {
			effectiveRCName = rcn
		}
	}

	if err := validate(sb, effectiveRCName); err != nil {
		return nil, err
	}

	podName := sb.Name + PodNameSuffix

	labels := map[string]string{
		SandboxLabelKey: sb.Name,
	}

	ctrl, bod := true, true
	ownerRef := metav1.OwnerReference{
		APIVersion:         setecv1alpha1.GroupVersion.String(),
		Kind:               sandboxKind,
		Name:               sb.Name,
		UID:                sb.UID,
		Controller:         &ctrl,
		BlockOwnerDeletion: &bod,
	}

	container := corev1.Container{
		Name:      ContainerName,
		Image:     sb.Spec.Image,
		Command:   append([]string(nil), sb.Spec.Command...),
		Env:       append([]corev1.EnvVar(nil), sb.Spec.Env...),
		Resources: buildResourceRequirements(sb.Spec.Resources),
	}

	rcName := effectiveRCName
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            podName,
			Namespace:       sb.Namespace,
			Labels:          labels,
			OwnerReferences: []metav1.OwnerReference{ownerRef},
		},
		Spec: corev1.PodSpec{
			RuntimeClassName: &rcName,
			RestartPolicy:    corev1.RestartPolicyNever,
			Containers:       []corev1.Container{container},
		},
	}

	if opts.NodeName != "" {
		pod.Spec.NodeName = opts.NodeName
	}

	// Apply the RuntimeSelection LAST so the dispatcher's MutatePod sees the
	// fully-assembled pod (per task-12 requirement: option applied last in pipeline).
	if opts.RuntimeSelection != nil {
		if err := applyRuntimeSelection(pod, opts.RuntimeSelection); err != nil {
			return nil, fmt.Errorf("podspec: apply runtime selection: %w", err)
		}
	}

	return pod, nil
}

// applyRuntimeSelection applies the dispatcher-derived fields to pod:
//  1. RuntimeClassName is already set above (before MutatePod needs it).
//  2. NodeAffinity terms from the dispatcher are MERGED into any existing
//     required affinity terms — not replaced — so caller-provided affinity
//     is preserved.
//  3. Overhead from the dispatcher is set when non-empty.
//  4. MutatePod is called last so dispatchers can see (and depend on) any
//     of the above values.
//
// The params map is extracted from sandbox.Spec if SandboxClass runtime.Params
// were set; since the builder does not have access to the SandboxClass we
// pass nil here — callers that need param propagation should invoke
// sel.Dispatcher.MutatePod directly after BuildWithOptions.
func applyRuntimeSelection(pod *corev1.Pod, sel *runtimepkg.Selection) error {
	// Merge NodeAffinity required terms.
	dispatcherAffinity := sel.Dispatcher.NodeAffinity()
	if dispatcherAffinity != nil &&
		dispatcherAffinity.RequiredDuringSchedulingIgnoredDuringExecution != nil {
		if pod.Spec.Affinity == nil {
			pod.Spec.Affinity = &corev1.Affinity{}
		}
		if pod.Spec.Affinity.NodeAffinity == nil {
			pod.Spec.Affinity.NodeAffinity = &corev1.NodeAffinity{}
		}
		if pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
			pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution = &corev1.NodeSelector{}
		}
		// Merge: append dispatcher terms to any existing terms (do not replace).
		pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms = append(
			pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms,
			dispatcherAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms...,
		)
	}

	// Set Overhead when the dispatcher provides it.  Note: Kubernetes validates
	// that Pod.Spec.Overhead exactly matches the RuntimeClass's overhead field.
	// The RuntimeClass must therefore already declare the same overhead values.
	// In clusters where the RuntimeClass does not define overhead (e.g. dev
	// envtest environments), callers should pass an empty BackendConfig.DefaultOverhead
	// so the dispatcher returns nil here.
	if overhead := sel.Dispatcher.Overhead(); len(overhead) > 0 {
		pod.Spec.Overhead = overhead.DeepCopy()
	}

	// MutatePod is called last. The params map is nil here because the builder
	// does not carry SandboxClass.Spec.Runtime.Params; callers needing param
	// propagation should set them via a post-build MutatePod call or by passing
	// them through a future BuildOptions extension.
	if err := sel.Dispatcher.MutatePod(pod, nil); err != nil {
		return fmt.Errorf("dispatcher %q MutatePod: %w", sel.Backend, err)
	}

	return nil
}

// WithRuntimeSelection returns a BuildOptions with RuntimeSelection set to sel.
// It is a convenience constructor for callers that use the functional-option
// style; callers that already have a BuildOptions struct may set the field directly.
func WithRuntimeSelection(sel *runtimepkg.Selection) BuildOptions {
	return BuildOptions{RuntimeSelection: sel}
}

// validate performs the structural checks that the OpenAPI schema cannot
// express. Returning a structured error keeps Build side-effect free.
func validate(sb *setecv1alpha1.Sandbox, runtimeClassName string) error {
	if sb == nil {
		return ErrNilSandbox
	}
	if sb.Name == "" {
		return ErrMissingName
	}
	if runtimeClassName == "" {
		return ErrMissingRuntimeClass
	}
	if sb.Spec.Image == "" {
		return ErrMissingImage
	}
	if len(sb.Spec.Command) == 0 {
		return ErrMissingCommand
	}
	if sb.Spec.Resources.VCPU < 1 {
		return fmt.Errorf("%w: got %d", ErrInvalidVCPU, sb.Spec.Resources.VCPU)
	}
	if sb.Spec.Resources.Memory.Sign() <= 0 {
		return fmt.Errorf("%w: got %q", ErrInvalidMemory, sb.Spec.Resources.Memory.String())
	}
	return nil
}

// buildResourceRequirements maps the Sandbox resources block to a
// corev1.ResourceRequirements value with identical requests and limits so the
// kubelet guarantees the microVM gets exactly what was asked for.
func buildResourceRequirements(r setecv1alpha1.Resources) corev1.ResourceRequirements {
	cpu := *resource.NewQuantity(int64(r.VCPU), resource.DecimalSI)
	mem := r.Memory.DeepCopy()

	rl := corev1.ResourceList{
		corev1.ResourceCPU:    cpu,
		corev1.ResourceMemory: mem,
	}

	return corev1.ResourceRequirements{
		Requests: rl.DeepCopy(),
		Limits:   rl.DeepCopy(),
	}
}
