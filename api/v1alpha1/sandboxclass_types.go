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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// VMM identifies a virtual machine monitor a SandboxClass may target. The
// enum matches the set of VMMs Kata Containers currently ships support for;
// the operator itself does not embed VMM-specific logic — the value is
// surfaced to administrators as an explicit capability declaration.
// +kubebuilder:validation:Enum=firecracker;qemu;cloud-hypervisor
type VMM string

const (
	// VMMFirecracker selects the Firecracker VMM (Kata runtime kata-fc).
	VMMFirecracker VMM = "firecracker"
	// VMMQEMU selects the QEMU VMM (Kata runtime kata-qemu).
	VMMQEMU VMM = "qemu"
	// VMMCloudHypervisor selects Cloud Hypervisor (Kata runtime kata-clh).
	VMMCloudHypervisor VMM = "cloud-hypervisor"
)

// SandboxClassRuntime selects the isolation backend for Sandboxes in this
// class and optionally declares an ordered fallback chain. When Runtime is
// nil the operator infers a backend from the legacy VMM field; when that is
// also unset the cluster-default backend from Helm values is used.
// +kubebuilder:validation:Optional
type SandboxClassRuntime struct {
	// Backend is the isolation runtime to use for Sandboxes in this class.
	// Must be one of the four supported backends. When unset the operator
	// falls back to the cluster default declared in Helm values.
	// +kubebuilder:validation:Enum=kata-fc;kata-qemu;gvisor;runc
	// +optional
	Backend string `json:"backend,omitempty"`

	// Params is a map of backend-specific tuning parameters forwarded to the
	// RuntimeDispatcher. Keys and semantics vary per backend (e.g.
	// params.vcpus, params.memory for kata-qemu). Unknown keys are ignored
	// by backends that do not understand them.
	// +optional
	Params map[string]string `json:"params,omitempty"`

	// Fallback is an ordered list of backend names to attempt when no node
	// advertises the requested Backend. Each entry must be one of the four
	// supported values. The operator tries each in order; the first backend
	// with a capable node wins. status.runtime.chosen records the final
	// selection. When the whole chain is exhausted the Sandbox stays
	// Pending with reason NoEligibleNode and the operator keeps retrying:
	// on an autoscaled cluster a capable node may not exist yet.
	// +optional
	Fallback []string `json:"fallback,omitempty"`
}

// SandboxClassSpec defines the constraints and defaults a cluster
// administrator publishes for tenant-facing Sandboxes. Tenants reference a
// SandboxClass by name in Sandbox.spec.sandboxClassName (added to
// SandboxSpec in a later task) and the operator enforces that the requested
// Sandbox fits within the class.
type SandboxClassSpec struct {
	// Deprecated: use Runtime.Backend instead.
	// VMM selects the virtual machine monitor targeted by this class.
	//
	// Optional. It was previously required, which made every class that
	// states its isolation the current way — through Runtime.Backend —
	// unadmittable: the API server rejected it with "spec.vmm: Required
	// value", so the chart's own SandboxClasses could not be applied, no
	// class resolved, and every Sandbox fell back to deny-all. A field
	// that is deprecated cannot also be mandatory. When it is empty the
	// operator reads Runtime.Backend; when both are empty the operator's
	// configured default backend applies.
	// +optional
	VMM VMM `json:"vmm,omitempty"`

	// Deprecated: use Runtime.Backend instead.
	// RuntimeClassName optionally overrides the operator-wide default
	// RuntimeClass name (e.g. "kata-fc", "kata-qemu"). When empty the
	// controller falls back to its --runtime-class-name flag.
	// +optional
	RuntimeClassName string `json:"runtimeClassName,omitempty"`

	// Runtime selects the isolation backend and optional fallback chain for
	// Sandboxes in this class. When nil the operator infers the backend from
	// the legacy VMM field for backwards compatibility.
	// +optional
	Runtime *SandboxClassRuntime `json:"runtime,omitempty"`

	// KernelImage is an optional OCI reference to a custom guest kernel
	// image the node agent pre-pulls and hands to Kata. Empty means the
	// Kata-packaged kernel for the selected VMM is used.
	// +optional
	KernelImage string `json:"kernelImage,omitempty"`

	// RootfsImage is an optional OCI reference to a custom guest rootfs
	// image. Empty means the Kata-packaged rootfs is used.
	// +optional
	RootfsImage string `json:"rootfsImage,omitempty"`

	// DefaultResources is the resource budget applied to Sandboxes that do
	// not specify their own. Optional; when nil the Sandbox must declare
	// its own resources explicitly.
	// +optional
	DefaultResources *Resources `json:"defaultResources,omitempty"`

	// MaxResources is the upper bound tenant Sandboxes may request. The
	// validating admission webhook rejects any Sandbox requesting more
	// than these values. Optional; when nil the class imposes no ceiling
	// beyond whatever ResourceQuota the tenant namespace enforces.
	// +optional
	MaxResources *Resources `json:"maxResources,omitempty"`

	// AllowedNetworkModes enumerates the Sandbox.network.mode values
	// tenants may request under this class. Empty list means all modes
	// are allowed (back-compat: Phase 1 behavior).
	// +optional
	AllowedNetworkModes []NetworkMode `json:"allowedNetworkModes,omitempty"`

	// DefaultNetworkMode is the egress posture applied to Sandboxes in
	// this class that do not declare their own spec.network. When unset
	// the effective mode is "none": a Sandbox that says nothing about
	// networking is fully isolated (ADR-0052, setec#66). Classes whose
	// workloads must reach external endpoints set this to
	// "external-only"; classes whose workloads talk to a small declared
	// destination set use "egress-allow-list".
	// +kubebuilder:validation:Enum=external-only;egress-allow-list;none
	// +optional
	DefaultNetworkMode NetworkMode `json:"defaultNetworkMode,omitempty"`

	// EgressExemptCIDRs lists address ranges this class is permitted to
	// reach even though the operator reserved them cluster-wide via
	// --reserved-cidrs. Entries are subtracted from the reserved list
	// before it is rendered into ipBlock.except, so a class may punch a
	// deliberate, audited hole for a specific in-cluster endpoint (for
	// example a platform check-in service a connector must reach).
	// Empty — the expected value for every class that runs tenant
	// workloads — keeps the full reserved list in force.
	// +optional
	EgressExemptCIDRs []string `json:"egressExemptCIDRs,omitempty"`

	// DefaultEgressAllow is the class-level egress allowlist applied
	// when DefaultNetworkMode is "egress-allow-list" and a Sandbox does
	// not declare its own network block. It lets an administrator open a
	// small, audited set of destinations (e.g. a package mirror) for
	// every Sandbox in the class while keeping everything else denied.
	// Ignored unless DefaultNetworkMode is "egress-allow-list".
	// +optional
	DefaultEgressAllow []NetworkAllow `json:"defaultEgressAllow,omitempty"`

	// NodeSelector is injected into every Sandbox Pod produced under this
	// class. It is additive to any Pod-level selectors the controller sets
	// for RuntimeClass affinity.
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// Tolerations is injected into every Sandbox Pod produced under this
	// class, additive to any tolerations the controller itself sets. This
	// lets an administrator target a tainted NodePool (e.g. a Karpenter
	// pool reserved for sandbox-host nodes via a NoSchedule taint) by
	// declaring the matching toleration once on the class rather than
	// requiring every tenant Sandbox to know about the taint.
	// +optional
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`

	// Default marks this SandboxClass as the cluster-wide default. Only
	// one SandboxClass may carry this flag set to true; multiple defaults
	// produce a startup warning and cause the resolver to reject
	// defaulting until the ambiguity is resolved.
	// +optional
	Default bool `json:"default,omitempty"`

	// PreWarmPoolSize declares how many paused microVMs the node-agent
	// maintains per eligible node for this class. Zero disables the
	// pool (Phase 1/2 behavior). When non-zero PreWarmImage MUST be
	// set — the webhook enforces the pairing.
	// +kubebuilder:validation:Minimum=0
	// +optional
	PreWarmPoolSize int32 `json:"preWarmPoolSize,omitempty"`

	// PreWarmImage is the OCI reference baked into pre-warmed pool
	// entries. Sandboxes requesting a different image fall through to
	// the cold-boot path. The format follows the usual OCI reference
	// grammar; validation beyond non-empty is a webhook concern so the
	// CRD schema remains minimal.
	// +optional
	PreWarmImage string `json:"preWarmImage,omitempty"`

	// PreWarmTTL bounds the age of pool entries. Entries older than
	// this are recycled (torn down and reprovisioned) to avoid stale
	// kernel state accumulating in paused VMs. When unset the
	// node-agent defaults to 24h at runtime.
	// +optional
	PreWarmTTL *metav1.Duration `json:"preWarmTTL,omitempty"`

	// MaxPauseDuration bounds how long a Sandbox may remain in
	// phase=Paused — a paused microVM keeps its full memory
	// reservation, and this cap bounds that residency. Beyond it the
	// reconciler transitions the Sandbox to Failed with
	// reason=PauseTimeoutExceeded and deletes its VM Pod.
	//
	// Sessions in a class with spec.sessionCheckpoint enabled are the
	// exception (setec#202, ADR-0006): past the cap they suspend
	// instead — checkpoint retained, microVM released, phase=Suspended
	// with reason=SuspendedPauseTimeout — and resume when
	// spec.desiredState returns to Running. The Suspended phase itself
	// is not bounded by this cap: a suspended session holds no microVM.
	//
	// When unset pauses are unbounded. Must be positive when set (the
	// webhook rejects zero or negative values; the reconciler treats a
	// non-positive value as unbounded).
	// +optional
	MaxPauseDuration *metav1.Duration `json:"maxPauseDuration,omitempty"`

	// SessionIdleTimeout is the idle-eviction threshold for session
	// Sandboxes in this class (ADR-0006). A Running session whose last
	// recorded activity — the setec.zeroroot.ai/last-activity
	// annotation the frontend stamps on Attach and heartbeats while a
	// client stream is open, falling back to status.startedAt and then
	// the creation timestamp — is older than this duration is evicted:
	// the operator marks it Failed with reason=IdleTimeout and deletes
	// its VM Pod. An actively-used session is therefore never
	// idle-reaped, because its activity timestamp keeps moving.
	//
	// Only session Sandboxes are subject to this policy; ephemeral
	// Sandboxes are bounded by spec.lifecycle.timeout instead. Unset,
	// zero, or negative means sessions in this class are never
	// idle-evicted. Set it comfortably above one minute — the
	// frontend's activity heartbeat interval — so an attached client
	// always refreshes the clock in time.
	// +optional
	SessionIdleTimeout *metav1.Duration `json:"sessionIdleTimeout,omitempty"`

	// SessionCheckpoint enables L2 memory checkpoints for session
	// Sandboxes of this class (setec#194, ADR-0006/0007): periodic
	// checkpoints while Running, checkpoint-on-drain when the node is
	// cordoned or the VM Pod is evicted, and suspend-instead-of-evict
	// when the sessionIdleTimeout deadline passes — the idle session
	// checkpoints, releases its microVM, and resumes transparently on
	// the next reattach, on whichever node the scheduler picks. Nil
	// disables checkpoints: sessions then survive VM loss only via
	// their durable workspace, and idle sessions are hard-evicted per
	// sessionIdleTimeout.
	// +optional
	SessionCheckpoint *SessionCheckpointSpec `json:"sessionCheckpoint,omitempty"`
}

// SessionCheckpointSpec tunes the per-class memory-checkpoint policy
// (setec#194). Checkpoints are encrypted with a per-checkpoint DEK
// sealed under the session's cluster-scoped KEK Secret, stored on the
// S3-compatible checkpoint store, and destroyed at session end.
type SessionCheckpointSpec struct {
	// Interval is the cadence of periodic memory checkpoints while
	// the session is Running. Because the durable workspace already
	// provides continuous data safety (ADR-0007), checkpoints serve
	// process continuity only and SHOULD be infrequent — the trade is
	// bandwidth/cost against how much process replay a resume loses.
	// Unset or zero disables periodic checkpoints; checkpoints are
	// then taken only on suspend and drain.
	// +optional
	Interval *metav1.Duration `json:"interval,omitempty"`

	// Backend names the portable StorageBackend checkpoints are
	// written to. Only "s3" (any S3-compatible store — real S3,
	// MinIO, …) is supported. Defaults to "s3".
	// +kubebuilder:validation:Enum=s3
	// +kubebuilder:default=s3
	// +optional
	Backend string `json:"backend,omitempty"`
}

// CheckpointBackend returns the effective checkpoint backend name.
func (s *SessionCheckpointSpec) CheckpointBackend() string {
	if s == nil || s.Backend == "" {
		return "s3"
	}
	return s.Backend
}

// SandboxClassStatus reflects the observed state of a SandboxClass. Phase 2
// does not compute any status fields — the struct exists so future phases
// can record counts, validation summaries, or image-prefetch state without
// breaking the CRD schema.
type SandboxClassStatus struct {
	// ObservedGeneration is the .metadata.generation the operator last
	// reconciled. Optional; left empty in Phase 2.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions surface class-level facts the operator wants loudly
	// visible. Today the only stamped type is
	// UnverifiedRestoresAllowed: True when the class carries the
	// setec.zeroroot.ai/allow-unverified-restores="true" dev-mode
	// annotation AND the cluster-level dev gate label is present, so
	// the ADR-0005 invariant gate may serve unverified restores for
	// this class. Anyone auditing the cluster sees the opt-out on the
	// class itself, not buried in per-sandbox events.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=sbxcls
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="VMM",type=string,JSONPath=`.spec.vmm`
// +kubebuilder:printcolumn:name="Default",type=boolean,JSONPath=`.spec.default`
// +kubebuilder:printcolumn:name="Max-VCPU",type=integer,JSONPath=`.spec.maxResources.vcpu`,priority=1
// +kubebuilder:printcolumn:name="Max-Memory",type=string,JSONPath=`.spec.maxResources.memory`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// SandboxClass is a cluster-scoped, administrator-authored resource that
// publishes a named, pre-approved sandbox configuration. Tenant users
// reference a SandboxClass by name in their Sandbox manifests; the
// operator's validating webhook enforces that the Sandbox fits within the
// class's constraints.
type SandboxClass struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is the standard Kubernetes object metadata. SandboxClass is
	// cluster-scoped so namespace is ignored.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// spec defines the constraints and defaults of the class.
	// +required
	Spec SandboxClassSpec `json:"spec"`

	// status reflects the observed state of the class.
	// +optional
	Status SandboxClassStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// SandboxClassList is a list of SandboxClass resources.
type SandboxClassList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SandboxClass `json:"items"`
}

func init() {
	SchemeBuilder.Register(&SandboxClass{}, &SandboxClassList{})
}
