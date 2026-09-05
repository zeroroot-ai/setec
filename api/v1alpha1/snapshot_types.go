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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SnapshotInUseFinalizer is applied by the operator to any Snapshot that
// is referenced by a running Sandbox or a pre-warm pool entry. The
// SnapshotReconciler removes the finalizer once the reference count
// drops to zero AND the backend Delete has successfully reclaimed the
// state files.
const SnapshotInUseFinalizer = "setec.zeroroot.ai/snapshot-in-use"

// SnapshotPhase is the high-level lifecycle state of a Snapshot.
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

	// KernelVersion is the guest kernel version used by the source
	// sandbox at snapshot time. Mismatches between snapshot and
	// restore-target kernels cause the Validator to reject the restore.
	// +optional
	KernelVersion string `json:"kernelVersion,omitempty"`

	// VMM is the virtual machine monitor the snapshot is compatible
	// with. Cross-VMM restore is not supported.
	// +required
	VMM VMM `json:"vmm"`

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
	// +kubebuilder:validation:MinLength=1
	// +required
	StorageRef string `json:"storageRef"`

	// Size is the size in bytes of the persisted snapshot state
	// (state.bin + memory.bin). Populated by the operator at creation.
	// +kubebuilder:validation:Minimum=0
	// +optional
	Size int64 `json:"size,omitempty"`

	// SHA256 is the hex-encoded SHA256 digest of the persisted state
	// file, written alongside the state on disk and verified on
	// restore. Populated by the operator; must not be edited.
	// +optional
	SHA256 string `json:"sha256,omitempty"`

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
