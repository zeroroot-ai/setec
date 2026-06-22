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

package snapshot

import (
	"context"
	"errors"
	"fmt"
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

	setecgrpcv1 "github.com/zeroroot-ai/setec/api/grpc/v1"
	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
	"github.com/zeroroot-ai/setec/internal/metrics"
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
	QueryPool(ctx context.Context, in *setecgrpcv1.QueryPoolRequest) (*setecgrpcv1.QueryPoolResponse, error)
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

	// KataSocketPattern is a format string for the per-Sandbox
	// Firecracker API socket. The format receives a single string
	// argument: the Pod UID (which Kata uses as the sandbox id). The
	// default "/run/kata-containers/%s/firecracker.socket" matches
	// the documented Kata 3.x layout; custom Kata builds may override.
	KataSocketPattern string

	// StorageBackendName is the backend identifier forwarded to the
	// node-agent in CreateSnapshotRequest.StorageBackend. Defaults to
	// "local-disk".
	StorageBackendName string
}

// Event reason constants — exported so callers can use them for
// reason strings on Sandbox/Snapshot status without redefining.
const (
	EventReasonSnapshotCreated        = "SnapshotCreated"
	EventReasonSnapshotCreateFailed   = "SnapshotCreateFailed"
	EventReasonSnapshotRestoreFailed  = "SnapshotRestoreFailed"
	EventReasonSnapshotRestoreStarted = "SnapshotRestoreStarted"
	EventReasonPauseFailed            = "PauseFailed"
	EventReasonResumeFailed           = "ResumeFailed"
	EventReasonInsufficientStorage    = "InsufficientStorage"
	EventReasonNodeAgentUnreachable   = "NodeAgentUnreachable"
	EventReasonSnapshotNameConflict   = "SnapshotNameConflict"
)

// defaultKataSocketPattern is used when the Coordinator's
// KataSocketPattern field is empty.
const defaultKataSocketPattern = "/run/kata-containers/%s/firecracker.socket"

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
	existing := &setecv1alpha1.Snapshot{}
	err := c.Client.Get(ctx, types.NamespacedName{
		Namespace: sb.Namespace,
		Name:      sb.Spec.Snapshot.Name,
	}, existing)
	switch {
	case err == nil:
		c.emit(sb, corev1.EventTypeWarning, EventReasonSnapshotNameConflict,
			fmt.Sprintf("snapshot %q already exists in namespace %q", sb.Spec.Snapshot.Name, sb.Namespace))
		setSpanErr(span, "name conflict")
		return ErrSnapshotNameConflict
	case !apierrors.IsNotFound(err):
		setSpanErr(span, err.Error())
		return fmt.Errorf("coordinator: get existing Snapshot: %w", err)
	}

	// 2. Resolve the node-agent for the Sandbox's pod.
	pod, podErr := c.getPod(ctx, sb)
	if podErr != nil {
		setSpanErr(span, podErr.Error())
		return podErr
	}
	if pod.Spec.NodeName == "" {
		setSpanErr(span, "pod not scheduled")
		return fmt.Errorf("coordinator: Pod %q has no NodeName; cannot dial node-agent", pod.Name)
	}

	na, dialErr := c.Dialer.Dial(ctx, pod.Spec.NodeName)
	if dialErr != nil {
		c.emit(sb, corev1.EventTypeWarning, EventReasonNodeAgentUnreachable,
			fmt.Sprintf("dial node-agent on %q: %v", pod.Spec.NodeName, dialErr))
		setSpanErr(span, dialErr.Error())
		return fmt.Errorf("coordinator: dial node-agent: %w", dialErr)
	}

	// 3. Issue the CreateSnapshot RPC.
	socket := c.socketForPod(pod)
	resp, rpcErr := na.CreateSnapshot(ctx, &setecgrpcv1.CreateSnapshotRequest{
		SandboxId:        sb.Namespace + "/" + sb.Name,
		SnapshotId:       sb.Namespace + "-" + sb.Spec.Snapshot.Name,
		StorageBackend:   c.backendName(),
		SourceKataSocket: socket,
	})
	if rpcErr != nil {
		reason := EventReasonSnapshotCreateFailed
		if isInsufficientStorage(rpcErr) {
			reason = EventReasonInsufficientStorage
		}
		c.emit(sb, corev1.EventTypeWarning, reason, rpcErr.Error())
		setSpanErr(span, rpcErr.Error())
		c.recordDuration("create", sb, time.Since(start))
		return fmt.Errorf("coordinator: CreateSnapshot RPC: %w", rpcErr)
	}

	// 4. Materialize the Snapshot CR. VMM is populated from the
	// resolved class when possible so the CRD enum validation is
	// satisfied. Callers using the bare sandbox (no class) fall back
	// to Firecracker, matching Phase 3's supported-VMM default.
	vmm := setecv1alpha1.VMMFirecracker
	if sb.Spec.SandboxClassName != "" {
		cls := &setecv1alpha1.SandboxClass{}
		if gerr := c.Client.Get(ctx, types.NamespacedName{Name: sb.Spec.SandboxClassName}, cls); gerr == nil && cls.Spec.VMM != "" { //nolint:staticcheck // back-compat: VMM retained until v2
			vmm = cls.Spec.VMM //nolint:staticcheck // back-compat: VMM retained until v2
		}
	}
	className := sb.Spec.SandboxClassName
	if className == "" {
		// Snapshot.spec.sandboxClass is required non-empty by the CRD
		// schema; fall back to the sandbox name to preserve the
		// invariant even when the user didn't set a class explicitly.
		className = sb.Name
	}
	snap := &setecv1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: sb.Namespace,
			Name:      sb.Spec.Snapshot.Name,
		},
		Spec: setecv1alpha1.SnapshotSpec{
			SourceSandbox:  sb.Name,
			SandboxClass:   className,
			ImageRef:       sb.Spec.Image,
			VMM:            vmm,
			TTL:            ttlFrom(sb.Spec.Snapshot.TTL),
			StorageBackend: c.backendName(),
			StorageRef:     resp.GetStorageRef(),
			Size:           resp.GetSizeBytes(),
			SHA256:         resp.GetSha256(),
			Node:           pod.Spec.NodeName,
		},
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

	// 5. Mark Ready on the status subresource.
	snap.Status.Phase = setecv1alpha1.SnapshotPhaseReady
	now := metav1.NewTime(time.Now())
	snap.Status.LastTransitionTime = &now
	if err := c.Client.Status().Update(ctx, snap); err != nil {
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

// RestoreSandbox issues the RestoreSandbox RPC to the node holding
// the snapshot state. Pod pinning is the reconciler's responsibility;
// this function assumes the Pod is already scheduled to
// snap.Spec.Node. A non-nil error leaves the Sandbox in Restoring
// state so the reconciler can decide whether to fail or retry.
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

	pod, err := c.getPod(ctx, sb)
	if err != nil {
		setSpanErr(span, err.Error())
		return err
	}
	if pod.Spec.NodeName == "" {
		setSpanErr(span, "pod not scheduled")
		return fmt.Errorf("coordinator: Pod %q has no NodeName; restore requires a scheduled pod", pod.Name)
	}
	if pod.Spec.NodeName != snap.Spec.Node {
		setSpanErr(span, "node mismatch")
		return fmt.Errorf("coordinator: snapshot lives on %q but Pod is on %q; restore must run on the snapshot's node",
			snap.Spec.Node, pod.Spec.NodeName)
	}

	na, dialErr := c.Dialer.Dial(ctx, pod.Spec.NodeName)
	if dialErr != nil {
		c.emit(sb, corev1.EventTypeWarning, EventReasonNodeAgentUnreachable, dialErr.Error())
		setSpanErr(span, dialErr.Error())
		return fmt.Errorf("coordinator: dial node-agent: %w", dialErr)
	}

	resp, rpcErr := na.RestoreSandbox(ctx, &setecgrpcv1.RestoreSandboxRequest{
		SnapshotId:       snap.Namespace + "-" + snap.Name,
		StorageRef:       snap.Spec.StorageRef,
		StorageBackend:   snap.Spec.StorageBackend,
		KataSocketTarget: c.socketForPod(pod),
	})
	if rpcErr != nil || (resp != nil && !resp.Success) {
		msg := errString(rpcErr, resp)
		c.emit(sb, corev1.EventTypeWarning, EventReasonSnapshotRestoreFailed, msg)
		setSpanErr(span, msg)
		c.recordDuration("restore", sb, time.Since(start))
		return fmt.Errorf("coordinator: RestoreSandbox RPC: %s", msg)
	}

	c.emit(sb, corev1.EventTypeNormal, EventReasonSnapshotRestoreStarted,
		fmt.Sprintf("restored sandbox from snapshot %q on node %q", snap.Name, pod.Spec.NodeName))
	c.recordDuration("restore", sb, time.Since(start))
	return nil
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
		SandboxId:        sb.Namespace + "/" + sb.Name,
		KataSocketTarget: c.socketForPod(pod),
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

	if snap.Spec.Node == "" {
		return errors.New("coordinator: Snapshot has no node; cannot delete without a routing target")
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
		SandboxId:        sb.Namespace + "/" + sb.Name,
		KataSocketTarget: c.socketForPod(pod),
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

// socketForPod renders the KataSocketPattern for the given Pod using
// the Pod UID (which Kata uses as the sandbox id). An empty UID
// returns an empty string so callers can detect the error.
func (c *Coordinator) socketForPod(pod *corev1.Pod) string {
	uid := string(pod.UID)
	if uid == "" {
		return ""
	}
	pattern := c.KataSocketPattern
	if pattern == "" {
		pattern = defaultKataSocketPattern
	}
	return fmt.Sprintf(pattern, uid)
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
