// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package v1alpha1

import (
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// SandboxClassRuntime names the isolation backend of the Sandboxes of a
// class. setec has one backend: each Sandbox is a Firecracker machine in a
// launcher Pod (docs/design/runtime.md). The field stays, so that a class
// written for a removed backend gets a clear refusal from the webhook and
// the operator rather than a silent change of its isolation.
// +kubebuilder:validation:Optional
type SandboxClassRuntime struct {
	// Backend is "launcher" or empty. The webhook and the operator refuse
	// each other name, and name the reason for a backend that left in
	// setec#198.
	// +optional
	Backend string `json:"backend,omitempty"`
}

// SandboxClassSpec defines the constraints and defaults a cluster
// administrator publishes for tenant-facing Sandboxes. Tenants reference a
// SandboxClass by name in Sandbox.spec.sandboxClassName (added to
// SandboxSpec in a later task) and the operator enforces that the requested
// Sandbox fits within the class.
type SandboxClassSpec struct {
	// Runtime names the isolation backend of the class. Nil means the one
	// backend, the launcher.
	// +optional
	Runtime *SandboxClassRuntime `json:"runtime,omitempty"`

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

	// Requests is the scheduler reservation for every Sandbox Pod in
	// this class, declared separately from the Sandbox's own resources
	// block, which stays the Pod's limits. A Sandbox that sits idle most
	// of the time can then reserve a small slice of a node and still
	// burst to its full budget: an always-on agent member reserves
	// 250m / 768Mi and runs with a 2 vCPU / 4Gi ceiling.
	//
	// Each value is bounded by the Sandbox's limits on the Pod: a
	// reservation above the budget would make the Pod invalid, so the
	// controller writes min(request, limit). Unset, or an unset field,
	// keeps the Phase 1 shape where requests equal limits (Guaranteed
	// QoS).
	// +optional
	Requests *ResourceRequests `json:"requests,omitempty"`

	// AllowedNetworkModes enumerates the Sandbox.network.mode values
	// tenants may request under this class. Empty list means all modes
	// are allowed (back-compat: Phase 1 behavior).
	// +optional
	AllowedNetworkModes []NetworkMode `json:"allowedNetworkModes,omitempty"`

	// DefaultNetworkMode is the egress posture applied to Sandboxes in
	// this class that do not declare their own spec.network. When unset
	// the effective mode is "none": a Sandbox that says nothing about
	// networking is fully isolated (docs/design/threat-model.md, setec#66). Classes whose
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

	// EgressAllowSelectors lists in-cluster workloads this class may
	// reach, selected by namespace and Pod labels (setec#76). Each entry
	// renders as one egress rule whose peer carries the namespaceSelector
	// and podSelector and whose ports are the listed ones, beside the
	// ipBlock rules the mode produces.
	//
	// This is the only way to reach a Service. Kubernetes evaluates
	// egress policy after kube-proxy translates the ClusterIP to a
	// backend Pod address, so an ipBlock for a ClusterIP, whether from
	// egressExemptCIDRs or an allow-list entry, never matches. A
	// selector matches the Pod the packet actually reaches.
	//
	// An entry whose ports include 53 also lets the Sandbox use a
	// configured resolver that sits inside the reserved ranges, such as
	// the kube-dns ClusterIP. Without such an entry a class never has
	// its Pods pointed at cluster DNS, whatever --sandbox-resolvers
	// lists.
	//
	// Every entry widens what every Sandbox in the class reaches. Empty,
	// the expected value for every class that runs tenant workloads,
	// grants nothing. Ignored under mode "none".
	// +optional
	EgressAllowSelectors []EgressAllowSelector `json:"egressAllowSelectors,omitempty"`

	// DefaultEgressAllow is the class-level egress allowlist applied
	// when DefaultNetworkMode is "egress-allow-list" and a Sandbox does
	// not declare its own network block. It lets an administrator open a
	// small, audited set of destinations (e.g. a package mirror) for
	// every Sandbox in the class while keeping everything else denied.
	// Ignored unless DefaultNetworkMode is "egress-allow-list".
	// +optional
	DefaultEgressAllow []NetworkAllow `json:"defaultEgressAllow,omitempty"`

	// NodeSelector is injected into every Sandbox Pod produced under this
	// class. It is additive to the node affinity that the controller sets.
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

	// PreWarmPoolSize is the number of warm pool bases that the operator
	// keeps for this class (setec#103). Zero disables the pool. When
	// non-zero, PreWarmImage with a digest and DefaultResources must be
	// set; the webhook enforces both.
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

	// CPUTemplate names a Firecracker custom CPU template that the
	// launcher image holds. The machine of each launcher Sandbox of the
	// class shows the guest the CPU features of the template, so a
	// snapshot loads on any node of the class. With no template a
	// snapshot loads only on a node of the instance type of its source.
	// A snapshot loads only into a class with the same template.
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:MaxLength=63
	// +optional
	CPUTemplate string `json:"cpuTemplate,omitempty"`

	// MaxPauseDuration bounds how long a Sandbox may remain in
	// phase=Paused — a paused microVM keeps its full memory
	// reservation, and this cap bounds that residency. Beyond it the
	// reconciler transitions the Sandbox to Failed with
	// reason=PauseTimeoutExceeded and deletes its VM Pod.
	//
	// Sessions in a class with spec.sessionCheckpoint enabled are the
	// exception (setec#202, docs/design/lifecycles.md): past the cap they suspend
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
	// Sandboxes in this class (docs/design/lifecycles.md). A Running session whose last
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
	// Sandboxes of this class (setec#194, docs/design/lifecycles.md and docs/design/storage.md): periodic
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

// EgressAllowSelector names a set of in-cluster Pods a SandboxClass may
// reach on a set of ports. It is rendered verbatim as a NetworkPolicyPeer
// with the listed ports, so its semantics are the Kubernetes ones: both
// selectors set selects Pods matching podSelector in namespaces matching
// namespaceSelector, namespaceSelector alone selects every Pod in those
// namespaces, and podSelector alone selects Pods in the Sandbox's own
// namespace. At least one selector is required.
type EgressAllowSelector struct {
	// NamespaceSelector selects the namespaces the peer Pods live in.
	// +optional
	NamespaceSelector *metav1.LabelSelector `json:"namespaceSelector,omitempty"`

	// PodSelector selects the peer Pods by label.
	// +optional
	PodSelector *metav1.LabelSelector `json:"podSelector,omitempty"`

	// Ports lists the destination ports permitted on the selected Pods.
	// At least one is required: an allowance never opens every port.
	// +kubebuilder:validation:MinItems=1
	// +required
	Ports []EgressAllowPort `json:"ports"`
}

// EgressAllowPort is one destination port of an EgressAllowSelector.
type EgressAllowPort struct {
	// Protocol is the transport protocol. Defaults to TCP.
	// +kubebuilder:validation:Enum=TCP;UDP;SCTP
	// +kubebuilder:default=TCP
	// +optional
	Protocol corev1.Protocol `json:"protocol,omitempty"`

	// Port is the destination port on the selected Pod, as a number or
	// as the name of a container port. A named port follows the Pod's
	// own declaration, so a Service that maps port 443 to a container
	// port named "https" is reached with Port "https". Name the DNS
	// ports by number (53): the operator reads the class alone and
	// cannot see the kube-dns Pod's port names.
	// +required
	Port intstr.IntOrString `json:"port"`
}

// SessionCheckpointSpec tunes the per-class memory-checkpoint policy
// (setec#194). Checkpoints are encrypted with a per-checkpoint DEK
// sealed under the session's cluster-scoped KEK Secret, stored on the
// S3-compatible checkpoint store, and destroyed at session end.
type SessionCheckpointSpec struct {
	// Interval is the cadence of periodic memory checkpoints while
	// the session is Running. Because the durable workspace already
	// provides continuous data safety (docs/design/storage.md), checkpoints serve
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

	// Durable keeps a session through the loss of its node (setec#194).
	// A durable session gets a checkpoint each Interval (15 minutes when
	// Interval is unset), as a diff on the last checkpoint of the same
	// machine. When its node goes away with no notice, the session resumes
	// from its last checkpoint on another node.
	// +optional
	Durable bool `json:"durable,omitempty"`

	// SuspendedTTL is how long a suspended session is kept. A session
	// suspended for longer is recycled: the Sandbox is deleted with its
	// checkpoint, its key and its workspace. Defaults to 7 days.
	// +optional
	SuspendedTTL *metav1.Duration `json:"suspendedTTL,omitempty"`
}

// DefaultSuspendedTTL is the recycle time of a suspended session when the
// class does not set one (setec#193).
const DefaultSuspendedTTL = 7 * 24 * time.Hour

// DefaultSessionIdleTimeout is the idle time after which a session of a
// class with checkpoints is suspended when the class does not set one
// (setec#193): no attach, no exec and no client stream for 10 minutes.
const DefaultSessionIdleTimeout = 10 * time.Minute

// DefaultDurableInterval is the checkpoint interval of a durable session
// when the class does not set one (setec#194).
const DefaultDurableInterval = 15 * time.Minute

// EffectiveInterval returns the periodic checkpoint interval: Interval, or
// DefaultDurableInterval for a durable session, or 0 for none.
func (s *SessionCheckpointSpec) EffectiveInterval() time.Duration {
	if s == nil {
		return 0
	}
	if s.Interval != nil {
		return s.Interval.Duration
	}
	if s.Durable {
		return DefaultDurableInterval
	}
	return 0
}

// RecycleAfter returns the effective SuspendedTTL.
func (s *SessionCheckpointSpec) RecycleAfter() time.Duration {
	if s == nil || s.SuspendedTTL == nil || s.SuspendedTTL.Duration <= 0 {
		return DefaultSuspendedTTL
	}
	return s.SuspendedTTL.Duration
}

// CheckpointBackend returns the effective checkpoint backend name.
func (s *SessionCheckpointSpec) CheckpointBackend() string {
	if s == nil || s.Backend == "" {
		return "s3"
	}
	return s.Backend
}

// ResourceRequests is the scheduler reservation a SandboxClass places
// on every Sandbox Pod it produces, separate from the Pod's limits (see
// SandboxClassSpec.Requests). Both fields are optional; a field left
// unset keeps that resource's request equal to its limit.
type ResourceRequests struct {
	// CPU is the CPU reservation, e.g. "250m" or "1". Must be positive
	// when set.
	// +optional
	CPU *resource.Quantity `json:"cpu,omitempty"`

	// Memory is the memory reservation, e.g. "768Mi". Must be positive
	// when set.
	// +optional
	Memory *resource.Quantity `json:"memory,omitempty"`
}

// SandboxClassStatus reflects the observed state of a SandboxClass: its
// conditions and its warm pool.
type SandboxClassStatus struct {
	// Conditions surface class-level facts the operator wants loudly
	// visible. Today the only stamped type is
	// UnverifiedRestoresAllowed: True when the class carries the
	// setec.zeroroot.ai/allow-unverified-restores="true" dev-mode
	// annotation AND the cluster-level dev gate label is present, so
	// the docs/design/isolation.md invariant gate may serve unverified restores for
	// this class. Anyone auditing the cluster sees the opt-out on the
	// class itself, not buried in per-sandbox events.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// WarmPool reports the warm pool of a launcher class (setec#103).
	// +optional
	WarmPool *SandboxClassWarmPoolStatus `json:"warmPool,omitempty"`
}

// SandboxClassWarmPoolStatus is the state of the warm pool of a class.
type SandboxClassWarmPoolStatus struct {
	// Ready is the number of Ready bases with the current key.
	// +optional
	Ready int32 `json:"ready,omitempty"`

	// Key is the hash of the inputs of the current base: the image
	// digest, the launcher image, the CPU template and the machine size.
	// +optional
	Key string `json:"key,omitempty"`

	// LastUsed is the last time a Sandbox of the class asked for the pool
	// image. A pool whose image nobody ran for 7 days keeps no base.
	// +optional
	LastUsed *metav1.Time `json:"lastUsed,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=sbxcls
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Default",type=boolean,JSONPath=`.spec.default`
// +kubebuilder:printcolumn:name="Max-VCPU",type=integer,JSONPath=`.spec.maxResources.vcpu`,priority=1
// +kubebuilder:printcolumn:name="Max-Memory",type=string,JSONPath=`.spec.maxResources.memory`,priority=1
// +kubebuilder:printcolumn:name="Pool-Ready",type=integer,JSONPath=`.status.warmPool.ready`
// +kubebuilder:printcolumn:name="Pool-Key",type=string,JSONPath=`.status.warmPool.key`,priority=1
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
