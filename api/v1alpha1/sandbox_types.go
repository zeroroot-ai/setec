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

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// NOTE: json tags are required. Fields without a json tag will not be
// serialized.  DeepCopy methods are generated via controller-gen; do not
// edit zz_generated.deepcopy.go by hand.

// NetworkMode selects the egress posture applied to a Sandbox microVM.
//
// Every mode produces a NetworkPolicy. There is no mode that means "emit no
// policy": the operator always writes one, and the reconciler refuses to
// start the microVM until it is accepted by the API server.
// +kubebuilder:validation:Enum=external-only;egress-allow-list;none
type NetworkMode string

const (
	// NetworkModeExternalOnly permits egress to public networks while
	// denying every address range the cluster operator has reserved
	// (spec.reservedCIDRs on the operator; see the --reserved-cidrs
	// flag). It is the posture for workloads that must reach arbitrary
	// external endpoints — scanners, crawlers, package fetchers — but
	// have no business talking to cluster-internal services.
	//
	// The generated policy allows 0.0.0.0/0 on all ports with the
	// reserved ranges subtracted via ipBlock.except, denies all
	// ingress, and permits DNS only to the operator-configured
	// resolvers.
	NetworkModeExternalOnly NetworkMode = "external-only"

	// NetworkModeEgressAllowList restricts egress to the destinations
	// declared in Network.Allow. Each entry is rendered as its own
	// port-scoped egress rule, and each rule carries the same reserved
	// range subtraction as external-only unless the SandboxClass
	// explicitly exempts a range via spec.egressExemptCIDRs.
	NetworkModeEgressAllowList NetworkMode = "egress-allow-list"

	// NetworkModeNone denies all network access. The controller
	// generates a NetworkPolicy with empty ingress and empty egress,
	// isolating the Sandbox from every endpoint including DNS. This is
	// the posture a Sandbox resolves to when neither it nor its
	// SandboxClass states an intent.
	NetworkModeNone NetworkMode = "none"
)

// SandboxPhase is the high-level lifecycle state of a Sandbox.
// +kubebuilder:validation:Enum=Pending;Running;Completed;Failed;Paused;Snapshotting;Restoring
type SandboxPhase string

const (
	// SandboxPhasePending indicates the microVM has not yet started.
	SandboxPhasePending SandboxPhase = "Pending"
	// SandboxPhaseRunning indicates the microVM is executing the workload.
	SandboxPhaseRunning SandboxPhase = "Running"
	// SandboxPhaseCompleted indicates the workload exited with code 0.
	SandboxPhaseCompleted SandboxPhase = "Completed"
	// SandboxPhaseFailed indicates the workload did not complete successfully.
	SandboxPhaseFailed SandboxPhase = "Failed"
	// SandboxPhasePaused indicates the microVM is paused (Phase 3).
	// Pause is either user-requested (desiredState=Paused) or
	// transient during a snapshot operation.
	SandboxPhasePaused SandboxPhase = "Paused"
	// SandboxPhaseSnapshotting indicates the snapshot.Coordinator is
	// currently persisting the microVM state. Transient.
	SandboxPhaseSnapshotting SandboxPhase = "Snapshotting"
	// SandboxPhaseRestoring indicates the node-agent is loading a
	// Firecracker snapshot before the microVM resumes. Transient.
	SandboxPhaseRestoring SandboxPhase = "Restoring"
)

// SandboxDesiredState expresses the user's intent with respect to
// pause/resume. Only Running and Paused are meaningful in Phase 3.
// +kubebuilder:validation:Enum=Running;Paused
type SandboxDesiredState string

const (
	// SandboxDesiredStateRunning keeps (or resumes) the microVM
	// executing. This is the Phase 1/2 default.
	SandboxDesiredStateRunning SandboxDesiredState = "Running"
	// SandboxDesiredStatePaused requests that the microVM transition
	// to a paused state. CPU/memory consumption drops to near-zero;
	// state is preserved in memory (not on disk) until Resume.
	SandboxDesiredStatePaused SandboxDesiredState = "Paused"
)

// SandboxSnapshotAfterCreate enumerates the states a Sandbox may
// transition to after a successful snapshot operation.
// +kubebuilder:validation:Enum=Running;Paused;Terminated
type SandboxSnapshotAfterCreate string

const (
	// SandboxSnapshotAfterCreateRunning resumes the microVM after the
	// snapshot is persisted. This is the default.
	SandboxSnapshotAfterCreateRunning SandboxSnapshotAfterCreate = "Running"
	// SandboxSnapshotAfterCreatePaused leaves the microVM paused after
	// the snapshot is persisted.
	SandboxSnapshotAfterCreatePaused SandboxSnapshotAfterCreate = "Paused"
	// SandboxSnapshotAfterCreateTerminated deletes the Sandbox after
	// the snapshot is persisted (e.g. for one-shot "capture state then
	// tear down" workflows).
	SandboxSnapshotAfterCreateTerminated SandboxSnapshotAfterCreate = "Terminated"
)

// SandboxSnapshotSpec configures snapshot-creation behavior on a
// Sandbox. All fields are optional; when Create is false the block is
// effectively a no-op.
type SandboxSnapshotSpec struct {
	// Create, when true, requests that the operator take a snapshot of
	// the Sandbox once it reaches Running. Snapshot creation is a
	// one-shot operation — once the Snapshot CR is Ready, further
	// reconciles of the Sandbox do not re-snapshot.
	// +optional
	Create bool `json:"create,omitempty"`

	// Name is the name given to the resulting Snapshot CR. Must be a
	// valid DNS-1123 label and unique within the Sandbox's namespace.
	// +optional
	Name string `json:"name,omitempty"`

	// AfterCreate controls what happens to the Sandbox after the
	// snapshot is successfully persisted. Defaults to Running.
	// +kubebuilder:default=Running
	// +optional
	AfterCreate SandboxSnapshotAfterCreate `json:"afterCreate,omitempty"`

	// TTL is forwarded to the resulting Snapshot CR's spec.ttl. When
	// set the snapshot is auto-deleted after TTL elapses if no Sandbox
	// references it.
	// +optional
	TTL *metav1.Duration `json:"ttl,omitempty"`
}

// SandboxSnapshotRef references a Snapshot CR in the same namespace
// that the Sandbox should restore from.
type SandboxSnapshotRef struct {
	// Name is the name of the Snapshot in the Sandbox's namespace.
	// Cross-namespace references are rejected by the admission
	// webhook.
	// +kubebuilder:validation:MinLength=1
	// +required
	Name string `json:"name"`
}

// Resources declares the CPU and memory budget allocated to the Sandbox
// microVM. Both fields are required.
type Resources struct {
	// VCPU is the number of virtual CPUs to assign to the microVM.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=32
	// +required
	VCPU int32 `json:"vcpu"`

	// Memory is the amount of RAM allocated to the microVM, expressed as a
	// Kubernetes resource.Quantity (e.g. "2Gi", "512Mi").
	// +required
	Memory resource.Quantity `json:"memory"`
}

// NetworkAllow describes a single permitted egress destination when
// NetworkMode is egress-allow-list.
type NetworkAllow struct {
	// Host is the DNS name or IP address permitted as an egress target.
	//
	// Kubernetes NetworkPolicy cannot match on a DNS name, so the
	// operator resolves it and writes the answer into the rule as
	// ipBlock peers, re-resolving periodically so the rule follows the
	// name. The declared value is also kept on an annotation as the
	// human-readable record.
	//
	// A name the operator cannot resolve is DROPPED from the policy, not
	// widened: the entry contributes no rule and the drop is recorded on
	// the setec.zeroroot.ai/unresolved-allow annotation. Set CIDR to pin
	// the destination and skip resolution entirely.
	// +kubebuilder:validation:MinLength=1
	// +required
	Host string `json:"host"`

	// Port is the destination TCP port permitted for this host.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +required
	Port int32 `json:"port"`

	// CIDR optionally pins this entry to an address block, replacing
	// resolution of Host for this rule.
	//
	// An explicit pin is a statement about the destination's address
	// range that DNS cannot improve on, so it wins outright and no
	// lookup happens. Use it for a destination whose addresses are known
	// and stable, or one whose name does not resolve from inside the
	// cluster. When empty, Host is resolved instead.
	// +optional
	CIDR string `json:"cidr,omitempty"`
}

// Network describes the egress policy applied to the Sandbox. When omitted,
// the Sandbox inherits its SandboxClass's spec.defaultNetworkMode; when the
// class states no default either, the effective mode is NetworkModeNone.
// There is no configuration that yields an unpoliced Sandbox.
type Network struct {
	// Mode selects the egress posture for the Sandbox.
	// +kubebuilder:default=none
	// +required
	Mode NetworkMode `json:"mode"`

	// Allow is the set of permitted egress destinations. Only meaningful
	// when Mode=egress-allow-list.
	// +optional
	Allow []NetworkAllow `json:"allow,omitempty"`
}

// Lifecycle carries optional runtime constraints on the Sandbox.
type Lifecycle struct {
	// Timeout bounds the maximum wall-clock runtime of the Sandbox. Once
	// the timeout elapses the controller terminates the underlying Pod and
	// marks the Sandbox Failed with reason "Timeout". Accepts any Go-style
	// duration string recognized by metav1.Duration (e.g. "30m", "8h").
	// +optional
	Timeout *metav1.Duration `json:"timeout,omitempty"`
}

// SandboxSpec defines the desired state of a Sandbox.
type SandboxSpec struct {
	// SandboxClassName is the name of the cluster-scoped SandboxClass this
	// Sandbox is subject to. When empty the operator resolves the class
	// flagged default:true (if any); when set the operator enforces the
	// referenced class's constraints via the validating admission webhook.
	// Optional for Phase 1 back-compat.
	// +optional
	SandboxClassName string `json:"sandboxClassName,omitempty"`

	// Image is the OCI reference the microVM will run. Pull policy follows
	// the kubelet defaults; Setec does not interpret the registry.
	// +kubebuilder:validation:MinLength=1
	// +required
	Image string `json:"image"`

	// Command is the entrypoint executed inside the microVM. All arguments
	// are passed verbatim; no shell interpretation occurs.
	// +kubebuilder:validation:MinItems=1
	// +required
	Command []string `json:"command"`

	// Env is an optional set of environment variables made available to
	// the workload. Values follow the standard Kubernetes EnvVar schema.
	// +optional
	Env []corev1.EnvVar `json:"env,omitempty"`

	// Resources declares the CPU and memory budget for the microVM.
	// +required
	Resources Resources `json:"resources"`

	// Network describes the egress policy. Optional; when omitted the
	// SandboxClass default applies, and when the class states no default
	// the Sandbox resolves to NetworkMode=none.
	// +optional
	Network *Network `json:"network,omitempty"`

	// Lifecycle carries optional runtime constraints such as a timeout.
	// +optional
	Lifecycle *Lifecycle `json:"lifecycle,omitempty"`

	// DesiredState expresses the user's intent with respect to
	// pause/resume. Phase 3 feature; defaults to Running which
	// preserves Phase 1/2 semantics.
	// +kubebuilder:default=Running
	// +optional
	DesiredState SandboxDesiredState `json:"desiredState,omitempty"`

	// Snapshot optionally requests that the operator take a snapshot
	// of the Sandbox once it reaches Running. Phase 3 feature. See
	// SandboxSnapshotSpec for field semantics.
	// +optional
	Snapshot *SandboxSnapshotSpec `json:"snapshot,omitempty"`

	// SnapshotRef optionally requests that the Sandbox be restored
	// from a previously-captured Snapshot rather than cold-booted.
	// When set the operator pins the Pod to the node holding the
	// snapshot state files and invokes a restore via the node-agent
	// gRPC service. Phase 3 feature.
	// +optional
	SnapshotRef *SandboxSnapshotRef `json:"snapshotRef,omitempty"`
}

// SandboxRuntimeStatus records the runtime backend that was actually selected
// for this Sandbox after fallback resolution. Populated by the reconciler
// once a backend is chosen; empty while the Sandbox is still Pending.
type SandboxRuntimeStatus struct {
	// Chosen is the name of the backend selected after evaluating the
	// SandboxClass's primary backend and any fallback chain. One of
	// kata-fc, kata-qemu, gvisor, or runc.
	// +optional
	Chosen string `json:"chosen,omitempty"`
}

// SandboxStatus reflects the observed state of a Sandbox.
type SandboxStatus struct {
	// Phase is the high-level lifecycle state derived from the underlying
	// Pod. Once a Sandbox enters a terminal phase (Completed or Failed)
	// the controller will not roll it back to Pending or Running.
	// +optional
	Phase SandboxPhase `json:"phase,omitempty"`

	// Reason is a short, machine-readable explanation for the current
	// phase. Populated on Failed (e.g. "Timeout", "ImagePullFailure",
	// "RuntimeUnavailable", "ContainerExitedNonZero").
	// +optional
	Reason string `json:"reason,omitempty"`

	// ExitCode is the exit status of the workload container once the
	// Sandbox reaches a terminal phase. Nil while still running.
	// +optional
	ExitCode *int32 `json:"exitCode,omitempty"`

	// PodName is the name of the backing Pod created by the controller.
	// Defaults to "<sandbox-name>-vm".
	// +optional
	PodName string `json:"podName,omitempty"`

	// StartedAt is the time the underlying Pod first transitioned to
	// Running.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`

	// LastTransitionTime is the timestamp of the most recent phase change.
	// +optional
	LastTransitionTime *metav1.Time `json:"lastTransitionTime,omitempty"`

	// PausedAt is the time the Sandbox entered a Paused phase. Used by
	// the reconciler to enforce SandboxClass.spec.maxPauseDuration and
	// by the metrics subsystem to record pause latency. Cleared when
	// the Sandbox resumes.
	// +optional
	PausedAt *metav1.Time `json:"pausedAt,omitempty"`

	// Runtime records the isolation backend selected for this Sandbox.
	// Populated by the reconciler after backend selection; nil while Pending.
	// +optional
	Runtime *SandboxRuntimeStatus `json:"runtime,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,shortName=sbx
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Runtime",type=string,JSONPath=`.status.runtime.chosen`
// +kubebuilder:printcolumn:name="Class",type=string,JSONPath=`.spec.sandboxClassName`
// +kubebuilder:printcolumn:name="Image",type=string,JSONPath=`.spec.image`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:printcolumn:name="Exit-Code",type=integer,JSONPath=`.status.exitCode`,priority=1

// Sandbox is the Schema for the sandboxes API. Each Sandbox represents a
// single ephemeral microVM execution.
type Sandbox struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is the standard Kubernetes object metadata.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// spec defines the desired state of the Sandbox.
	// +required
	Spec SandboxSpec `json:"spec"`

	// status reflects the observed state of the Sandbox.
	// +optional
	Status SandboxStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// SandboxList is a list of Sandbox resources.
type SandboxList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Sandbox `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Sandbox{}, &SandboxList{})
}
