// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SnapshotInUseFinalizer is applied by the operator to any Snapshot that
// is referenced by a running Sandbox or a pre-warm pool entry. The
// SnapshotReconciler removes the finalizer once the reference count
// drops to zero AND the backend Delete has successfully reclaimed the
// state files.
const SnapshotInUseFinalizer = "setec.zeroroot.ai/snapshot-in-use"

// SnapshotSourcePodUIDAnnotation records the UID of the Pod whose machine
// the Snapshot was taken from. A diff snapshot is only valid on top of a
// parent from the same machine, so the operator compares the two.
const SnapshotSourcePodUIDAnnotation = "setec.zeroroot.ai/source-pod-uid"

// SnapshotVCPUAnnotation and SnapshotMemoryAnnotation record the machine
// size of the source of a Snapshot. A Sandbox that loads it needs the
// same size.
const (
	SnapshotVCPUAnnotation   = "setec.zeroroot.ai/vcpu"
	SnapshotMemoryAnnotation = "setec.zeroroot.ai/memory"
)

// SnapshotPhase is the high-level lifecycle state of a Snapshot.
//
// All four values are written by the operator. Ready was once the only
// one that any code assigned, so a Snapshot whose storage write failed
// kept whatever phase it had and an operator reading .status.phase
// could not tell a finished snapshot from a broken one (setec#129).
//
//	Creating     the CR exists and the storage write is in flight
//	Ready        the state is persisted and may be restored from
//	Failed       the write could not complete; status.reason names why
//	Terminating  deletion started and the in-use finalizer is still held
//
// +kubebuilder:validation:Enum=Creating;Ready;Failed;Terminating
type SnapshotPhase string

const (
	// SnapshotPhaseCreating indicates the snapshot storage write is
	// in-flight; state files have not yet been finalized.
	SnapshotPhaseCreating SnapshotPhase = "Creating"
	// SnapshotPhaseReady indicates the snapshot has been persisted,
	// verified via SHA256, and is ready to be referenced by a Sandbox.
	SnapshotPhaseReady SnapshotPhase = "Ready"
	// SnapshotPhaseFailed indicates creation failed; the Snapshot CR is
	// retained for observability. Failed snapshots never transition
	// back to Ready; the user must delete and re-snapshot.
	SnapshotPhaseFailed SnapshotPhase = "Failed"
	// SnapshotPhaseTerminating indicates deletion is in-flight; the
	// backend is erasing state files before the CR finalizer is
	// removed.
	SnapshotPhaseTerminating SnapshotPhase = "Terminating"
)

// SnapshotSpec captures the user-visible description of a saved microVM
// state. All scalar fields are populated by the operator when it
// creates the Snapshot on behalf of a Sandbox; direct user-authored
// Snapshot CRs are accepted but uncommon (the usual entry point is
// Sandbox.spec.snapshot.create=true).
type SnapshotSpec struct {
	// Kept marks a Snapshot kept for review (setec#196): sealed with the
	// key of its tenant, loaded only by a review Sandbox, deleted after
	// its TTL (30 days by default) unless Pinned.
	// +optional
	Kept bool `json:"kept,omitempty"`

	// Pinned keeps a kept Snapshot past its TTL. The pinned Snapshots of a
	// tenant count against a storage limit, and a pin above it is refused.
	// +optional
	Pinned bool `json:"pinned,omitempty"`

	// Forkable lets a Sandbox of the same namespace other than the source
	// start from this Snapshot (setec#195). The namespace is the one owner
	// pair, so a fork loads only for the owner of the Snapshot.
	// +optional
	Forkable bool `json:"forkable,omitempty"`

	// Parent is the Snapshot that this diff snapshot builds on. A restore
	// loads the parent first, and a parent is not deleted while a diff
	// names it. Empty for a full snapshot.
	// +optional
	Parent string `json:"parent,omitempty"`

	// SourceSandbox is the name of the Sandbox the snapshot was taken
	// from. May be empty for pool-origin snapshots that were never tied
	// to a user Sandbox.
	// +optional
	SourceSandbox string `json:"sourceSandbox,omitempty"`

	// SandboxClass is the name of the SandboxClass this snapshot is
	// compatible with. A Sandbox restoring from this snapshot MUST
	// reference the same class; the snapshot.Validator enforces the
	// match.
	// +kubebuilder:validation:MinLength=1
	// +required
	SandboxClass string `json:"sandboxClass"`

	// ImageRef is the OCI reference the source Sandbox was running at
	// snapshot time. The restore-target Sandbox's image MUST match
	// (empty image on the Sandbox is allowed — the snapshot's image is
	// used verbatim).
	// +kubebuilder:validation:MinLength=1
	// +required
	ImageRef string `json:"imageRef"`

	// TTL optionally bounds the lifetime of the snapshot. Once the
	// snapshot is older than TTL AND no Sandbox references it, the
	// SnapshotReconciler deletes it. When unset, snapshots live until
	// explicitly deleted or garbage-collected by namespace deletion.
	// +optional
	TTL *metav1.Duration `json:"ttl,omitempty"`

	// StorageBackend names the backend that wrote the state files
	// (e.g. "local-disk"). Phase 3 ships only local-disk; future phases
	// may add object-store etc.
	// +kubebuilder:validation:MinLength=1
	// +required
	StorageBackend string `json:"storageBackend"`

	// StorageRef is the opaque backend reference the node-agent uses to
	// locate the state files. For local-disk this is the snapshot ID
	// under the configured snapshot root. Users SHOULD treat this as
	// opaque.
	//
	// EMPTY WHILE Creating. The backend chooses the reference and only
	// returns it once the state write has completed, so a Snapshot that
	// is still being written has no storageRef to record. This field was
	// required with a minimum length of 1, which is why the CR could not
	// be created until after the write, and why status.phase could only
	// ever hold a terminal value (setec#129). A Snapshot in phase Ready
	// always carries a non-empty storageRef, and snapshot.Validate
	// refuses a restore from one that does not.
	// +optional
	StorageRef string `json:"storageRef,omitempty"`

	// Size is the size in bytes of the persisted snapshot state
	// (state.bin + memory.bin). Populated by the operator at creation.
	// +kubebuilder:validation:Minimum=0
	// +optional
	Size int64 `json:"size,omitempty"`

	// CPUTemplate is the CPU template of the class at the time of the
	// snapshot. A restore needs a class with the same template.
	// +optional
	CPUTemplate string `json:"cpuTemplate,omitempty"`

	// InstanceType is the node.kubernetes.io/instance-type of the source
	// node. With no CPU template, a restore runs only on a node of this
	// instance type, which has the same CPU.
	// +optional
	InstanceType string `json:"instanceType,omitempty"`

	// Node is the name of the node holding the state files. Sandboxes
	// restoring from this snapshot are pinned to this node via
	// Pod.Spec.NodeName.
	// +kubebuilder:validation:MinLength=1
	// +required
	Node string `json:"node"`
}

// SnapshotStatus reflects the observed state of a Snapshot.
type SnapshotStatus struct {
	// Phase is the high-level lifecycle state.
	// +optional
	Phase SnapshotPhase `json:"phase,omitempty"`

	// Reason is a short, machine-readable explanation for the current
	// phase (e.g. "InsufficientStorage", "NodeAgentUnreachable",
	// "NameConflict").
	// +optional
	Reason string `json:"reason,omitempty"`

	// LastTransitionTime is the timestamp of the most recent phase
	// change.
	// +optional
	LastTransitionTime *metav1.Time `json:"lastTransitionTime,omitempty"`

	// ReferenceCount is the number of Sandboxes currently pointing at
	// this Snapshot via spec.snapshotRef, observed by the
	// SnapshotReconciler via a field indexer. The finalizer is kept
	// while ReferenceCount > 0.
	// +optional
	ReferenceCount int32 `json:"referenceCount,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,shortName=snap
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Class",type=string,JSONPath=`.spec.sandboxClass`
// +kubebuilder:printcolumn:name="Size",type=integer,JSONPath=`.spec.size`
// +kubebuilder:printcolumn:name="Node",type=string,JSONPath=`.spec.node`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// status.lastTransitionTime is stamped on every phase change and read by
// nothing (#121). With the phases now reachable (#137), when a snapshot entered
// its phase is the operator's question, so a print column is its consumer.
// priority=1, so the default `kubectl get snapshots` is unchanged.
// +kubebuilder:printcolumn:name="Transitioned",type=date,JSONPath=`.status.lastTransitionTime`,priority=1

// Snapshot is the Schema for the snapshots API. A Snapshot is a
// namespaced representation of a saved microVM state (CPU state,
// memory, and associated metadata). Snapshots are produced by the
// operator + node-agent at the user's request (via
// Sandbox.spec.snapshot.create=true) and consumed by later Sandboxes
// via spec.snapshotRef.name. Snapshots are node-local: they cannot be
// restored across nodes without a future cross-node migration backend.
type Snapshot struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is the standard Kubernetes object metadata.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// spec defines the desired state of the Snapshot.
	// +required
	Spec SnapshotSpec `json:"spec"`

	// status reflects the observed state of the Snapshot.
	// +optional
	Status SnapshotStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// SnapshotList is a list of Snapshot resources.
type SnapshotList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Snapshot `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Snapshot{}, &SnapshotList{})
}
