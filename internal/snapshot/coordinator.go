// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package snapshot

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	setecgrpcv1 "github.com/zeroroot-ai/setec/api/grpc/v1"
	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
	"github.com/zeroroot-ai/setec/internal/metrics"
	"github.com/zeroroot-ai/setec/internal/snapshot/gate"
	"github.com/zeroroot-ai/setec/internal/snapshot/storage"
)

// NodeAgentClient is the operator-facing view of the gRPC
// NodeAgentService. Declared here (rather than imported from the
// generated stubs directly) so the Coordinator can be unit-tested
// against a hand-rolled mock, and so the controller layer can compose
// a NodeAgentDialer that picks the right client per node.
type NodeAgentClient interface {
	CreateSnapshot(ctx context.Context, in *setecgrpcv1.CreateSnapshotRequest) (*setecgrpcv1.CreateSnapshotResponse, error)
	RestoreSandbox(ctx context.Context, in *setecgrpcv1.RestoreSandboxRequest) (*setecgrpcv1.RestoreSandboxResponse, error)
	PauseSandbox(ctx context.Context, in *setecgrpcv1.PauseSandboxRequest) (*setecgrpcv1.PauseSandboxResponse, error)
	ResumeSandbox(ctx context.Context, in *setecgrpcv1.ResumeSandboxRequest) (*setecgrpcv1.ResumeSandboxResponse, error)
	DeleteSnapshot(ctx context.Context, in *setecgrpcv1.DeleteSnapshotRequest) (*setecgrpcv1.DeleteSnapshotResponse, error)
}

// NodeAgentDialer resolves a node name (as reported by
// Pod.Spec.NodeName) to a NodeAgentClient bound to that node's
// DaemonSet pod. The dialer handles connection re-use, mTLS, and
// endpoint discovery; from the Coordinator's perspective it is a
// plain factory.
type NodeAgentDialer interface {
	Dial(ctx context.Context, nodeName string) (NodeAgentClient, error)
}

// Coordinator orchestrates snapshot-related operations across the
// operator and a single node-agent. It is the operator-side glue
// between the Sandbox/Snapshot CRDs and the node-local work carried
// out by the node-agent gRPC server.
//
// All external effects are injected as interfaces so the Coordinator
// is fully unit-testable without a live Kubernetes API server or a
// real gRPC transport.
type Coordinator struct {
	// Client is a controller-runtime client used to read and write
	// Sandbox/Snapshot CRs and Pods.
	Client client.Client

	// Storage is consulted for Stat calls from the operator side
	// (e.g. "does this storage ref still exist before we attempt
	// restore?"). The operator is NOT expected to Save/Open through
	// this — those calls run on the node-agent side only.
	Storage storage.StorageBackend

	// Dialer resolves node names to NodeAgentClients.
	Dialer NodeAgentDialer

	// Recorder is used to emit Events on the parent Sandbox when a
	// step fails.
	Recorder events.EventRecorder

	// Metrics is optional; nil disables all collector invocations.
	Metrics *metrics.Collectors

	// Tracer is optional; nil disables span emission.
	Tracer trace.Tracer

	// StorageBackendName is the backend identifier forwarded to the
	// node-agent in CreateSnapshotRequest.StorageBackend. Defaults to
	// "local-disk".
	StorageBackendName string

	// Gate resolves the dev-mode opt-out for the docs/design/isolation.md invariant
	// gate. The gate itself is ALWAYS enforced — every restore/resume
	// the Coordinator serves passes through one decision point that
	// fails closed on any unverified invariant. A nil Gate only means
	// no dev opt-out can ever be granted.
	Gate *gate.Gate

	// PoolNamespace is the namespace of the warm pool (setec#103). Only
	// the operator writes there. A base Snapshot is trusted as a clean
	// base only in this namespace.
	PoolNamespace string
}

// Event reason constants — exported so callers can use them for
// reason strings on Sandbox/Snapshot status without redefining.
const (
	EventReasonSnapshotCreated        = "SnapshotCreated"
	EventReasonSnapshotCreateFailed   = "SnapshotCreateFailed"
	EventReasonSnapshotRestoreFailed  = "SnapshotRestoreFailed"
	EventReasonSnapshotRestoreStarted = "SnapshotRestoreStarted"
	EventReasonEntropyReseeded        = "EntropyReseeded"
	EventReasonSandboxUniquified      = "SandboxUniquified"
	EventReasonPauseFailed            = "PauseFailed"
	EventReasonResumeFailed           = "ResumeFailed"
	EventReasonInsufficientStorage    = "InsufficientStorage"
	EventReasonNodeAgentUnreachable   = "NodeAgentUnreachable"
	EventReasonSnapshotNameConflict   = "SnapshotNameConflict"
	EventReasonSnapshotNodeGone       = "SnapshotNodeGone"
	// EventReasonInvariantGateViolation is the typed reason surfaced
	// when the docs/design/isolation.md invariant gate refuses a restore/resume: one
	// or more per-restore invariant verifications did not pass and no
	// dev-mode opt-out is active. The sandbox that received the
	// unverified state is destroyed, never handed to a caller.
	EventReasonInvariantGateViolation = "InvariantGateViolation"
	// EventReasonUnverifiedRestoreAllowed is emitted (as a Warning)
	// every time the dev-mode opt-out serves a restore despite failed
	// invariant verifications. Deliberately loud: dev-mode is an
	// auditable exception, not a quiet default.
	EventReasonUnverifiedRestoreAllowed = "UnverifiedRestoreAllowed"
)

// actionRecordSnapshotPhase is the action constant for events emitted
// by the Coordinator. Defined locally to avoid an import cycle with
// internal/controller (which imports this package).
const actionRecordSnapshotPhase = "RecordSnapshotPhase"

// defaultStorageBackend is the Phase 3 default and is forwarded to
// the node-agent when StorageBackendName is empty.
const defaultStorageBackend = "local-disk"

// ErrSnapshotNameConflict is surfaced when CreateSnapshot is invoked
// for a Sandbox whose target snapshot name is already taken in the
// namespace. The reconciler detects this early and emits a specific
// Event reason.
var ErrSnapshotNameConflict = errors.New("snapshot: name already in use in namespace")

// ErrInvariantGateViolation is surfaced when the docs/design/isolation.md invariant
// gate refuses a restore/resume. Callers MUST treat it as terminal
// for the target sandbox — the VM may already hold unverified
// restored state, so the sandbox is destroyed, never retried into
// service.
var ErrInvariantGateViolation = errors.New("snapshot: docs/design/isolation.md invariant gate refused the restore")

// checkNameFree returns ErrSnapshotNameConflict, with an Event, when a
// Snapshot with the requested name exists.
func (c *Coordinator) checkNameFree(ctx context.Context, sb *setecv1alpha1.Sandbox) error {
	existing := &setecv1alpha1.Snapshot{}
	err := c.Client.Get(ctx, types.NamespacedName{
		Namespace: sb.Namespace,
		Name:      sb.Spec.Snapshot.Name,
	}, existing)
	switch {
	case err == nil:
		c.emit(sb, corev1.EventTypeWarning, EventReasonSnapshotNameConflict,
			fmt.Sprintf("snapshot %q already exists in namespace %q", sb.Spec.Snapshot.Name, sb.Namespace))
		return ErrSnapshotNameConflict
	case !apierrors.IsNotFound(err):
		return fmt.Errorf("coordinator: get existing Snapshot: %w", err)
	}
	return nil
}

// applyKept sets up a kept Snapshot and returns its backend and the key of
// its tenant. Any other Snapshot keeps the default backend and no key.
func (c *Coordinator) applyKept(
	ctx context.Context, sb *setecv1alpha1.Sandbox, snap *setecv1alpha1.Snapshot, parentRef string,
) (backend string, kek []byte, err error) {
	backend = c.backendName()
	if !sb.Spec.Snapshot.Kept {
		return backend, nil, nil
	}
	if parentRef != "" {
		return "", nil, errors.New("coordinator: a kept snapshot is a full snapshot, not a diff")
	}
	if kek, err = c.TenantKEK(ctx, sb.Namespace); err != nil {
		return "", nil, err
	}
	snap.Spec.Kept = true
	snap.Spec.StorageBackend = KeptBackend
	if snap.Spec.TTL == nil {
		snap.Spec.TTL = &metav1.Duration{Duration: DefaultKeptTTL}
	}
	return KeptBackend, kek, nil
}

// CreateSnapshot pauses the source sandbox, delegates snapshot
// persistence to the node-agent, and creates a Snapshot CR on
// success. On any error the Coordinator emits an Event on the parent
// Sandbox explaining the failure and returns the error so the
// caller can re-queue.
//
// Idempotency: if a Snapshot CR already exists with the requested
// name, CreateSnapshot returns ErrSnapshotNameConflict without
// touching the source VM.
func (c *Coordinator) CreateSnapshot(ctx context.Context, sb *setecv1alpha1.Sandbox) error {
	if sb == nil || sb.Spec.Snapshot == nil || sb.Spec.Snapshot.Name == "" {
		return errors.New("coordinator: CreateSnapshot requires Sandbox.spec.snapshot.name")
	}

	ctx, span := c.startSpan(ctx, "snapshot.Create")
	defer span.End()
	span.SetAttributes(
		attribute.String("setec.sandbox", sb.Namespace+"/"+sb.Name),
		attribute.String("setec.snapshot.name", sb.Spec.Snapshot.Name),
	)
	start := time.Now()

	// 1. Detect name conflicts. We do this first so we can fail fast
	//    before touching the source VM.
	if err := c.checkNameFree(ctx, sb); err != nil {
		setSpanErr(span, err.Error())
		return err
	}

	// 2. Resolve the node-agent for the Sandbox's pod. The node name is
	//    part of the Snapshot spec, so it has to be known before the CR
	//    is created.
	pod, podErr := c.getPod(ctx, sb)
	if podErr != nil {
		setSpanErr(span, podErr.Error())
		return podErr
	}
	if pod.Spec.NodeName == "" {
		setSpanErr(span, "pod not scheduled")
		return fmt.Errorf("coordinator: Pod %q has no NodeName; cannot dial node-agent", pod.Name)
	}

	// 3. Materialize the Snapshot CR in phase Creating, BEFORE the
	//    storage write.
	//
	// The CR used to be created after the RPC returned, which made three
	// of the four declared phases unreachable: with no object, there was
	// nothing to report Creating or Failed on, so a snapshot whose write
	// failed left no record at all and an operator reading .status.phase
	// saw only Ready (setec#129). Creating it first also makes the
	// name-conflict check above cover an in-flight attempt, not just a
	// finished one.
	//
	// spec.storageRef is empty here. The backend chooses the reference
	// and returns it with the response, so it is filled in at step 5.
	parentRef, err := c.diffParent(ctx, sb, string(pod.UID))
	if err != nil {
		c.emit(sb, corev1.EventTypeWarning, EventReasonSnapshotCreateFailed, err.Error())
		setSpanErr(span, err.Error())
		return err
	}
	snap := c.newSnapshotCR(ctx, sb, pod.Spec.NodeName)
	snap.Annotations = map[string]string{
		setecv1alpha1.SnapshotSourcePodUIDAnnotation: string(pod.UID),
		// The machine size of the source: a review Sandbox, which may
		// start long after its source is gone, needs it (setec#196).
		setecv1alpha1.SnapshotVCPUAnnotation:   strconv.Itoa(int(sb.Spec.Resources.VCPU)),
		setecv1alpha1.SnapshotMemoryAnnotation: sb.Spec.Resources.Memory.String(),
	}
	snap.Spec.Parent = sb.Spec.Snapshot.Parent
	snap.Spec.Forkable = sb.Spec.Snapshot.Forkable
	// A kept Snapshot goes to the S3-compatible store, sealed with the key
	// of its tenant, and lives 30 days unless the request sets a TTL or a
	// person pins it (setec#196).
	backend, kek, err := c.applyKept(ctx, sb, snap, parentRef)
	if err != nil {
		return err
	}
	if err := c.Client.Create(ctx, snap); err != nil {
		if apierrors.IsAlreadyExists(err) {
			// Someone raced us. Return the sentinel so the reconciler
			// can pick up the existing Snapshot on the next cycle.
			return ErrSnapshotNameConflict
		}
		setSpanErr(span, err.Error())
		return fmt.Errorf("coordinator: create Snapshot CR: %w", err)
	}
	if err := c.markPhase(ctx, snap, setecv1alpha1.SnapshotPhaseCreating, "SnapshotWriteInFlight"); err != nil {
		// The write has not started, so refusing here costs nothing and
		// keeps the invariant that a Snapshot never sits phase-less
		// while its state is being written.
		setSpanErr(span, err.Error())
		return fmt.Errorf("coordinator: mark Snapshot Creating: %w", err)
	}

	// 4. Dial the node-agent and issue the CreateSnapshot RPC.
	na, dialErr := c.Dialer.Dial(ctx, pod.Spec.NodeName)
	if dialErr != nil {
		c.emit(sb, corev1.EventTypeWarning, EventReasonNodeAgentUnreachable,
			fmt.Sprintf("dial node-agent on %q: %v", pod.Spec.NodeName, dialErr))
		c.failSnapshot(ctx, snap, "NodeAgentUnreachable", dialErr)
		setSpanErr(span, dialErr.Error())
		return fmt.Errorf("coordinator: dial node-agent: %w", dialErr)
	}

	resp, rpcErr := na.CreateSnapshot(ctx, &setecgrpcv1.CreateSnapshotRequest{
		SandboxId:        sb.Namespace + "/" + sb.Name,
		SnapshotId:       sb.Namespace + "-" + sb.Spec.Snapshot.Name,
		StorageBackend:   backend,
		SourcePodUid:     string(pod.UID),
		ParentStorageRef: parentRef,
		SessionKek:       kek,
		// The tokens after the snapshot carry the next generation, and
		// no token in the snapshot does (setec#235).
		IdentityGeneration: nextIdentityGeneration(sb),
	})
	if rpcErr != nil {
		reason := EventReasonSnapshotCreateFailed
		if isInsufficientStorage(rpcErr) {
			reason = EventReasonInsufficientStorage
		}
		c.emit(sb, corev1.EventTypeWarning, reason, rpcErr.Error())
		c.failSnapshot(ctx, snap, reason, rpcErr)
		setSpanErr(span, rpcErr.Error())
		c.recordDuration("create", sb, time.Since(start))
		return fmt.Errorf("coordinator: CreateSnapshot RPC: %w", rpcErr)
	}

	// 5. Record the identity generation before the Snapshot is Ready: a
	// Sandbox can load the Snapshot only once it is Ready, and from then
	// on a token from before the snapshot must fail.
	if err := c.recordIdentityGeneration(ctx, sb, nextIdentityGeneration(sb)); err != nil {
		c.failSnapshot(ctx, snap, "IdentityGenerationNotRecorded", err)
		setSpanErr(span, err.Error())
		c.recordDuration("create", sb, time.Since(start))
		return err
	}

	// 6. Record what the backend wrote, then mark Ready.
	original := snap.DeepCopy()
	snap.Spec.StorageRef = resp.GetStorageRef()
	snap.Spec.Size = resp.GetSizeBytes()
	if err := c.Client.Patch(ctx, snap, client.MergeFrom(original)); err != nil {
		// The state is on disk but the CR does not say where. Leaving it
		// Creating would be a lie in the safe direction; Ready without a
		// storageRef would be a lie in the dangerous one, because the
		// restore path keys off the phase.
		c.failSnapshot(ctx, snap, "StorageRefNotRecorded", err)
		setSpanErr(span, err.Error())
		c.recordDuration("create", sb, time.Since(start))
		return fmt.Errorf("coordinator: record Snapshot storage reference: %w", err)
	}

	if err := c.markPhase(ctx, snap, setecv1alpha1.SnapshotPhaseReady, ""); err != nil {
		// Non-fatal; the SnapshotReconciler will re-derive.
		c.emit(sb, corev1.EventTypeWarning, EventReasonSnapshotCreated,
			fmt.Sprintf("snapshot %q persisted but status update failed: %v", snap.Name, err))
	} else {
		c.emit(sb, corev1.EventTypeNormal, EventReasonSnapshotCreated,
			fmt.Sprintf("snapshot %q ready on node %q (%d bytes)", snap.Name, snap.Spec.Node, snap.Spec.Size))
	}

	c.recordDuration("create", sb, time.Since(start))
	return nil
}

// ErrInvalidParent is returned when the parent of a diff snapshot cannot
// carry it.
var ErrInvalidParent = errors.New("coordinator: the parent snapshot cannot carry a diff")

// diffParent returns the storage reference of the parent of a diff
// snapshot, or "" for a full snapshot. The parent must be a Ready Snapshot
// of the same Sandbox, taken from the same Pod, in the same store: the
// memory of a diff holds only the pages that changed in that one machine.
func (c *Coordinator) diffParent(ctx context.Context, sb *setecv1alpha1.Sandbox, podUID string) (string, error) {
	name := sb.Spec.Snapshot.Parent
	if name == "" {
		return "", nil
	}
	parent := &setecv1alpha1.Snapshot{}
	if err := c.Client.Get(ctx, types.NamespacedName{Namespace: sb.Namespace, Name: name}, parent); err != nil {
		return "", fmt.Errorf("%w: get %q: %w", ErrInvalidParent, name, err)
	}
	switch {
	case parent.Status.Phase != setecv1alpha1.SnapshotPhaseReady || parent.Spec.StorageRef == "":
		return "", fmt.Errorf("%w: %q is not Ready", ErrInvalidParent, name)
	case parent.Spec.SourceSandbox != sb.Name:
		return "", fmt.Errorf("%w: %q is a snapshot of %q, not of %q", ErrInvalidParent, name, parent.Spec.SourceSandbox, sb.Name)
	case parent.Annotations[setecv1alpha1.SnapshotSourcePodUIDAnnotation] != podUID:
		return "", fmt.Errorf("%w: %q was taken from another Pod; take a full snapshot first", ErrInvalidParent, name)
	case parent.Spec.StorageBackend != c.backendName():
		return "", fmt.Errorf("%w: %q is in the store %q, not %q", ErrInvalidParent, name, parent.Spec.StorageBackend, c.backendName())
	}
	return parent.Spec.StorageRef, nil
}

// newSnapshotCR builds the Snapshot CR for a sandbox snapshot request.
// Everything it fills is knowable before the storage write: the node is
// the Pod's node, and the CPU template comes from the resolved class.
func (c *Coordinator) newSnapshotCR(ctx context.Context, sb *setecv1alpha1.Sandbox, nodeName string) *setecv1alpha1.Snapshot {
	cpuTemplate := ""
	if sb.Spec.SandboxClassName != "" {
		cls := &setecv1alpha1.SandboxClass{}
		if gerr := c.Client.Get(ctx, types.NamespacedName{Name: sb.Spec.SandboxClassName}, cls); gerr == nil {
			cpuTemplate = cls.Spec.CPUTemplate
		}
	}
	className := sb.Spec.SandboxClassName
	if className == "" {
		// Snapshot.spec.sandboxClass is required non-empty by the CRD
		// schema; fall back to the sandbox name to preserve the
		// invariant even when the user didn't set a class explicitly.
		className = sb.Name
	}
	return &setecv1alpha1.Snapshot{
		Namespace: sb.Namespace,
		Name:      sb.Spec.Snapshot.Name,
		Spec: setecv1alpha1.SnapshotSpec{
			SourceSandbox:  sb.Name,
			SandboxClass:   className,
			ImageRef:       sb.Spec.Image,
			TTL:            ttlFrom(sb.Spec.Snapshot.TTL),
			StorageBackend: c.backendName(),
			Node:           nodeName,
			CPUTemplate:    cpuTemplate,
			InstanceType:   c.instanceType(ctx, nodeName),
		},
	}
}

// instanceType returns the node.kubernetes.io/instance-type of a node, or
// "" when the node or the label is missing.
func (c *Coordinator) instanceType(ctx context.Context, nodeName string) string {
	node := &corev1.Node{}
	if err := c.Client.Get(ctx, types.NamespacedName{Name: nodeName}, node); err != nil {
		return ""
	}
	return node.Labels[corev1.LabelInstanceTypeStable]
}

// markPhase writes one phase/reason pair to the Snapshot status
// subresource and stamps the transition time.
func (c *Coordinator) markPhase(ctx context.Context, snap *setecv1alpha1.Snapshot, phase setecv1alpha1.SnapshotPhase, reason string) error {
	original := snap.DeepCopy()
	snap.Status.Phase = phase
	snap.Status.Reason = reason
	now := metav1.NewTime(time.Now())
	snap.Status.LastTransitionTime = &now
	if err := c.Client.Status().Patch(ctx, snap, client.MergeFrom(original)); err != nil {
		return fmt.Errorf("patch Snapshot status to %s: %w", phase, err)
	}
	return nil
}

// failSnapshot records phase Failed and the reason on a Snapshot whose
// creation could not complete. The CR is deliberately kept: it is the
// only place an operator can read why a snapshot is missing, and the
// phase doc states a Failed snapshot never returns to Ready.
//
// The caller is already returning an error, so a failure to write the
// status is logged on the span rather than replacing that error. It is
// not discarded silently: a swallowed status write is how the phase
// stopped being reported in the first place.
func (c *Coordinator) failSnapshot(ctx context.Context, snap *setecv1alpha1.Snapshot, reason string, cause error) {
	if err := c.markPhase(ctx, snap, setecv1alpha1.SnapshotPhaseFailed, reason); err != nil {
		log.FromContext(ctx).Error(err, "could not record Snapshot phase Failed",
			"snapshot", snap.Namespace+"/"+snap.Name, "reason", reason, "cause", cause)
	}
}

// RestoreSandbox issues the RestoreSandbox RPC to the node holding
// the snapshot state. Pod pinning is the reconciler's responsibility;
// this function assumes the Pod is already scheduled to
// snap.Spec.Node. A non-nil error leaves the Sandbox in Restoring
// state so the reconciler can decide whether to fail or retry —
// EXCEPT an error wrapping ErrInvariantGateViolation, which is
// terminal: the sandbox must be destroyed.
//
// This method is the shared restore/resume chokepoint: any future
// resume path (e.g. session checkpoint resume) that lands its state
// through this coordinator inherits the docs/design/isolation.md invariant gate
// automatically.
func (c *Coordinator) RestoreSandbox(ctx context.Context, sb *setecv1alpha1.Sandbox, snap *setecv1alpha1.Snapshot) error {
	if sb == nil || snap == nil {
		return errors.New("coordinator: RestoreSandbox requires non-nil sandbox and snapshot")
	}
	ctx, span := c.startSpan(ctx, "snapshot.Restore")
	defer span.End()
	span.SetAttributes(
		attribute.String("setec.sandbox", sb.Namespace+"/"+sb.Name),
		attribute.String("setec.snapshot.name", snap.Name),
	)
	start := time.Now()

	// docs/design/isolation.md gate, operator-verifiable half. A snapshot restore is
	// only an intra-session resume when the artifact provably came
	// from the sandbox it is being restored into (spec.sourceSandbox
	// binding). Cross-sandbox restore reuses one session's state for
	// another — invariants 1/3/4 all fail — so outside dev-mode it is
	// refused BEFORE any state is loaded into the target VM.
	cls := c.classOf(ctx, sb)
	bound := snap.Spec.SourceSandbox == sb.Name || c.isCleanBase(snap) || isOwnFork(sb, snap) || isReviewOf(sb, snap)
	preflight := gate.Evidence{
		CleanBase:          bound,
		EntropyReseeded:    true, // verified post-RPC
		IdentityUniquified: true, // verified post-RPC
		SingleSession:      bound,
		ProvenanceVerified: bound,
		EncryptedAtRest:    true, // verified post-RPC
	}
	if decision, gateErr := c.Gate.Decide(ctx, cls, preflight); !decision.Allowed {
		msg := c.gateRefusalMsg("snapshot "+snap.Name, decision, gateErr)
		c.emit(sb, corev1.EventTypeWarning, EventReasonInvariantGateViolation, msg)
		setSpanErr(span, msg)
		return fmt.Errorf("coordinator: %w: %s", ErrInvariantGateViolation, msg)
	}

	pod, err := c.restoreTarget(ctx, sb, snap)
	if err != nil {
		setSpanErr(span, err.Error())
		return err
	}

	var restoreKEK []byte
	if snap.Spec.Kept {
		k, err := c.TenantKEK(ctx, snap.Namespace)
		if err != nil {
			setSpanErr(span, err.Error())
			return err
		}
		restoreKEK = k
	}
	na, dialErr := c.Dialer.Dial(ctx, pod.Spec.NodeName)
	if dialErr != nil {
		c.emit(sb, corev1.EventTypeWarning, EventReasonNodeAgentUnreachable, dialErr.Error())
		setSpanErr(span, dialErr.Error())
		return fmt.Errorf("coordinator: dial node-agent: %w", dialErr)
	}

	resp, rpcErr := na.RestoreSandbox(ctx, &setecgrpcv1.RestoreSandboxRequest{
		SnapshotId:         snap.Namespace + "-" + snap.Name,
		StorageRef:         snap.Spec.StorageRef,
		StorageBackend:     snap.Spec.StorageBackend,
		TargetPodUid:       string(pod.UID),
		SandboxId:          sb.Namespace + "/" + sb.Name,
		PodIp:              pod.Status.PodIP,
		Hostname:           sb.Name,
		StateTakenUnixNano: snap.CreationTimestamp.UnixNano(),
		SessionKek:         restoreKEK,
	})
	if rpcErr != nil || (resp != nil && !resp.Success) {
		msg := errString(rpcErr, resp)
		c.emit(sb, corev1.EventTypeWarning, EventReasonSnapshotRestoreFailed, msg)
		setSpanErr(span, msg)
		c.recordDuration("restore", sb, time.Since(start))
		return fmt.Errorf("coordinator: RestoreSandbox RPC: %s", msg)
	}

	// docs/design/isolation.md gate, full evidence. The node reported success — the
	// state is loaded — so a refusal here is terminal for the sandbox:
	// pause the VM best-effort and surface the typed violation. This
	// closes the "node-agent opted out of a verification" hole: a
	// restore whose reseed/uniquification/encryption is unverified is
	// never handed to a caller outside dev-mode.
	ev := gate.Evidence{
		CleanBase:          bound,
		EntropyReseeded:    resp.GetEntropyReseeded(),
		IdentityUniquified: resp.GetUniquified(),
		SingleSession:      bound,
		ProvenanceVerified: bound,
		EncryptedAtRest:    resp.GetEncryptedAtRest(),
	}
	decision, gateErr := c.Gate.Decide(ctx, cls, ev)
	if !decision.Allowed {
		msg := c.gateRefusalMsg("snapshot "+snap.Name, decision, gateErr)
		if _, pauseErr := na.PauseSandbox(ctx, &setecgrpcv1.PauseSandboxRequest{
			SandboxId:    sb.Namespace + "/" + sb.Name,
			TargetPodUid: string(pod.UID),
		}); pauseErr != nil {
			msg += fmt.Sprintf("; additionally failed to pause the unverified VM: %v", pauseErr)
		}
		c.emit(sb, corev1.EventTypeWarning, EventReasonInvariantGateViolation, msg)
		setSpanErr(span, msg)
		c.recordDuration("restore", sb, time.Since(start))
		return fmt.Errorf("coordinator: %w: %s", ErrInvariantGateViolation, msg)
	}
	if decision.DevOptOut {
		c.emit(sb, corev1.EventTypeWarning, EventReasonUnverifiedRestoreAllowed,
			fmt.Sprintf("DEV-MODE OPT-OUT: serving restore of snapshot %q despite %s", snap.Name, decision.String()))
	}

	c.emitRestored(sb, snap, pod, resp)
	c.recordDuration("restore", sb, time.Since(start))
	return nil
}

// emitRestored emits the Events of a restore that passed the gate.
func (c *Coordinator) emitRestored(
	sb *setecv1alpha1.Sandbox, snap *setecv1alpha1.Snapshot, pod *corev1.Pod, resp *setecgrpcv1.RestoreSandboxResponse,
) {
	c.emit(sb, corev1.EventTypeNormal, EventReasonSnapshotRestoreStarted,
		fmt.Sprintf("restored sandbox from snapshot %q on node %q", snap.Name, pod.Spec.NodeName))
	// Surface the node-agent's active entropy-reseed confirmation
	// (setec#72): the restored guest's CSPRNG verifiably received
	// fresh entropy before the sandbox was handed over. Only emitted
	// on explicit confirmation — never inferred.
	if resp.GetEntropyReseeded() {
		c.emit(sb, corev1.EventTypeNormal, EventReasonEntropyReseeded,
			fmt.Sprintf("restored guest CSPRNG reseeded with fresh entropy (snapshot %q)", snap.Name))
	}
	// Surface the node-agent's uniquification confirmation (docs/design/isolation.md
	// invariant 2, setec#189): the restored guest verifiably adopted a
	// fresh machine-id/boot-id/hostname, observes its CNI-assigned Pod
	// IP, and its vsock CID is unique on the node. Only emitted on
	// explicit confirmation — never inferred.
	if resp.GetUniquified() {
		c.emit(sb, corev1.EventTypeNormal, EventReasonSandboxUniquified,
			fmt.Sprintf("restored guest identity uniquified: fresh machine-id/boot-id/hostname, Pod IP verified, vsock CID unique (snapshot %q)", snap.Name))
	}
}

// restoreTarget is the scheduled Pod that loads snap. A snapshot on the
// local disk of a node loads on that node only. A snapshot in the
// S3-compatible store loads on any node.
func (c *Coordinator) restoreTarget(ctx context.Context, sb *setecv1alpha1.Sandbox, snap *setecv1alpha1.Snapshot) (*corev1.Pod, error) {
	pod, err := c.getPod(ctx, sb)
	if err != nil {
		return nil, err
	}
	if pod.Spec.NodeName == "" {
		return nil, fmt.Errorf("coordinator: Pod %q has no NodeName; restore requires a scheduled pod", pod.Name)
	}
	local := snap.Spec.StorageBackend == "" || snap.Spec.StorageBackend == "local-disk"
	if local && pod.Spec.NodeName != snap.Spec.Node {
		return nil, fmt.Errorf("coordinator: snapshot lives on %q but Pod is on %q; restore must run on the snapshot's node",
			snap.Spec.Node, pod.Spec.NodeName)
	}
	return pod, nil
}

// Pause invokes the node-agent Firecracker pause RPC.
func (c *Coordinator) Pause(ctx context.Context, sb *setecv1alpha1.Sandbox) error {
	ctx, span := c.startSpan(ctx, "snapshot.Pause")
	defer span.End()
	start := time.Now()

	pod, err := c.getPod(ctx, sb)
	if err != nil {
		setSpanErr(span, err.Error())
		return err
	}
	na, dialErr := c.Dialer.Dial(ctx, pod.Spec.NodeName)
	if dialErr != nil {
		c.emit(sb, corev1.EventTypeWarning, EventReasonNodeAgentUnreachable, dialErr.Error())
		setSpanErr(span, dialErr.Error())
		return dialErr
	}
	resp, rpcErr := na.PauseSandbox(ctx, &setecgrpcv1.PauseSandboxRequest{
		SandboxId:    sb.Namespace + "/" + sb.Name,
		TargetPodUid: string(pod.UID),
	})
	if rpcErr != nil || (resp != nil && !resp.Success) {
		msg := errString(rpcErr, resp)
		c.emit(sb, corev1.EventTypeWarning, EventReasonPauseFailed, msg)
		setSpanErr(span, msg)
		c.recordDuration("pause", sb, time.Since(start))
		return fmt.Errorf("coordinator: PauseSandbox RPC: %s", msg)
	}
	c.recordDuration("pause", sb, time.Since(start))
	return nil
}

// DeleteSnapshot drives the node-agent to securely erase the
// snapshot's persisted state. Returns (deleted=true, nil) on
// success. Callers are expected to remove the in-use finalizer only
// after this completes.
func (c *Coordinator) DeleteSnapshot(ctx context.Context, snap *setecv1alpha1.Snapshot) error {
	if snap == nil {
		return errors.New("coordinator: DeleteSnapshot requires a non-nil Snapshot")
	}
	ctx, span := c.startSpan(ctx, "snapshot.Delete")
	defer span.End()
	start := time.Now()

	// A snapshot whose write failed has no storage reference: nothing was
	// stored, so nothing is erased. Asking the node agent kept the
	// finalizer on and the Snapshot undeletable.
	if snap.Spec.StorageRef == "" {
		c.recordDelete(snap, time.Since(start))
		return nil
	}
	if snap.Spec.Node == "" {
		return errors.New("coordinator: Snapshot has no node; cannot delete without a routing target")
	}
	// A local-disk snapshot lives on its node's disk. When that Node no
	// longer exists, the state went with it, and no node-agent can ever
	// answer for it. Retrying forever kept the finalizer on and the
	// Snapshot undeletable (setec#19: TestPhase3_SnapshotTTL).
	if gone, err := c.nodeGone(ctx, snap); err != nil {
		setSpanErr(span, err.Error())
		return err
	} else if gone {
		if c.Recorder != nil {
			c.Recorder.Eventf(snap, nil, corev1.EventTypeNormal, EventReasonSnapshotNodeGone, "DeleteSnapshot",
				"node %q no longer exists; its local-disk state went with it", snap.Spec.Node)
		}
		c.recordDelete(snap, time.Since(start))
		return nil
	}
	na, dialErr := c.Dialer.Dial(ctx, snap.Spec.Node)
	if dialErr != nil {
		setSpanErr(span, dialErr.Error())
		return fmt.Errorf("coordinator: dial node-agent: %w", dialErr)
	}
	resp, rpcErr := na.DeleteSnapshot(ctx, &setecgrpcv1.DeleteSnapshotRequest{
		SnapshotId:     snap.Namespace + "-" + snap.Name,
		StorageRef:     snap.Spec.StorageRef,
		StorageBackend: snap.Spec.StorageBackend,
	})
	if rpcErr != nil || (resp != nil && !resp.Success) {
		msg := errString(rpcErr, resp)
		setSpanErr(span, msg)
		c.recordDelete(snap, time.Since(start))
		return fmt.Errorf("coordinator: DeleteSnapshot RPC: %s", msg)
	}
	c.recordDelete(snap, time.Since(start))
	return nil
}

// nodeGone reports whether snap is node-local (local-disk) and its Node
// no longer exists. A Snapshot on any other backend is never gone this
// way: its state does not live on the node.
func (c *Coordinator) nodeGone(ctx context.Context, snap *setecv1alpha1.Snapshot) (bool, error) {
	backend := snap.Spec.StorageBackend
	if backend == "" {
		backend = defaultStorageBackend
	}
	if backend != defaultStorageBackend || c.Client == nil {
		return false, nil
	}
	var node corev1.Node
	err := c.Client.Get(ctx, client.ObjectKey{Name: snap.Spec.Node}, &node)
	if apierrors.IsNotFound(err) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("coordinator: get node %q: %w", snap.Spec.Node, err)
	}
	return false, nil
}

// Resume invokes the node-agent Firecracker resume RPC.
func (c *Coordinator) Resume(ctx context.Context, sb *setecv1alpha1.Sandbox) error {
	ctx, span := c.startSpan(ctx, "snapshot.Resume")
	defer span.End()
	start := time.Now()

	pod, err := c.getPod(ctx, sb)
	if err != nil {
		setSpanErr(span, err.Error())
		return err
	}
	na, dialErr := c.Dialer.Dial(ctx, pod.Spec.NodeName)
	if dialErr != nil {
		c.emit(sb, corev1.EventTypeWarning, EventReasonNodeAgentUnreachable, dialErr.Error())
		setSpanErr(span, dialErr.Error())
		return dialErr
	}
	resp, rpcErr := na.ResumeSandbox(ctx, &setecgrpcv1.ResumeSandboxRequest{
		SandboxId:    sb.Namespace + "/" + sb.Name,
		TargetPodUid: string(pod.UID),
	})
	if rpcErr != nil || (resp != nil && !resp.Success) {
		msg := errString(rpcErr, resp)
		c.emit(sb, corev1.EventTypeWarning, EventReasonResumeFailed, msg)
		setSpanErr(span, msg)
		c.recordDuration("resume", sb, time.Since(start))
		return fmt.Errorf("coordinator: ResumeSandbox RPC: %s", msg)
	}
	c.recordDuration("resume", sb, time.Since(start))
	return nil
}

// --- helpers -------------------------------------------------------

// classOf resolves the Sandbox's SandboxClass for the invariant gate's
// dev-opt-out lookup. Any failure (no class named, class missing)
// returns nil, which the gate treats as "no opt-out" — fail closed.
func (c *Coordinator) classOf(ctx context.Context, sb *setecv1alpha1.Sandbox) *setecv1alpha1.SandboxClass {
	if sb.Spec.SandboxClassName == "" {
		return nil
	}
	cls := &setecv1alpha1.SandboxClass{}
	if err := c.Client.Get(ctx, types.NamespacedName{Name: sb.Spec.SandboxClassName}, cls); err != nil {
		return nil
	}
	return cls
}

// gateRefusalMsg renders the one-line message for an invariant-gate
// refusal, appending the opt-out resolution error (e.g. an unreadable
// gate namespace) when there is one.
func (c *Coordinator) gateRefusalMsg(subject string, decision gate.Decision, gateErr error) string {
	msg := fmt.Sprintf("docs/design/isolation.md invariant gate refused restore of %s: %s (destroying sandbox; dev-mode opt-out requires the %s=\"true\" annotation on the SandboxClass AND the %s=true label on the %q namespace)",
		subject, decision.String(), gate.AllowUnverifiedRestoresAnnotation, gate.DefaultAllowDevLabel, gate.DefaultGateNamespace)
	if gateErr != nil {
		msg += fmt.Sprintf("; opt-out resolution failed closed: %v", gateErr)
	}
	return msg
}

// getPod returns the Pod backing the Sandbox (named "<sandbox>-vm" by
// convention) or an error if it is missing.
func (c *Coordinator) getPod(ctx context.Context, sb *setecv1alpha1.Sandbox) (*corev1.Pod, error) {
	name := sb.Status.PodName
	if name == "" {
		name = sb.Name + "-vm"
	}
	pod := &corev1.Pod{}
	if err := c.Client.Get(ctx, types.NamespacedName{Namespace: sb.Namespace, Name: name}, pod); err != nil {
		return nil, fmt.Errorf("coordinator: get Pod %q: %w", name, err)
	}
	return pod, nil
}

// backendName returns the configured storage backend identifier,
// defaulting to local-disk.
func (c *Coordinator) backendName() string {
	if c.StorageBackendName != "" {
		return c.StorageBackendName
	}
	return defaultStorageBackend
}

// startSpan returns a span from the configured tracer, or a no-op
// span from the OTel noop provider when tracing is disabled.
func (c *Coordinator) startSpan(ctx context.Context, name string) (context.Context, trace.Span) {
	t := c.Tracer
	if t == nil {
		t = tracenoop.NewTracerProvider().Tracer("")
	}
	return t.Start(ctx, name)
}

// emit emits an Event via the configured recorder if one is set.
func (c *Coordinator) emit(obj any, eventType, reason, message string) {
	if c.Recorder == nil {
		return
	}
	if _, ok := obj.(interface {
		GetName() string
		GetNamespace() string
	}); ok {
		if r, ok := obj.(*setecv1alpha1.Sandbox); ok {
			c.Recorder.Eventf(r, nil, eventType, reason, actionRecordSnapshotPhase, "%s", message)
			return
		}
	}
}

// recordDuration observes a snapshot-operation duration when metrics
// are enabled.
func (c *Coordinator) recordDuration(operation string, sb *setecv1alpha1.Sandbox, d time.Duration) {
	if c.Metrics == nil {
		return
	}
	c.Metrics.RecordSnapshotDuration(operation, sb.Spec.SandboxClassName, d)
}

// recordDelete records the delete-operation duration. Delete is the
// only Snapshot-bound operation without a Sandbox context, so it
// gets its own helper.
func (c *Coordinator) recordDelete(snap *setecv1alpha1.Snapshot, d time.Duration) {
	if c.Metrics == nil {
		return
	}
	c.Metrics.RecordSnapshotDuration("delete", snap.Spec.SandboxClass, d)
}

// setSpanErr records an error on a span, with defensive nil-handling.
func setSpanErr(span trace.Span, msg string) {
	if span == nil {
		return
	}
	span.SetStatus(codes.Error, msg)
}

// isInsufficientStorage inspects an RPC error string for the local-
// disk sentinel so we can emit a user-meaningful Event reason. The
// gRPC surface will eventually use a proper status code; for Phase 3
// we match on the embedded sentinel text the node-agent forwards.
func isInsufficientStorage(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, storage.ErrInsufficientStorage.Error())
}

// errString reduces an RPC (err, resp) pair to a single human message
// suitable for an Event. Prefers the explicit resp.Error when
// present, otherwise stringifies the Go error.
func errString(rpcErr error, resp interface{ GetError() string }) string {
	if resp != nil && resp.GetError() != "" {
		return resp.GetError()
	}
	if rpcErr != nil {
		return rpcErr.Error()
	}
	return "unknown error"
}

// ttlFrom safely propagates a pointer-typed duration.
func ttlFrom(ttl *metav1.Duration) *metav1.Duration {
	if ttl == nil {
		return nil
	}
	out := *ttl
	return &out
}

// isOwnFork reports whether sb is a fork of a forkable Snapshot of its own
// namespace (setec#195). The namespace is one owner pair, so the state
// stays with its owner, and each fork gets a new identity and randomness.
func isOwnFork(sb *setecv1alpha1.Sandbox, snap *setecv1alpha1.Snapshot) bool {
	return snap.Spec.Forkable && snap.Namespace == sb.Namespace && snap.Spec.SourceSandbox != ""
}

// nextIdentityGeneration is the identity generation of a Sandbox after a
// snapshot (setec#235), or 0 for a Sandbox with no identity.
func nextIdentityGeneration(sb *setecv1alpha1.Sandbox) int64 {
	if sb.Status.Identity == nil {
		return 0
	}
	return sb.Status.Identity.Generation + 1
}

// recordIdentityGeneration raises the identity generation in the status of
// the live Sandbox to gen. A token of an earlier generation then fails.
func (c *Coordinator) recordIdentityGeneration(ctx context.Context, sb *setecv1alpha1.Sandbox, gen int64) error {
	if gen == 0 {
		return nil
	}
	live := &setecv1alpha1.Sandbox{}
	if err := c.Client.Get(ctx, client.ObjectKeyFromObject(sb), live); err != nil {
		return fmt.Errorf("coordinator: get the Sandbox to raise its identity generation: %w", err)
	}
	if live.Status.Identity == nil || live.Status.Identity.Generation >= gen {
		return nil
	}
	original := live.DeepCopy()
	live.Status.Identity.Generation = gen
	if err := c.Client.Status().Patch(ctx, live, client.MergeFrom(original)); err != nil {
		return fmt.Errorf("coordinator: raise the identity generation: %w", err)
	}
	if sb.Status.Identity != nil {
		sb.Status.Identity.Generation = gen
	}
	return nil
}
