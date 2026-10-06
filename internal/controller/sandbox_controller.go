// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

// Package controller wires the Sandbox custom resource to a backing Pod via
// the controller-runtime reconciler pattern. This file is the only place in
// the operator that performs Kubernetes I/O — all transformation logic lives
// in the pure packages (internal/podspec, internal/status, internal/runtime)
// that the reconciler composes.
package controller

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"time"

	"github.com/go-logr/logr"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
	"github.com/zeroroot-ai/setec/internal/class"
	"github.com/zeroroot-ai/setec/internal/errwrap"
	"github.com/zeroroot-ai/setec/internal/limits"
	"github.com/zeroroot-ai/setec/internal/metrics"
	"github.com/zeroroot-ai/setec/internal/netpol"
	"github.com/zeroroot-ai/setec/internal/podspec"
	runtimepkg "github.com/zeroroot-ai/setec/internal/runtime"
	"github.com/zeroroot-ai/setec/internal/snapshot"
	"github.com/zeroroot-ai/setec/internal/status"
	"github.com/zeroroot-ai/setec/internal/tenancy"
	"github.com/zeroroot-ai/setec/internal/tracing"
)

const (
	// pendingRequeue is how long the reconciler waits before it checks a
	// pending Sandbox again: a missing tenant label, a class violation, or
	// a Snapshot that is not Ready. Once a minute keeps the API-server
	// load low.
	pendingRequeue = 60 * time.Second

	// networkPolicyReadBackRequeue is how long the reconciler waits
	// before retrying when a freshly-created NetworkPolicy is not yet
	// readable. Short, because the Pod is held back until it is.
	networkPolicyReadBackRequeue = 2 * time.Second

	// classNotFoundGrace bounds how long a Sandbox may sit Pending because
	// its SandboxClass does not resolve before the reconciler gives up and
	// fails it terminally (setec#299).
	//
	// A missing class is transient in exactly one window: the Sandbox and
	// its class were created together and the class has not yet been
	// observed by this controller's cache. Past that, the class was either
	// never created or has been deleted, and no amount of requeueing brings
	// it back — the Sandbox was previously requeued forever, accumulating
	// as Pending/ClassNotFound (10 on staging, oldest 23h) with no Pod, so
	// nothing was unschedulable, so no autoscaler ever reacted and the
	// objects simply piled up. A real stuck dispatch then looks exactly
	// like CI litter.
	//
	// Five minutes is far beyond any cache-sync window while still short
	// enough that an operator watching a launch sees the truth quickly.
	classNotFoundGrace = 5 * time.Minute

	// classNotFoundRequeue is the poll interval while inside the grace
	// window. Shorter than pendingRequeue so the terminal
	// transition lands promptly after the deadline rather than up to a
	// minute late.
	classNotFoundRequeue = 30 * time.Second

	// orphanedSandboxRetention bounds how long a Sandbox that failed
	// terminally on ClassNotFound is kept before the reconciler deletes it
	// (setec#299).
	//
	// Failing the object terminally stops it pretending to be schedulable,
	// but it does not stop the pile growing: the staging cluster still
	// accumulates one dead object per leaked class. Nothing else will ever
	// remove them — Kubernetes garbage collection needs an in-cluster owner,
	// and by definition this Sandbox has none: its class is gone, and the run
	// that dispatched it lives in gibson, outside the cluster. So the
	// reconciler owns the reap.
	//
	// The blast radius is deliberately the narrowest population that is
	// provably garbage: terminal, reason ClassNotFound, and the class still
	// unresolvable on the reconcile that reaps it. Such a Sandbox never had a
	// Pod and holds no result — there is nothing to lose by deleting it, and
	// the Warning Events explaining why it failed outlive it.
	//
	// One hour leaves a generous window to `kubectl describe` a genuine
	// misconfiguration before the object is collected. Terminal Sandboxes
	// that actually ran (Completed, or Failed for any other reason) are never
	// touched by this path: they carry a result their creator owns.
	orphanedSandboxRetention = time.Hour

	// ephemeralFinishedRetention bounds how long an ephemeral Sandbox that
	// reached a terminal phase (Completed or Failed) is kept before the
	// reconciler deletes it, honoring docs/design/lifecycles.md's "auto-destroy on exit" for
	// the run-to-completion lifecycle.
	//
	// An ephemeral Sandbox is one agent/tool action. Its creator lives in
	// gibson, outside the cluster, and drives it request/response: Launch,
	// then StreamLogs and/or Wait, then — on the happy path — Kill. Nothing
	// in the cluster owns it, so a creator that dies between Launch and Kill
	// (a crashed run, a dropped connection) used to leak the Sandbox, its
	// Pod, and the microVM forever: a terminal Sandbox with a stable Pod
	// generates no further watch events and was never revisited.
	//
	// The retention is a grace window, not an eviction: it starts when the
	// Sandbox goes terminal (status.LastTransitionTime), so the creator
	// keeps a bounded window to read the outcome (Wait) and drain the
	// captured output (StreamLogs, which serves a terminated container's
	// log, setec#263) before the object is collected. After it, the
	// reconciler deletes the Sandbox and owner-reference GC removes the Pod
	// and any NetworkPolicy — no lingering resources. Session Sandboxes are
	// never touched by this path: they end by explicit teardown (Kill) which
	// wipes the durable workspace via the finalizer.
	//
	// Ten minutes is far longer than the request/response turnaround of a
	// tool call, yet short enough that a leaked Sandbox does not hold a
	// microVM for long.
	ephemeralFinishedRetention = 10 * time.Minute

	// Event reasons. Kept as constants so tests and docs can reference them
	// by name rather than string-matching fragments of the message.
	eventReasonPodCreateFailed       = "PodCreateFailed"
	eventReasonPodCreated            = "PodCreated"
	eventReasonTimeout               = "TimeoutExceeded"
	eventReasonIdleTimeout           = "IdleTimeoutExceeded"
	eventReasonPauseTimeout          = "PauseTimeoutExceeded"
	eventReasonReconcileError        = "ReconcileError"
	eventReasonClassNotFound         = "ClassNotFound"
	eventReasonOrphanReaped          = "OrphanedSandboxReaped"
	eventReasonEphemeralReaped       = "EphemeralSandboxReaped"
	eventReasonConstraintViolated    = "ConstraintViolated"
	eventReasonTenantMissing         = "TenantLabelMissing"
	eventReasonNetworkPolicy         = "NetworkPolicyApplied"
	eventReasonNetworkPolicyPending  = "NetworkPolicyPending"
	eventReasonNamespaceBaseline     = "NamespaceBaselineApplied"
	eventReasonSnapshotUnavailable   = "SnapshotUnavailable"
	eventReasonSnapshotIncompatible  = "SnapshotIncompatible"
	eventReasonPaused                = "Paused"
	eventReasonResumed               = "Resumed"
	eventReasonSnapshotCreateStarted = "SnapshotCreateStarted"
	eventReasonSnapshotCreateFailed  = "SnapshotCreateFailed"
	eventReasonWorkspaceCreated      = "WorkspaceCreated"
	eventReasonWorkspaceDeleted      = "WorkspaceDeleted"
	eventReasonSessionVMRestart      = "SessionVMRestart"
	// eventReasonUnsupportedBackend: the class names a removed backend
	// (setec#198). The Sandbox fails with the reason; it never runs on
	// another isolation.
	eventReasonUnsupportedBackend = "UnsupportedBackend"

	// eventReasonInvariantGateViolation mirrors the coordinator's
	// typed reason (snapshot.EventReasonInvariantGateViolation): the
	// docs/design/isolation.md invariant gate refused a restore and the Pod holding
	// the unverified state is destroyed.
	eventReasonInvariantGateViolation = "InvariantGateViolation"

	// workspaceFinalizer guards session-Sandbox deletion so the durable
	// workspace PVC is wiped and deleted before the Sandbox object goes
	// away (docs/design/isolation.md invariant 3: one session, wiped at session end).
	// Ephemeral Sandboxes never carry it.
	workspaceFinalizer = "setec.zeroroot.ai/workspace-teardown"

	// workspaceTeardownRequeue is how long the teardown path waits
	// between checks that the workspace PVC deletion has been accepted.
	workspaceTeardownRequeue = 2 * time.Second

	// snapshotInFlightRequeue is how long the snapshot-create path waits
	// when a Snapshot with the target name is already in phase Creating.
	// A second CreateSnapshot RPC at the same name would race the first.
	snapshotInFlightRequeue = 5 * time.Second

	// defaultWorkspaceSize is the workspace PVC capacity used when a
	// session Sandbox does not declare spec.lifecycle.workspace.size.
	defaultWorkspaceSize = "10Gi"
)

// SandboxReconciler reconciles a Sandbox object. All fields are set at
// construction time in cmd/manager/main.go; the struct is immutable after
// SetupWithManager completes.
type SandboxReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder

	// APIReader reads from the API server, not from the cache. A step
	// that must run once, such as the restore of a session checkpoint,
	// checks the live object with it: the cache can still show the state
	// from before the step. Nil reads through Client.
	APIReader client.Reader

	// LauncherImage and DiskRepo serve the one backend
	// (docs/design/runtime.md). Each Sandbox gets a launcher Pod from this
	// image, and its machine boots the signed disk of its image digest from
	// DiskRepo.
	LauncherImage string
	DiskRepo      string
	DiskKeys      []string
	// WarmPoolNamespace is the namespace of the warm pool bases
	// (WarmPoolReconciler). Empty turns warm starts off.
	WarmPoolNamespace string
	// DiskBuilder runs setec-disk-builder before the first launcher Pod of
	// an image digest.
	DiskBuilder DiskBuilderConfig

	// --- Phase 2 optional dependencies ---
	//
	// All four of these may be nil. A nil value disables the
	// corresponding feature and the reconciler falls through to its
	// Phase 1 behavior. This is the back-compat contract called out in
	// design.md Requirement 8.

	// ClassResolver maps a Sandbox to its effective SandboxClass. When
	// nil, Sandboxes are reconciled without class constraint validation
	// (Phase 1 behavior).
	ClassResolver *class.Resolver

	// MetricsCollector records Prometheus metrics on phase transitions.
	// When nil, metrics recording is a no-op.
	MetricsCollector *metrics.Collectors

	// Tracer emits OTEL spans for the reconcile loop. When nil, no
	// spans are recorded.
	Tracer trace.Tracer

	// MultiTenancyEnabled, when true, requires Sandboxes' namespaces to
	// carry a tenant label. A missing tenant label is surfaced as a
	// ClassMissing Event and the Sandbox stays Pending.
	MultiTenancyEnabled bool

	// TenantLabelKey is the label key consulted when
	// MultiTenancyEnabled. Default "setec.zeroroot.ai/tenant".
	TenantLabelKey string

	// ClassNotFoundGrace bounds how long a Sandbox may sit Pending on an
	// unresolvable SandboxClass before it is failed terminally (setec#299).
	// Zero means classNotFoundGrace. Overridden in tests so the terminal
	// transition can be driven through a real reconcile without waiting.
	ClassNotFoundGrace time.Duration

	// OrphanRetention bounds how long a Sandbox that failed terminally on an
	// unresolvable SandboxClass is kept before it is deleted (setec#299).
	// Zero means orphanedSandboxRetention. Overridden in tests so the reap
	// can be driven through a real reconcile without waiting.
	OrphanRetention time.Duration

	// EphemeralRetention bounds how long an ephemeral Sandbox that reached a
	// terminal phase is kept before it is auto-destroyed (docs/design/lifecycles.md). Zero
	// means ephemeralFinishedRetention. Overridden in tests so the reap can
	// be driven through a real reconcile without waiting.
	EphemeralRetention time.Duration

	// --- Phase 3 optional dependencies ---
	//
	// Both fields may be nil. A nil Coordinator disables every Phase 3
	// reconcile branch (snapshot create, restore, pause/resume); the
	// reconciler then falls through to Phase 2 behavior unchanged. The
	// SnapshotReadyIndex (see SetupWithManager) is populated only when
	// Coordinator is wired.

	// Coordinator orchestrates the operator-side snapshot work.
	Coordinator *snapshot.Coordinator

	// NetPol is the cluster-wide egress posture every generated
	// NetworkPolicy is built from: the reserved address ranges no
	// Sandbox may reach, and the DNS resolvers it may query. The same
	// resolver list is written into each Pod's dnsConfig so the policy
	// and the Pod agree by construction.
	//
	// Set at construction time from the operator's --reserved-cidrs and
	// --sandbox-resolvers flags, which are validated at startup.
	NetPol netpol.Config

	// NamespaceBaselineDeny, when true, makes the reconciler ensure a
	// namespace-wide default-deny NetworkPolicy (podSelector: {}) in the
	// namespace of every Sandbox it reconciles, before that Sandbox's
	// own policy and therefore before its Pod.
	//
	// The per-Sandbox policies select on the setec.zeroroot.ai/sandbox
	// label, so they confine Pods the operator built. A Pod written into
	// the same namespace by any other route carries no such label, is
	// selected by no policy, and is consequently unrestricted. The
	// baseline removes that state for the whole namespace, which is the
	// one control here that does not depend on the workload labeling
	// itself correctly.
	//
	// It is set from --namespace-baseline-deny (default true). Turning it
	// off is only correct where Sandbox namespaces are shared with
	// workloads that need ordinary egress — a topology this operator
	// recommends against, because there is no label that separates the
	// two populations that an adversary could not also apply.
	NamespaceBaselineDeny bool
}

// RBAC markers. These are consumed by controller-gen to generate the
// ClusterRole at config/rbac/role.yaml. The markers live as a standalone
// comment block (not attached to a declaration) because controller-gen
// v0.20+ recognizes +kubebuilder:rbac markers at package level; binding
// them to a func's doc comment suppresses generation.
//
// +kubebuilder:rbac:groups=setec.zeroroot.ai,resources=sandboxes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=setec.zeroroot.ai,resources=sandboxes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=setec.zeroroot.ai,resources=sandboxes/finalizers,verbs=update
// +kubebuilder:rbac:groups=setec.zeroroot.ai,resources=sandboxclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=persistentvolumeclaims,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups=core,resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=core,resources=nodes,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=namespaces,verbs=get;list;watch
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete

// Reconcile drives a single Sandbox toward its desired state. The function is
// intentionally thin: the three pure packages own every non-trivial decision
// and Reconcile restricts itself to I/O, error handling, and idempotent
// status patching.
//
// Idempotency invariants (re-running Reconcile on a stable state is a no-op):
//   - Pod creation is guarded by a NotFound Get; the second call finds the
//     Pod and skips the Create path.
//   - Status patches are guarded by reflect.DeepEqual against the live
//     status; an equivalent derived status produces zero writes.
//   - Terminal Sandbox phases short-circuit Pod creation so a finished
//     Sandbox never spawns a replacement Pod.
//   - Timeout-triggered Pod deletion is guarded by DeletionTimestamp so a
//     Pod already being deleted is not re-deleted.
func (r *SandboxReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("sandbox", req.NamespacedName)

	// (1) Fetch the Sandbox. If it has been deleted the garbage collector
	// will clean up the owned Pod and NetworkPolicy via OwnerReference,
	// so we have nothing to do here.
	sb := &setecv1alpha1.Sandbox{}
	if err := r.Get(ctx, req.NamespacedName, sb); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("get Sandbox: %w", err)
	}

	// (1a) Session teardown. A Sandbox being deleted that still carries
	// the workspace finalizer must have its workspace PVC deleted before
	// the object is released (docs/design/isolation.md invariant 3). Ephemeral Sandboxes
	// never carry the finalizer and fall straight through to owner-ref GC.
	if !sb.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(sb, workspaceFinalizer) {
			return r.teardownWorkspace(ctx, logger, sb)
		}
		return ctrl.Result{}, nil
	}

	// (1b) Session Sandboxes acquire the workspace finalizer before any
	// other work so no window exists in which the PVC could outlive its
	// Sandbox unattended.
	if sb.Spec.IsSession() && !controllerutil.ContainsFinalizer(sb, workspaceFinalizer) {
		original := sb.DeepCopy()
		controllerutil.AddFinalizer(sb, workspaceFinalizer)
		if err := r.Patch(ctx, sb, client.MergeFrom(original)); err != nil {
			return ctrl.Result{}, fmt.Errorf("add workspace finalizer: %w", err)
		}
	}

	// (1c) docs/design/lifecycles.md "auto-destroy on exit": a terminal ephemeral Sandbox is
	// kept for a bounded window so its creator can read the outcome, then the
	// reconciler deletes it and owner-ref GC collects the Pod. This owns the
	// reconcile for a terminal ephemeral Sandbox — nothing below (class
	// resolution, Pod reconcile) applies once an ephemeral run has finished.
	if res, handled, err := r.reapExpiredEphemeral(ctx, logger, sb); handled || err != nil {
		return res, err
	}

	// (2) Phase 2: start OTEL span. The helper returns a no-op span when
	// Tracer is nil so the defer stays harmless on Phase 1 deployments.
	ctx, span := tracing.StartSandboxSpan(ctx, r.Tracer, sb)
	defer span.End()

	// Record the pre-reconcile status so metrics and events can detect
	// phase transitions at the end of the loop.
	prevPhase := sb.Status.Phase

	// (3) Phase 2: multi-tenancy enforcement. When enabled, the Sandbox's
	// namespace must carry the configured tenant label. Missing label is
	// surfaced as an Event and blocks reconciliation — we set Pending
	// with a clear reason and requeue.
	tenantID, tenantOK, tenantErr := r.resolveTenant(ctx, sb)
	if tenantErr != nil {
		return r.recordAndReturnErr(sb, eventReasonReconcileError, fmt.Errorf("resolve tenant: %w", tenantErr))
	}
	if r.MultiTenancyEnabled && !tenantOK {
		r.Recorder.Eventf(sb, nil, corev1.EventTypeWarning, eventReasonTenantMissing, actionResolveTenant,
			"namespace %q is missing tenant label %q", sb.Namespace, r.TenantLabelKey)
		if err := r.patchPendingStatus(ctx, sb, eventReasonTenantMissing); err != nil {
			return ctrl.Result{}, fmt.Errorf("patch TenantMissing status: %w", err)
		}
		setSpanError(span, "tenant label missing")
		return ctrl.Result{RequeueAfter: pendingRequeue}, nil
	}

	// (4) Phase 2: resolve the effective SandboxClass. A missing named
	// class is fatal; a missing default class with Phase 1-shaped Sandbox
	// (no class name, no multitenancy) is explicitly back-compat-tolerated.
	cls, classResolved, classErr := r.resolveClass(ctx, sb)
	if classErr != nil {
		setSpanError(span, classErr.Error())
		return r.handleUnresolvableClass(ctx, logger, sb, classErr)
	}

	// (5) Phase 2: validate the Sandbox against its class. Defense in
	// depth — the webhook should have caught any violation, but a
	// manually-created CR skipping admission must not silently produce a
	// Pod that violates the class ceiling.
	if classResolved {
		if violations := class.Validate(sb, cls); len(violations) > 0 {
			v := violations[0]
			r.Recorder.Eventf(sb, nil, corev1.EventTypeWarning, eventReasonConstraintViolated, actionValidateConstraints, "%s", v.String())
			if err := r.patchPendingStatus(ctx, sb, eventReasonConstraintViolated); err != nil {
				return ctrl.Result{}, fmt.Errorf("patch ConstraintViolated status: %w", err)
			}
			setSpanError(span, "constraint violated: "+v.String())
			return ctrl.Result{RequeueAfter: pendingRequeue}, nil
		}
	}

	// (5b) Phase 3: if snapshotRef is set, resolve and validate the
	// referenced Snapshot BEFORE the Pod is created. A missing snapshot
	// keeps the Sandbox Pending; an incompatible snapshot fails.
	pinnedNode, res, err := r.resolveSnapshotRef(ctx, sb, cls)
	if err != nil || res.RequeueAfter > 0 {
		return res, err
	}

	// (6) Compute the deterministic Pod name.
	podName := sb.Name + podspec.PodNameSuffix

	// (8) Fetch the owned Pod and reconcile it. If the Pod does not exist,
	// createOrSkip handles creation (or skips for terminal Sandboxes).
	// If the Pod already exists, reconcileExistingPod handles status,
	// networking, and lifecycle transitions.
	pod := &corev1.Pod{}
	err = r.Get(ctx, types.NamespacedName{Namespace: sb.Namespace, Name: podName}, pod)
	if apierrors.IsNotFound(err) {
		return r.handleMissingPod(ctx, logger, sb, cls, pinnedNode)
	}
	if err != nil {
		return r.recordAndReturnErr(sb, eventReasonReconcileError, fmt.Errorf("get Pod %q: %w", podName, err))
	}
	return r.reconcileExistingPod(ctx, span, sb, cls, pod, prevPhase, tenantID)
}

// handleUnresolvableClass owns the whole lifecycle of a Sandbox whose
// SandboxClass does not resolve (setec#299). It is a three-stage ramp, and
// each stage exists because the previous one alone leaves objects piling up:
//
//  1. Inside classNotFoundGrace the condition is genuinely transient — the
//     Sandbox and its class were created together and this controller's cache
//     has not observed the class yet. Stay Pending and re-check.
//  2. Past the grace period the class was never created, or (the staging
//     case) it was a per-run class that has since been deleted. No amount of
//     requeueing brings it back, so the Sandbox fails terminally instead of
//     pretending to be schedulable forever.
//  3. Past a further orphanedSandboxRetention the object is deleted. Terminal
//     is not enough on its own: without a reap the cluster still accumulates
//     one dead Sandbox per leaked class, which is the pile the issue reports.
//     Kubernetes garbage collection cannot do this — it needs an in-cluster
//     owner, and an orphan by definition has none (its class is gone, and the
//     run that dispatched it lives in gibson, out of cluster).
//
// A Sandbox that already reached a terminal phase for some OTHER reason is
// left completely alone: it ran, it holds a result, and its class going away
// afterwards says nothing about it. That guard also closes a rollback bug —
// patchPendingStatus writes Pending unconditionally, so a Completed Sandbox
// whose class was deleted used to be dragged back to Pending/ClassNotFound,
// violating the terminal-phase invariant documented on SandboxStatus.Phase.
func (r *SandboxReconciler) handleUnresolvableClass(
	ctx context.Context,
	logger logr.Logger,
	sb *setecv1alpha1.Sandbox,
	classErr error,
) (ctrl.Result, error) {
	if isTerminalPhase(sb.Status.Phase) && sb.Status.Reason != eventReasonClassNotFound {
		logger.V(1).Info("Sandbox already terminal; ignoring unresolvable class",
			"phase", sb.Status.Phase, "reason", sb.Status.Reason)
		return ctrl.Result{}, nil
	}

	r.Recorder.Eventf(sb, nil, corev1.EventTypeWarning, eventReasonClassNotFound, actionResolveSandboxClass, "%s", classErr.Error())

	now := time.Now()
	grace := r.classNotFoundGraceOrDefault()
	if !classNotFoundExpired(sb, now, grace) {
		if err := r.patchPendingStatus(ctx, sb, eventReasonClassNotFound); err != nil {
			return ctrl.Result{}, fmt.Errorf("patch ClassNotFound status: %w", err)
		}
		// Requeue past the grace deadline so the terminal transition happens
		// even if nothing else touches this Sandbox again.
		return ctrl.Result{RequeueAfter: classNotFoundRequeue}, nil
	}

	retention := r.orphanRetentionOrDefault()
	due, remaining := orphanReapDue(sb, now, grace, retention)
	if due {
		r.Recorder.Eventf(sb, nil, corev1.EventTypeWarning, eventReasonOrphanReaped, actionReapOrphanedSandbox,
			"SandboxClass never resolved; deleting the orphaned Sandbox %s after it failed", retention)
		logger.Info("reaping orphaned Sandbox whose SandboxClass never resolved",
			"class", sb.Spec.SandboxClassName, "retention", retention)
		if err := r.Delete(ctx, sb); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("delete orphaned Sandbox: %w", err)
		}
		return ctrl.Result{}, nil
	}

	if !isTerminalPhase(sb.Status.Phase) {
		r.Recorder.Eventf(sb, nil, corev1.EventTypeWarning, eventReasonClassNotFound, actionResolveSandboxClass,
			"SandboxClass did not resolve within %s; failing terminally", grace)
	}
	if err := r.patchFailedStatus(ctx, sb, eventReasonClassNotFound); err != nil {
		return ctrl.Result{}, fmt.Errorf("patch ClassNotFound terminal status: %w", err)
	}
	// Come back exactly when the object is due to be reaped. Without this the
	// Sandbox would be terminal but immortal: nothing else generates an event
	// for a Sandbox that has no Pod.
	return ctrl.Result{RequeueAfter: remaining}, nil
}

// handleMissingPod is called when the owned Pod does not yet exist. It skips
// creation for terminal Sandboxes and otherwise selects a runtime, applies
// the NetworkPolicy, and only then creates the Pod.
//
// The order matters and is the point of this function: a Pod that exists
// before its policy does can send traffic in the window between the two
// writes. NetworkPolicy selects by label, so the policy is legal to write
// while its subject does not yet exist. If the policy cannot be applied the
// Pod is not created at all — the Sandbox stalls Pending and says why,
// rather than running unpoliced.
func (r *SandboxReconciler) handleMissingPod(
	ctx context.Context,
	logger logr.Logger,
	sb *setecv1alpha1.Sandbox,
	cls *setecv1alpha1.SandboxClass,
	pinnedNode string,
) (ctrl.Result, error) {
	if isTerminalPhase(sb.Status.Phase) {
		logger.V(1).Info("Sandbox is terminal; not recreating Pod", "phase", sb.Status.Phase)
		return ctrl.Result{}, nil
	}
	// Session checkpoints (setec#194): a Suspended session stays
	// suspended — no Pod — until its resume condition holds (fresh
	// activity for an idle suspend, desiredState=Running for a user
	// suspend, immediately after a drain). Resuming falls through to
	// ordinary Pod creation, deliberately unpinned: the checkpoint
	// store and the session KEK are cluster-scoped, so the scheduler
	// is free to pick a different node than the one that wrote the
	// checkpoint.
	if sb.Spec.IsSession() && sb.Status.Phase == setecv1alpha1.SandboxPhaseSuspended {
		if !suspendedSandboxAction(sb) {
			return r.recycleIfExpired(ctx, sb, cls)
		}
		logger.Info("resuming suspended session", "reason", sb.Status.Reason)
	}
	if policy := sessionCheckpointPolicy(sb, cls); policy != nil {
		// The per-session KEK must exist before the first checkpoint
		// could ever be taken, and a VM that vanished while a live
		// checkpoint exists must restore from it once the fresh VM
		// runs.
		if err := r.ensureSessionKEK(ctx, sb); err != nil {
			return r.recordAndReturnErr(sb, eventReasonReconcileError, fmt.Errorf("ensure session KEK: %w", err))
		}
		if ck := sb.Status.Checkpoint; ck != nil && ck.Ref != "" && !ck.PendingRestore {
			original := sb.DeepCopy()
			sb.Status.Checkpoint.PendingRestore = true
			if err := r.Status().Patch(ctx, sb, client.MergeFrom(original)); err != nil {
				return ctrl.Result{}, fmt.Errorf("mark checkpoint pending restore: %w", err)
			}
		}
	}
	if done, err := r.checkBackend(ctx, sb, cls); done {
		return ctrl.Result{}, err
	}
	// Namespace baseline first, then the per-Sandbox policy, then the
	// Pod. The baseline is ordered ahead of both because it is the only
	// one of the three that covers Pods this operator did not create; a
	// failure here is fatal to the reconcile for the same reason the
	// per-Sandbox failure is.
	if err := r.ensureNamespaceBaseline(ctx, logger, sb); err != nil {
		return r.recordAndReturnErr(sb, eventReasonReconcileError, err)
	}
	// Policy first. A failure here is fatal to this reconcile: returning
	// before createPod is what guarantees no Sandbox ever runs without a
	// NetworkPolicy in place.
	if res, err := r.applyNetworkPolicy(ctx, sb, cls); err != nil || res.RequeueAfter > 0 {
		if err != nil {
			r.Recorder.Eventf(sb, nil, corev1.EventTypeWarning, eventReasonNetworkPolicyPending,
				actionApplyNetworkPolicy,
				"Pod creation deferred: NetworkPolicy for Sandbox is not in place yet")
		}
		return res, err
	}
	// Session lifecycle: the durable workspace PVC must exist before the
	// Pod that mounts it. Like the NetworkPolicy, a failure here defers
	// Pod creation rather than producing a Pod without its workspace.
	if sb.Spec.IsSession() {
		if err := r.ensureWorkspacePVC(ctx, logger, sb); err != nil {
			return r.recordAndReturnErr(sb, eventReasonReconcileError, fmt.Errorf("ensure workspace PVC: %w", err))
		}
	}
	return r.createPod(ctx, sb, cls, pinnedNode)
}

// newWorkspacePVC builds the session Sandbox's durable workspace claim
// object (docs/design/storage.md: a portable RWO CSI volume; any CSI driver works).
// It is a pure function with no API calls, so the PVC shape can be
// asserted without a fake or live apiserver.
//
// The claim has volumeMode: Block (setec#91). Firecracker has no
// virtio-fs, and a raw block device is the one volume type that
// Firecracker can attach to the guest.
func newWorkspacePVC(sb *setecv1alpha1.Sandbox) *corev1.PersistentVolumeClaim {
	name := podspec.WorkspacePVCName(sb.Name)

	size := resource.MustParse(defaultWorkspaceSize)
	var storageClassName *string
	if sb.Spec.Lifecycle != nil && sb.Spec.Lifecycle.Workspace != nil {
		ws := sb.Spec.Lifecycle.Workspace
		if ws.Size != nil {
			size = *ws.Size
		}
		storageClassName = ws.StorageClassName
	}

	// Firecracker has no virtio-fs: the workspace reaches the machine as a
	// block device.
	block := corev1.PersistentVolumeBlock
	volumeMode := &block

	return &corev1.PersistentVolumeClaim{
		Name:      name,
		Namespace: sb.Namespace,
		Labels:    map[string]string{podspec.SandboxLabelKey: sb.Name},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: size},
			},
			StorageClassName: storageClassName,
			VolumeMode:       volumeMode,
		},
	}
}

// ensureWorkspacePVC creates the session Sandbox's durable workspace
// claim when it does not exist yet. The claim is owner-referenced to
// the Sandbox as defense in depth, but its authoritative teardown is
// the workspace finalizer, which wipes it deterministically at session
// end.
//
// A workspace PVC found mid-deletion is an error, not a wait: per
// docs/design/isolation.md invariant 3 a workspace serves exactly one session, so a
// name collision with a dying claim must fail loudly instead of
// adopting or racing it.
func (r *SandboxReconciler) ensureWorkspacePVC(
	ctx context.Context,
	logger logr.Logger,
	sb *setecv1alpha1.Sandbox,
) error {
	name := podspec.WorkspacePVCName(sb.Name)
	existing := &corev1.PersistentVolumeClaim{}
	err := r.Get(ctx, types.NamespacedName{Namespace: sb.Namespace, Name: name}, existing)
	if err == nil {
		if !existing.DeletionTimestamp.IsZero() {
			return fmt.Errorf("workspace PVC %q is terminating; a session workspace is never reused", name)
		}
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get workspace PVC %q: %w", name, err)
	}

	pvc := newWorkspacePVC(sb)
	if err := controllerutil.SetControllerReference(sb, pvc, r.Scheme); err != nil {
		return fmt.Errorf("set owner on workspace PVC: %w", err)
	}
	if err := r.Create(ctx, pvc); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create workspace PVC %q: %w", name, err)
	}
	logger.Info("created session workspace PVC", "pvc", name, "volumeMode", pvc.Spec.VolumeMode)
	r.Recorder.Eventf(sb, nil, corev1.EventTypeNormal, eventReasonWorkspaceCreated, actionManageWorkspace,
		"Created workspace PVC %q for session Sandbox", name)
	return nil
}

// teardownWorkspace runs while a session Sandbox is being deleted and
// still carries the workspace finalizer. It deletes the backing Pod
// (a mounted claim cannot finish deleting under pvc-protection), then
// deletes the workspace PVC, and releases the finalizer once the claim
// is gone or its deletion has been accepted by the API server. A
// Terminating claim can never bind again, so no cross-session reuse is
// possible from that point; the CSI driver destroys the volume — and
// with it every byte of session data — as soon as the Pod releases it
// (docs/design/isolation.md invariant 3).
func (r *SandboxReconciler) teardownWorkspace(
	ctx context.Context,
	logger logr.Logger,
	sb *setecv1alpha1.Sandbox,
) (ctrl.Result, error) {
	// (a0) Session-checkpoint teardown (setec#194): delete the
	// per-session KEK Secret — cryptographically erasing every
	// checkpoint sealed under it — then best-effort delete the stored
	// checkpoint objects. Idempotent; never blocks teardown.
	r.teardownSessionCheckpoint(ctx, logger, sb)

	// (a) Delete the backing Pod first so the claim can unmount.
	pod := &corev1.Pod{}
	podKey := types.NamespacedName{Namespace: sb.Namespace, Name: sb.Name + podspec.PodNameSuffix}
	switch err := r.Get(ctx, podKey, pod); {
	case err == nil:
		if pod.DeletionTimestamp.IsZero() {
			if err := r.Delete(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
				return ctrl.Result{}, fmt.Errorf("delete session Pod during teardown: %w", err)
			}
		}
	case !apierrors.IsNotFound(err):
		return ctrl.Result{}, fmt.Errorf("get session Pod during teardown: %w", err)
	}

	// (b) Delete the workspace PVC.
	pvcName := podspec.WorkspacePVCName(sb.Name)
	pvc := &corev1.PersistentVolumeClaim{}
	err := r.Get(ctx, types.NamespacedName{Namespace: sb.Namespace, Name: pvcName}, pvc)
	switch {
	case apierrors.IsNotFound(err):
		// Already gone — release the finalizer.
	case err != nil:
		return ctrl.Result{}, fmt.Errorf("get workspace PVC during teardown: %w", err)
	case pvc.DeletionTimestamp.IsZero():
		if err := r.Delete(ctx, pvc); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("delete workspace PVC %q: %w", pvcName, err)
		}
		r.Recorder.Eventf(sb, nil, corev1.EventTypeNormal, eventReasonWorkspaceDeleted, actionManageWorkspace,
			"Deleted workspace PVC %q at session end", pvcName)
		return ctrl.Result{RequeueAfter: workspaceTeardownRequeue}, nil
	default:
		// Deletion accepted; pvc-protection completes it once the Pod
		// releases the mount. Fall through to release the finalizer.
	}

	original := sb.DeepCopy()
	controllerutil.RemoveFinalizer(sb, workspaceFinalizer)
	if err := r.Patch(ctx, sb, client.MergeFrom(original)); err != nil {
		return ctrl.Result{}, fmt.Errorf("remove workspace finalizer: %w", err)
	}
	logger.Info("session workspace torn down", "pvc", pvcName)
	return ctrl.Result{}, nil
}

// reconcileExistingPod handles the case where the owned Pod already exists:
// it ensures the NetworkPolicy, derives and patches Sandbox status, records
// metrics/span, and handles lifecycle transitions (Phase 3) and timeout deletes.
func (r *SandboxReconciler) reconcileExistingPod(
	ctx context.Context,
	span trace.Span,
	sb *setecv1alpha1.Sandbox,
	cls *setecv1alpha1.SandboxClass,
	pod *corev1.Pod,
	prevPhase setecv1alpha1.SandboxPhase,
	tenantID string,
) (ctrl.Result, error) {
	// (9) Ensure NetworkPolicy (idempotent). The namespace baseline is
	// re-asserted on every pass as well, so a policy deleted or widened
	// out of band is restored on the next reconcile rather than staying
	// gone until the next Sandbox is created.
	if err := r.ensureNamespaceBaseline(ctx, log.FromContext(ctx), sb); err != nil {
		return ctrl.Result{}, err
	}
	if _, err := r.applyNetworkPolicy(ctx, sb, cls); err != nil {
		return ctrl.Result{}, err
	}

	// (9a) A just-suspended session's Pod is still terminating: hold
	// the Suspended phase steady until the Pod is gone (setec#194).
	// Deriving from the dying Pod here would flip the phase back to
	// Running and re-trigger the suspend.
	// The Pod may still show no deletion in the cache while it already
	// carries the suspend mark: deriving from it then flipped the phase to
	// Running, the next reconcile made a new Pod, and a session asked to
	// stay suspended came back at once (found in setec#193).
	if sb.Spec.IsSession() && sb.Status.Phase == setecv1alpha1.SandboxPhaseSuspended &&
		(!pod.DeletionTimestamp.IsZero() || pod.Annotations[annotationSuspendedPod] != "") {
		return ctrl.Result{RequeueAfter: suspendWaitRequeue}, nil
	}

	// (10) Derive status and patch when changed.
	desired, stop, err := r.deriveStatus(ctx, sb, cls, pod)
	if stop != nil || err != nil {
		return ctrlResult(stop), err
	}

	// (11) Record phase transition metrics and span status.
	r.recordTransition(sb, cls, prevPhase, desired, pod, tenantID)

	var checkpointRequeue time.Duration

	// (11c) Session-checkpoint state machine (setec#194): transparent
	// resume-from-checkpoint on the fresh VM's Running edge,
	// checkpoint-on-drain when the node is cordoned or the Pod
	// evicted, suspend on explicit request or on the class idle
	// deadline, and periodic checkpoints while Running.
	if sb.Spec.IsSession() {
		res, handled, err := r.reconcileSessionCheckpoint(ctx, log.FromContext(ctx), sb, cls, pod, desired)
		if err != nil || handled {
			return res, err
		}
		if res.RequeueAfter > 0 {
			checkpointRequeue = res.RequeueAfter
		}
	}

	// (11b) Session lifecycle: a dead VM is replaced, not mourned. When
	// the Pod reached a terminal phase but the Sandbox itself is not
	// terminal (Timeout still wins and stays terminal), delete the Pod;
	// the next reconcile recreates it and the fresh VM re-mounts the
	// durable workspace PVC, so session data survives the restart.
	if sb.Spec.IsSession() && !isTerminalPhase(desired.Phase) &&
		(pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed) &&
		pod.DeletionTimestamp.IsZero() {
		return r.restartExitedSession(ctx, sb, cls, pod)
	}

	// (11a) Phase 3: pause/resume lifecycle.
	if r.Coordinator != nil {
		if res, err := r.reconcilePhase3Lifecycle(ctx, sb, desired); err != nil || res.RequeueAfter > 0 {
			return res, err
		}
	}

	// (12) Delete the Pod of a Sandbox a wall-clock policy just failed —
	// the lifecycle timeout or the session idle eviction (guard against
	// repeated deletes). The terminal Failed phase is already persisted,
	// so the session-restart branch (11b) cannot resurrect the VM.
	if desired.Phase == setecv1alpha1.SandboxPhaseFailed && pod.DeletionTimestamp.IsZero() {
		if res, err := r.deleteFailedPod(ctx, sb, pod, desired.Reason); err != nil {
			return res, err
		}
	}

	span.SetAttributes(attribute.String("setec.sandbox.phase", string(desired.Phase)))
	if desired.Phase == setecv1alpha1.SandboxPhaseFailed {
		span.SetStatus(codes.Error, desired.Reason)
	}

	// (13) A policy built from resolved host names is only as accurate as
	// its last lookup, so a Sandbox that has one needs to come back
	// round. Without this the rules would be frozen at the DNS answer
	// that happened to be current when the Pod was created, and a
	// destination that moved would keep its old addresses allowed and its
	// new ones denied until some unrelated event triggered a reconcile
	// (setec#130).
	//
	// Only this path requeues. The creation path also applies the policy,
	// but there RequeueAfter means "do not create the Pod yet", so
	// reusing it for a refresh would defer the launch instead.
	return r.requeueAfter(sb, cls, desired, checkpointRequeue), nil
}

// deriveStatus derives the status of the Sandbox from its Pod and its
// policies, and patches it when it changed (steps 10 to 10c). A non-nil
// result ends the reconcile.
func (r *SandboxReconciler) deriveStatus(
	ctx context.Context, sb *setecv1alpha1.Sandbox, cls *setecv1alpha1.SandboxClass, pod *corev1.Pod,
) (setecv1alpha1.SandboxStatus, *ctrl.Result, error) {
	now := time.Now()
	desired := status.Derive(sb, pod, now)

	// (10a) A launcher Sandbox with a snapshotRef loads the snapshot
	// before it is Running (setec#105).
	desired = holdUntilRestored(sb, r.maybeRestoreLauncher(ctx, sb, pod, desired))

	// (10b) Session idle eviction (docs/design/lifecycles.md), layered on the derived
	// status: a Running session past its per-SandboxClass idle
	// deadline — no Attach and no client-stream heartbeat within
	// spec.sessionIdleTimeout — fails with reason IdleTimeout. Active
	// sessions keep their last-activity annotation fresh, so they are
	// never idle-reaped.
	desired = status.ApplySessionIdlePolicy(sb, cls, desired, now)

	// (10c) Pause-duration cap (setec#202), same layering: a Paused
	// Sandbox past the class maxPauseDuration fails with reason
	// PauseTimeoutExceeded — a paused microVM holds its full memory
	// reservation, and the class cap bounds that residency. When the
	// class enables sessionCheckpoint the suspend machinery (11c) owns
	// the deadline for sessions instead, so the policy passes through.
	desired = status.ApplyPausePolicy(sb, cls, desired, now)
	if !statusEqual(sb.Status, desired) {
		original := sb.DeepCopy()
		sb.Status = desired
		if err := r.Status().Patch(ctx, sb, client.MergeFrom(original)); err != nil {
			res, rerr := r.recordAndReturnErr(sb, eventReasonReconcileError, fmt.Errorf("patch Sandbox status: %w", err))
			return desired, &res, rerr
		}
	}
	return desired, nil, nil
}

// restartExitedSession deletes the exited Pod of a session, so the next
// reconcile makes a new VM against the durable workspace (step 11b). A
// session with checkpoints and no live checkpoint records the degraded
// recovery first.
func (r *SandboxReconciler) restartExitedSession(
	ctx context.Context, sb *setecv1alpha1.Sandbox, cls *setecv1alpha1.SandboxClass, pod *corev1.Pod,
) (ctrl.Result, error) {
	r.Recorder.Eventf(sb, nil, corev1.EventTypeNormal, eventReasonSessionVMRestart, actionRestartSessionVM,
		"Session VM exited (pod phase %s); restarting against durable workspace", pod.Status.Phase)
	// Degraded-recovery visibility (setec#194): a checkpoint-enabled
	// session losing its VM with NO live checkpoint can only restart
	// from the durable workspace — surface that as the distinct
	// condition before the replacement VM comes up. (With a live
	// checkpoint the replacement restores from it instead, marked
	// PendingRestore by handleMissingPod.)
	if policy := sessionCheckpointPolicy(sb, cls); policy != nil {
		if ck := sb.Status.Checkpoint; ck == nil || ck.Ref == "" {
			r.Recorder.Eventf(sb, nil, corev1.EventTypeWarning, eventReasonRestartedFromWorkspace, actionManageCheckpoint,
				"Session VM lost with no checkpoint; restarting from durable workspace — no data lost, process state gone")
			original := sb.DeepCopy()
			if sb.Status.Checkpoint == nil {
				sb.Status.Checkpoint = &setecv1alpha1.SandboxCheckpointStatus{Backend: policy.CheckpointBackend()}
			}
			sb.Status.Checkpoint.RecordRecovery(setecv1alpha1.SessionRecoveryRestartedFromWorkspace, metav1.Now(), nil)
			if perr := r.Status().Patch(ctx, sb, client.MergeFrom(original)); perr != nil {
				return ctrl.Result{}, fmt.Errorf("stamp degraded recovery: %w", perr)
			}
		}
	}
	if err := r.Delete(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
		return r.recordAndReturnErr(sb, eventReasonReconcileError, fmt.Errorf("delete exited session Pod: %w", err))
	}
	return ctrl.Result{}, nil
}

// failedPodDelete is the event and the error text of the Pod deletion
// after a policy failed a Sandbox (step 12).
type failedPodDelete struct {
	reason, action, note, errText string
}

// failedPodDeletes maps the reason of a failure to its Pod deletion.
var failedPodDeletes = map[string]failedPodDelete{
	status.ReasonTimeout: {eventReasonTimeout, actionEnforceTimeout,
		"Sandbox exceeded lifecycle.timeout; deleting Pod %q", "delete Pod after timeout"},
	status.ReasonIdleTimeout: {eventReasonIdleTimeout, actionEnforceIdleTimeout,
		"Session idle beyond the class sessionIdleTimeout; deleting Pod %q", "delete Pod after idle eviction"},
	status.ReasonPauseTimeout: {eventReasonPauseTimeout, actionEnforcePauseTimeout,
		"Sandbox paused beyond the class maxPauseDuration; deleting Pod %q", "delete Pod after pause timeout"},
	status.ReasonInvariantGateViolation: {eventReasonInvariantGateViolation, actionEnforceInvariantGate,
		"docs/design/isolation.md invariant gate refused the restore; destroying Pod %q (unverified restored state is never served)",
		"delete Pod after invariant-gate refusal"},
}

// deleteFailedPod deletes the Pod of a Sandbox that a wall-clock policy or
// the invariant gate just failed (step 12). Another reason keeps the Pod.
func (r *SandboxReconciler) deleteFailedPod(
	ctx context.Context, sb *setecv1alpha1.Sandbox, pod *corev1.Pod, reason string,
) (ctrl.Result, error) {
	d, ok := failedPodDeletes[reason]
	if !ok {
		return ctrl.Result{}, nil
	}
	r.Recorder.Eventf(sb, nil, corev1.EventTypeWarning, d.reason, d.action, d.note, pod.Name)
	if err := r.Delete(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
		return r.recordAndReturnErr(sb, eventReasonReconcileError, fmt.Errorf("%s: %w", d.errText, err))
	}
	return ctrl.Result{}, nil
}

// requeueAfter is the result of a reconcile of an existing Pod: the
// earliest of the DNS refresh, the next checkpoint and the next lifecycle
// deadline (steps 13 and 14).
func (r *SandboxReconciler) requeueAfter(
	sb *setecv1alpha1.Sandbox, cls *setecv1alpha1.SandboxClass,
	desired setecv1alpha1.SandboxStatus, checkpointRequeue time.Duration,
) ctrl.Result {
	result := ctrl.Result{}
	if netpol.DependsOnDNS(sb, cls) {
		result.RequeueAfter = r.NetPol.EffectiveRefreshInterval()
	}
	if checkpointRequeue > 0 && (result.RequeueAfter == 0 || checkpointRequeue < result.RequeueAfter) {
		result.RequeueAfter = checkpointRequeue
	}

	// (14) Deadline-driven requeue. The lifecycle timeout and the
	// session idle policy fire on the wall clock, not on cluster events:
	// a quietly sleeping Pod emits nothing, so without an explicit
	// requeue a Running Sandbox would only meet its deadline on the next
	// unrelated reconcile. Come back exactly when the earliest deadline
	// is due (whichever of the two is sooner also wins over the slower
	// DNS-refresh cadence).
	if after, ok := nextLifecycleDeadline(sb, cls, desired, time.Now()); ok &&
		(result.RequeueAfter == 0 || after < result.RequeueAfter) {
		result.RequeueAfter = after
	}
	return result
}

// nextLifecycleDeadline returns how long until the earliest wall-clock
// deadline of a Running or Paused Sandbox — the lifecycle timeout and,
// for sessions, the class idle-eviction deadline while Running; the
// class maxPauseDuration deadline while Paused — and whether any
// deadline is pending at all. The result is floored at one second so a
// deadline that passed while the status patch was in flight cannot
// produce a hot requeue loop.
func nextLifecycleDeadline(
	sb *setecv1alpha1.Sandbox,
	cls *setecv1alpha1.SandboxClass,
	st setecv1alpha1.SandboxStatus,
	now time.Time,
) (time.Duration, bool) {
	var earliest time.Time
	switch st.Phase {
	case setecv1alpha1.SandboxPhaseRunning:
		if st.StartedAt != nil && sb.Spec.Lifecycle != nil &&
			sb.Spec.Lifecycle.Timeout != nil && sb.Spec.Lifecycle.Timeout.Duration > 0 {
			earliest = st.StartedAt.Add(sb.Spec.Lifecycle.Timeout.Duration)
		}
		if d, ok := status.SessionIdleDeadline(sb, cls); ok &&
			(earliest.IsZero() || d.Before(earliest)) {
			earliest = d
		}
	case setecv1alpha1.SandboxPhasePaused:
		// A paused microVM emits no events either; only this requeue
		// delivers the reconcile that enforces maxPauseDuration —
		// whether that reconcile hard-fails the Sandbox (10c) or
		// suspends a checkpoint-enabled session (11c).
		if d, ok := status.PauseDeadline(st, cls); ok {
			earliest = d
		}
	case setecv1alpha1.SandboxPhasePending, setecv1alpha1.SandboxPhaseCompleted,
		setecv1alpha1.SandboxPhaseFailed, setecv1alpha1.SandboxPhaseSnapshotting,
		setecv1alpha1.SandboxPhaseRestoring, setecv1alpha1.SandboxPhaseSuspended:
		// No lifecycle deadline runs in these phases.
	}
	if earliest.IsZero() {
		return 0, false
	}
	after := max(earliest.Sub(now), time.Second)
	return after, true
}

// resolveSnapshotRef resolves and validates the Snapshot referenced by
// sb.Spec.SnapshotRef when set. Returns the pinned node name from the snapshot,
// or a non-zero RequeueAfter result when the snapshot is unavailable or
// incompatible. Returns an empty node name and zero result when no snapshotRef
// is set.
func (r *SandboxReconciler) resolveSnapshotRef(
	ctx context.Context,
	sb *setecv1alpha1.Sandbox,
	cls *setecv1alpha1.SandboxClass,
) (pinnedNode string, result ctrl.Result, err error) {
	if sb.Spec.SnapshotRef == nil || sb.Spec.SnapshotRef.Name == "" {
		return "", ctrl.Result{}, nil
	}
	snap := &setecv1alpha1.Snapshot{}
	getErr := r.Get(ctx, types.NamespacedName{Namespace: sb.Namespace, Name: sb.Spec.SnapshotRef.Name}, snap)
	switch {
	case apierrors.IsNotFound(getErr):
		r.Recorder.Eventf(sb, nil, corev1.EventTypeWarning, eventReasonSnapshotUnavailable, actionResolveSnapshot,
			"Snapshot %q not found in namespace %q", sb.Spec.SnapshotRef.Name, sb.Namespace)
		if perr := r.patchPendingStatus(ctx, sb, eventReasonSnapshotUnavailable); perr != nil {
			return "", ctrl.Result{}, fmt.Errorf("patch SnapshotUnavailable status: %w", perr)
		}
		return "", ctrl.Result{RequeueAfter: pendingRequeue}, nil
	case getErr != nil:
		res, rerr := r.recordAndReturnErr(sb, eventReasonReconcileError, fmt.Errorf("get Snapshot: %w", getErr))
		return "", res, rerr
	}
	if snap.Status.Phase != setecv1alpha1.SnapshotPhaseReady {
		if perr := r.patchPendingStatus(ctx, sb, eventReasonSnapshotUnavailable); perr != nil {
			return "", ctrl.Result{}, fmt.Errorf("patch Pending(SnapshotUnavailable): %w", perr)
		}
		return "", ctrl.Result{RequeueAfter: pendingRequeue}, nil
	}
	if snapViolations := snapshot.Validate(sb, snap, cls); len(snapViolations) > 0 {
		sv := snapViolations[0]
		r.Recorder.Eventf(sb, nil, corev1.EventTypeWarning, eventReasonSnapshotIncompatible, actionResolveSnapshot, "%s", sv.String())
		if perr := r.patchPendingStatus(ctx, sb, eventReasonSnapshotIncompatible); perr != nil {
			return "", ctrl.Result{}, fmt.Errorf("patch SnapshotIncompatible status: %w", perr)
		}
		return "", ctrl.Result{RequeueAfter: pendingRequeue}, nil
	}
	if !isLocalSnapshot(snap) {
		// The store serves the snapshot on any node of the class.
		return "", ctrl.Result{}, nil
	}
	return snap.Spec.Node, ctrl.Result{}, nil
}

// checkBackend holds the Sandbox to the one backend (setec#198). A class
// that names a removed backend fails the Sandbox with the reason, so a
// tenant never runs on an isolation that the class did not ask for. A
// Sandbox that passes records the launcher in status.runtime.chosen.
// done is true when the reconcile ends here.
func (r *SandboxReconciler) checkBackend(
	ctx context.Context, sb *setecv1alpha1.Sandbox, cls *setecv1alpha1.SandboxClass,
) (done bool, err error) {
	if cls != nil && cls.Spec.Runtime != nil {
		if verr := runtimepkg.ValidateBackend(cls.Spec.Runtime.Backend); verr != nil {
			r.Recorder.Eventf(sb, nil, corev1.EventTypeWarning, eventReasonUnsupportedBackend, actionResolveRuntime,
				"SandboxClass %q: %v", cls.Name, verr)
			original := sb.DeepCopy()
			sb.Status.Phase = setecv1alpha1.SandboxPhaseFailed
			sb.Status.Reason = eventReasonUnsupportedBackend
			if perr := r.Status().Patch(ctx, sb, client.MergeFrom(original)); perr != nil {
				return true, fmt.Errorf("patch Failed(UnsupportedBackend) status: %w", perr)
			}
			return true, nil
		}
	}
	if sb.Status.Runtime == nil || sb.Status.Runtime.Chosen != runtimepkg.BackendLauncher {
		original := sb.DeepCopy()
		sb.Status.Runtime = &setecv1alpha1.SandboxRuntimeStatus{Chosen: runtimepkg.BackendLauncher}
		if perr := r.Status().Patch(ctx, sb, client.MergeFrom(original)); perr != nil {
			// Not fatal: the status records the backend on a later reconcile.
			log.FromContext(ctx).Info("record status.runtime.chosen", "error", perr.Error())
		}
	}
	return false, nil
}

// resolveTenant returns the tenant ID of the Sandbox's namespace (when
// multi-tenancy is enabled). ok is false when the namespace has no tenant
// label; err is non-nil only for unexpected API errors. The Phase 1 path
// (multi-tenancy disabled) returns ("", true, nil) unconditionally.
func (r *SandboxReconciler) resolveTenant(ctx context.Context, sb *setecv1alpha1.Sandbox) (tenant string, ok bool, err error) {
	if !r.MultiTenancyEnabled || r.TenantLabelKey == "" {
		return "", true, nil
	}
	ns := &corev1.Namespace{}
	if err := r.Get(ctx, types.NamespacedName{Name: sb.Namespace}, ns); err != nil {
		if apierrors.IsNotFound(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("get namespace %q: %w", sb.Namespace, err)
	}
	tid, err := tenancy.FromNamespace(ns, r.TenantLabelKey)
	if err != nil {
		if errors.Is(err, tenancy.ErrTenantLabelMissing) || errors.Is(err, tenancy.ErrTenantInvalid) {
			return "", false, nil
		}
		return "", false, err
	}
	return tid.String(), true, nil
}

// resolveClass delegates to ClassResolver. Returns (nil, false, nil) on
// the Phase 1 path (no resolver configured, or resolver returns the
// "no default class" signal in a single-tenant cluster with no class
// names). An explicit class name that fails to resolve is surfaced as an
// error the caller turns into a ClassNotFound Event.
func (r *SandboxReconciler) resolveClass(ctx context.Context, sb *setecv1alpha1.Sandbox) (*setecv1alpha1.SandboxClass, bool, error) {
	if r.ClassResolver == nil {
		return nil, false, nil
	}
	cls, err := r.ClassResolver.Resolve(ctx, sb)
	if err == nil {
		return cls, true, nil
	}
	// Back-compat: no default class is only fatal when the Sandbox
	// explicitly names one or multi-tenancy is enabled.
	if errors.Is(err, class.ErrNoDefaultClass) {
		if sb.Spec.SandboxClassName == "" && !r.MultiTenancyEnabled {
			return nil, false, nil
		}
	}
	return nil, false, err
}

// applyNetworkPolicy generates the desired NetworkPolicy from the
// Sandbox's network intent (and the resolved SandboxClass default posture)
// and creates or patches it.
//
// Every Sandbox gets a policy. There is no mode, and no combination of an
// absent spec.network with an absent class default, that produces a nil
// desired policy: netpol resolves an unstated posture to deny-all. The
// caller relies on that, because it creates the Pod only after this
// function succeeds.
// ensureNamespaceBaseline makes sure the namespace hosting sb carries the
// namespace-wide default-deny policy before any Sandbox Pod is created in
// it.
//
// # SCOPE, STATED PRECISELY
//
// Every other NetworkPolicy this operator writes selects on
// setec.zeroroot.ai/sandbox, so it constrains Pods this operator built
// from a Sandbox object. A Pod written into the same namespace by any
// other route carries no such label, matches no policy, and is therefore
// unrestricted — Kubernetes treats an unselected Pod as allow-all. This
// function is what removes that state, because its policy selects
// podSelector: {} rather than a label.
//
// It does not, and cannot, stop such a Pod from being created; that is an
// RBAC question, not a NetworkPolicy one. It confines the Pod once it
// exists. It also does not apply to a Pod with hostNetwork: true, which
// runs in the node's network namespace and is outside NetworkPolicy
// enforcement entirely — that case needs Pod Security Admission on the
// namespace, which this operator does not own.
//
// The object is deliberately NOT owner-referenced to the Sandbox: it is
// namespace-scoped and must outlive every individual Sandbox, so garbage
// collection on Sandbox delete would reopen the hole between runs.
func (r *SandboxReconciler) ensureNamespaceBaseline(
	ctx context.Context,
	logger logr.Logger,
	sb *setecv1alpha1.Sandbox,
) error {
	if !r.NamespaceBaselineDeny {
		return nil
	}
	desired := netpol.NamespaceBaseline(sb.Namespace)

	existing := &networkingv1.NetworkPolicy{}
	err := r.Get(ctx, types.NamespacedName{Namespace: desired.Namespace, Name: desired.Name}, existing)
	switch {
	case apierrors.IsNotFound(err):
		if err := r.Create(ctx, desired); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create namespace baseline NetworkPolicy in %q: %w", desired.Namespace, err)
		}
		logger.Info("applied namespace-wide default-deny NetworkPolicy",
			"namespace", desired.Namespace, "policy", desired.Name)
		r.Recorder.Eventf(sb, nil, corev1.EventTypeNormal, eventReasonNamespaceBaseline,
			actionApplyNetworkPolicy,
			"Applied namespace-wide default-deny NetworkPolicy %q in %q; every Pod in this namespace is now selected by a policy",
			desired.Name, desired.Namespace)
		return nil
	case err != nil:
		return fmt.Errorf("get namespace baseline NetworkPolicy in %q: %w", desired.Namespace, err)
	}

	// The object exists. Reconcile it back whenever it has stopped
	// expressing the namespace-wide deny — a narrowed selector, a
	// dropped policy type or an added rule each turn it into an allow
	// for some population, and the name alone is not evidence.
	if netpol.BaselineIsIntact(existing) {
		return nil
	}
	original := existing.DeepCopy()
	existing.Spec = desired.Spec
	if existing.Labels == nil {
		existing.Labels = map[string]string{}
	}
	maps.Copy(existing.Labels, desired.Labels)
	if err := r.Patch(ctx, existing, client.MergeFrom(original)); err != nil {
		return fmt.Errorf("patch namespace baseline NetworkPolicy in %q: %w", desired.Namespace, err)
	}
	logger.Info("restored namespace-wide default-deny NetworkPolicy",
		"namespace", desired.Namespace, "policy", desired.Name)
	return nil
}

func (r *SandboxReconciler) applyNetworkPolicy(ctx context.Context, sb *setecv1alpha1.Sandbox, cls *setecv1alpha1.SandboxClass) (ctrl.Result, error) {
	desired, err := r.NetPol.GenerateForClass(ctx, sb, cls)
	if err != nil {
		return r.recordAndReturnErr(sb, eventReasonReconcileError, fmt.Errorf("generate NetworkPolicy: %w", err))
	}
	// Own the NetworkPolicy so it is garbage-collected when the
	// Sandbox is deleted.
	if err := controllerutil.SetControllerReference(sb, desired, r.Scheme); err != nil {
		return r.recordAndReturnErr(sb, eventReasonReconcileError, fmt.Errorf("set owner on NetworkPolicy: %w", err))
	}

	existing := &networkingv1.NetworkPolicy{}
	err = r.Get(ctx, types.NamespacedName{Namespace: desired.Namespace, Name: desired.Name}, existing)
	switch {
	case apierrors.IsNotFound(err):
		if err := r.Create(ctx, desired); err != nil {
			if apierrors.IsAlreadyExists(err) {
				// A policy of this name already exists but was not
				// visible to the Get above — a stale object from a
				// crashed reconcile, or one written by something other
				// than this operator. It is NOT evidence that the
				// desired posture is in place, so this must not report
				// success: requeue so the next pass takes the
				// Get-then-Patch path and reconciles the spec.
				return ctrl.Result{RequeueAfter: networkPolicyReadBackRequeue}, nil
			}
			return r.recordAndReturnErr(sb, eventReasonReconcileError, fmt.Errorf("create NetworkPolicy: %w", err))
		}
		// Read the policy back before reporting success. A Create that
		// the API server accepted but that is not yet readable means the
		// caller must not create the Pod yet, so a read-back miss is a
		// requeue rather than a pass.
		readBack := &networkingv1.NetworkPolicy{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: desired.Namespace, Name: desired.Name}, readBack); err != nil {
			return ctrl.Result{RequeueAfter: networkPolicyReadBackRequeue}, nil //nolint:nilerr // a missing read-back is a retry, not a failure
		}
		r.Recorder.Eventf(sb, nil, corev1.EventTypeNormal, eventReasonNetworkPolicy, actionApplyNetworkPolicy,
			"Created NetworkPolicy %q for Sandbox", desired.Name)
		return ctrl.Result{}, nil
	case err != nil:
		return r.recordAndReturnErr(sb, eventReasonReconcileError, fmt.Errorf("get NetworkPolicy: %w", err))
	}

	// Patch if spec differs; reflect.DeepEqual is coarse but good
	// enough for the small surface Generate produces.
	if !reflect.DeepEqual(existing.Spec, desired.Spec) ||
		!reflect.DeepEqual(existing.Annotations, desired.Annotations) ||
		!reflect.DeepEqual(existing.Labels, desired.Labels) {
		original := existing.DeepCopy()
		existing.Spec = desired.Spec
		existing.Labels = desired.Labels
		existing.Annotations = desired.Annotations
		// Owner references preserved from existing to avoid churn.
		if err := r.Patch(ctx, existing, client.MergeFrom(original)); err != nil {
			return r.recordAndReturnErr(sb, eventReasonReconcileError, fmt.Errorf("patch NetworkPolicy: %w", err))
		}
	}
	return ctrl.Result{}, nil
}

// podRunningSince returns the earliest running start among the Pod's
// containers: the moment the Pod first ran a process.
func podRunningSince(pod *corev1.Pod) (time.Time, bool) {
	if pod == nil {
		return time.Time{}, false
	}
	var earliest time.Time
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.State.Running == nil || cs.State.Running.StartedAt.IsZero() {
			continue
		}
		if t := cs.State.Running.StartedAt.Time; earliest.IsZero() || t.Before(earliest) {
			earliest = t
		}
	}
	return earliest, !earliest.IsZero()
}

// recordTransition emits metrics for Sandbox phase transitions observed
// this reconcile. Safe to call with a nil MetricsCollector (no-op).
func (r *SandboxReconciler) recordTransition(
	sb *setecv1alpha1.Sandbox,
	cls *setecv1alpha1.SandboxClass,
	prev setecv1alpha1.SandboxPhase,
	curr setecv1alpha1.SandboxStatus,
	pod *corev1.Pod,
	tenantID string,
) {
	if r.MetricsCollector == nil {
		return
	}
	className := ""
	if cls != nil {
		className = cls.Name
	}
	runtimeLabel := runtimepkg.BackendLauncher

	if prev != curr.Phase {
		r.MetricsCollector.RecordPhaseTransition(tenantID, className, curr.Phase)

		// Cold-start: Sandbox creation to the moment the Pod runs, with
		// the runtime label of the one backend.
		//
		// The Running moment is the running start of the launcher container.
		// status.startedAt is pod.Status.StartTime, when the kubelet
		// accepted the Pod: before the image pull and the VM boot, and in
		// the same second as the Sandbox's creation on any quick cluster.
		// Timestamps are second-precision, so that difference was 0, and a
		// 0 was dropped, so the histogram stayed empty (setec#22). A start
		// within the creation second is a real sample and is kept.
		if curr.Phase == setecv1alpha1.SandboxPhaseRunning && !sb.CreationTimestamp.IsZero() {
			startTime := time.Now()
			if t, ok := podRunningSince(pod); ok {
				startTime = t
			}
			if d := startTime.Sub(sb.CreationTimestamp.Time); d >= 0 {
				r.MetricsCollector.ObserveColdStart(runtimeLabel, className, d)
			}
		}

		// Active gauge: delta +1 going into Running, -1 on terminal.
		switch {
		case prev != setecv1alpha1.SandboxPhaseRunning && curr.Phase == setecv1alpha1.SandboxPhaseRunning:
			r.MetricsCollector.SetActive(tenantID, className, +1)
		case prev == setecv1alpha1.SandboxPhaseRunning && isTerminalPhase(curr.Phase):
			r.MetricsCollector.SetActive(tenantID, className, -1)
		}
	}
}

// setSpanError annotates the span with an error status and message. Safe
// to call on a no-op span.
func setSpanError(span trace.Span, msg string) {
	if span == nil {
		return
	}
	span.SetStatus(codes.Error, msg)
}

// createPod builds the launcher Pod of the Sandbox with the pure
// podspec.BuildLauncher, reconciles the OwnerReference to the live Sandbox
// UID/APIVersion via controllerutil, and creates the Pod. The next
// reconcile observes the new Pod via the Owns watch.
//
// cls is optional: when non-nil the Pod inherits the class's NodeSelector
// and Tolerations. nodeName, when non-empty, pins the Pod to a node: the
// node of a local snapshot or of a warm pool base.
func (r *SandboxReconciler) createPod(
	ctx context.Context,
	sb *setecv1alpha1.Sandbox,
	cls *setecv1alpha1.SandboxClass,
	nodeName string,
) (ctrl.Result, error) {
	// The Pod's resolvers come from the same config the NetworkPolicy's
	// DNS rule is built from, filtered to the ones this class's policy
	// can reach (netpol.Config.ResolversFor, setec#76), so the addresses
	// the workload queries and the addresses the policy permits are the
	// same list by construction.
	resolvers, err := r.NetPol.ResolversFor(cls)
	if err != nil {
		return r.recordAndReturnErr(sb, eventReasonPodCreateFailed, fmt.Errorf("select Sandbox resolvers: %w", err))
	}
	if proceed, res, derr := r.waitForLauncherDisk(ctx, sb); !proceed {
		return res, derr
	}
	if err := r.markPoolUsed(ctx, sb, cls); err != nil {
		log.FromContext(ctx).Error(err, "stamp the use of the warm pool", "class", cls.Name)
	}
	// A warm start loads a base of the pool instead of a boot
	// (setec#103). The choice is recorded before the Pod exists, so
	// the restore step knows the base.
	base, berr := r.selectBase(ctx, sb, cls)
	if berr != nil {
		return r.recordAndReturnErr(sb, eventReasonPodCreateFailed, fmt.Errorf("select a warm pool base: %w", berr))
	}
	if err := r.markWarmBase(ctx, sb, base); err != nil {
		return ctrl.Result{}, err
	}
	if base != nil {
		nodeName = base.Spec.Node
	} else if poolActive(cls) && sb.Spec.Image == cls.Spec.PreWarmImage {
		r.countWarmStart(cls, "miss")
		r.recordColdBoot(ctx, sb)
	}
	// The identity of the Sandbox (setec#235): its own key, outside
	// the machine, and the generation that the next token carries.
	identity, ierr := r.ensureIdentity(ctx, sb)
	if ierr != nil {
		return r.recordAndReturnErr(sb, eventReasonPodCreateFailed, fmt.Errorf("the identity of the Sandbox: %w", ierr))
	}
	pod, err := podspec.BuildLauncher(sb, podspec.LauncherOptions{
		Identity: identity,
		// The scratch size resolves with the class: the Sandbox's own
		// value, else the class default, else 10 GiB (setec#172).
		Scratch:  limits.EffectiveScratch(sb, cls),
		Requests: classRequests(cls),
		Image:    r.LauncherImage, DiskRepo: r.DiskRepo, DiskKeys: r.DiskKeys, ResolverIPs: resolvers,
		Restore:     (sb.Spec.SnapshotRef != nil && sb.Spec.SnapshotRef.Name != "") || pendingCheckpoint(sb),
		NodeName:    nodeName,
		FromBase:    sb.Annotations[WarmBaseAnnotation] != "",
		CPUTemplate: classCPUTemplate(cls), InstanceType: r.restoreInstanceType(ctx, sb, cls),
	})
	if err != nil {
		return r.recordAndReturnErr(sb, eventReasonPodCreateFailed, fmt.Errorf("build Pod spec: %w", err))
	}

	// Merge class-level NodeSelector into the Pod. The map is additive —
	// the class cannot override an existing Pod-selector key.
	if cls != nil && len(cls.Spec.NodeSelector) > 0 {
		if pod.Spec.NodeSelector == nil {
			pod.Spec.NodeSelector = map[string]string{}
		}
		for k, v := range cls.Spec.NodeSelector {
			if _, exists := pod.Spec.NodeSelector[k]; !exists {
				pod.Spec.NodeSelector[k] = v
			}
		}
	}

	// Append class-level Tolerations onto the Pod. Tolerations have no
	// natural "key" to dedupe on the way NodeSelector does (the same key
	// may legitimately appear more than once with different operators/
	// values/effects), so this is a straight append rather than a
	// conflict-avoiding merge. This is what lets a Sandbox actually
	// schedule onto a tainted NodePool (e.g. a Karpenter pool reserved
	// for sandbox-host nodes) when the class declares the matching
	// toleration.
	if cls != nil && len(cls.Spec.Tolerations) > 0 {
		pod.Spec.Tolerations = append(pod.Spec.Tolerations, cls.Spec.Tolerations...)
	}

	// podspec.BuildLauncher already populates a basic OwnerReference, but UID and
	// APIVersion are authoritative only once the Scheme is consulted.
	// SetControllerReference overwrites the reference in place, which keeps
	// responsibility for the canonical form in the controller.
	pod.OwnerReferences = nil
	if err := controllerutil.SetControllerReference(sb, pod, r.Scheme); err != nil {
		return r.recordAndReturnErr(sb, eventReasonPodCreateFailed, fmt.Errorf("set owner reference: %w", err))
	}

	if err := r.Create(ctx, pod); err != nil {
		// A conflicting Create means another reconcile already produced
		// the Pod. Treat it as success so the next reconcile can observe
		// the live Pod via the Owns watch.
		if apierrors.IsAlreadyExists(err) {
			return ctrl.Result{}, nil
		}
		return r.recordAndReturnErr(sb, eventReasonPodCreateFailed, fmt.Errorf("create Pod: %w", err))
	}

	r.Recorder.Eventf(sb, nil, corev1.EventTypeNormal, eventReasonPodCreated, actionCreateSandboxPod,
		"Created Pod %q for Sandbox", pod.Name)
	return ctrl.Result{}, nil
}

// classNotFoundExpired reports whether a Sandbox has been waiting on an
// unresolvable SandboxClass for longer than classNotFoundGrace (setec#299).
//
// The clock is the Sandbox's own creation timestamp, not the last status
// transition: patchPendingStatus only writes when the phase/reason actually
// changes, so LastTransitionTime stays pinned at the first ClassNotFound and
// would work here too — but creationTimestamp is set by the API server, is
// immutable, and cannot be reset by a status rewrite, which makes the deadline
// impossible to extend accidentally.
func classNotFoundExpired(sb *setecv1alpha1.Sandbox, now time.Time, grace time.Duration) bool {
	if grace <= 0 {
		grace = classNotFoundGrace
	}
	created := sb.CreationTimestamp.Time
	if created.IsZero() {
		// No creation stamp (fake client in a unit test, or an object built
		// by hand): treat as fresh rather than instantly failing it.
		return false
	}
	return now.Sub(created) >= grace
}

// classNotFoundGraceOrDefault resolves the effective grace period for this
// reconciler.
func (r *SandboxReconciler) classNotFoundGraceOrDefault() time.Duration {
	if r.ClassNotFoundGrace > 0 {
		return r.ClassNotFoundGrace
	}
	return classNotFoundGrace
}

// orphanRetentionOrDefault resolves the effective post-terminal retention for
// a Sandbox whose class never resolved.
func (r *SandboxReconciler) orphanRetentionOrDefault() time.Duration {
	if r.OrphanRetention > 0 {
		return r.OrphanRetention
	}
	return orphanedSandboxRetention
}

// orphanReapDue reports whether an orphaned Sandbox has outlived both the
// class-resolution grace period and the post-terminal diagnosis window, and
// returns how long remains when it has not (setec#299).
//
// Both deadlines hang off creationTimestamp for the same reason the grace
// period does: it is API-server-set, immutable, and cannot be pushed out by a
// status rewrite, so the reap instant is fixed the moment the object exists.
func orphanReapDue(sb *setecv1alpha1.Sandbox, now time.Time, grace, retention time.Duration) (bool, time.Duration) {
	created := sb.CreationTimestamp.Time
	if created.IsZero() {
		return false, retention
	}
	remaining := created.Add(grace + retention).Sub(now)
	return remaining <= 0, remaining
}

// ephemeralRetentionOrDefault resolves the effective post-terminal retention
// for an ephemeral Sandbox before it is auto-destroyed (docs/design/lifecycles.md).
func (r *SandboxReconciler) ephemeralRetentionOrDefault() time.Duration {
	if r.EphemeralRetention > 0 {
		return r.EphemeralRetention
	}
	return ephemeralFinishedRetention
}

// ephemeralReapDue reports whether a terminal ephemeral Sandbox has outlived
// its post-terminal retention window, and how long remains when it has not.
//
// The window is anchored on status.LastTransitionTime — the instant the
// Sandbox went terminal — not on creationTimestamp, so the creator gets the
// full window to read the result no matter how long the run itself took. A
// terminal Sandbox always carries LastTransitionTime: internal/status.setPhase
// stamps it on the transition. The nil guard is belt-and-braces — a Sandbox
// whose finish instant is unknown is never deleted; the reap is simply
// deferred one retention period so a hand-built object cannot be collected on
// a timeline the controller cannot see.
func ephemeralReapDue(sb *setecv1alpha1.Sandbox, now time.Time, retention time.Duration) (bool, time.Duration) {
	fin := sb.Status.LastTransitionTime
	if fin == nil || fin.Time.IsZero() {
		return false, retention
	}
	remaining := fin.Time.Add(retention).Sub(now)
	return remaining <= 0, remaining
}

// reapExpiredEphemeral enforces docs/design/lifecycles.md's "auto-destroy on exit" for the
// ephemeral lifecycle. A terminal ephemeral Sandbox is kept for
// ephemeralRetentionOrDefault() so its creator can read the outcome (Wait)
// and drain the captured output (StreamLogs), then the reconciler deletes it;
// owner-reference GC removes the backing Pod and any NetworkPolicy so no
// microVM lingers when a caller never calls Kill.
//
// It returns handled=true when it owns this reconcile — the Sandbox is a
// terminal ephemeral object — so the caller returns immediately: no class
// resolution or Pod reconcile is needed once an ephemeral run has finished.
// A live Sandbox, or any session Sandbox, returns handled=false and the
// normal reconcile proceeds. When the window has not yet closed it requeues
// for exactly the remaining time, because a terminal Sandbox with a stable
// Pod generates no further watch events and would otherwise be immortal.
func (r *SandboxReconciler) reapExpiredEphemeral(
	ctx context.Context,
	logger logr.Logger,
	sb *setecv1alpha1.Sandbox,
) (res ctrl.Result, reaped bool, err error) {
	if !sb.Spec.IsEphemeral() || !isTerminalPhase(sb.Status.Phase) {
		return ctrl.Result{}, false, nil
	}

	retention := r.ephemeralRetentionOrDefault()
	due, remaining := ephemeralReapDue(sb, time.Now(), retention)
	if !due {
		return ctrl.Result{RequeueAfter: remaining}, true, nil
	}

	r.Recorder.Eventf(sb, nil, corev1.EventTypeNormal, eventReasonEphemeralReaped, actionReapEphemeralSandbox,
		"ephemeral Sandbox reached %s; auto-destroyed %s after it finished (docs/design/lifecycles.md)", sb.Status.Phase, retention)
	logger.Info("reaping terminal ephemeral Sandbox",
		"phase", sb.Status.Phase, "retention", retention)
	if err := r.Delete(ctx, sb); err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, true, fmt.Errorf("delete terminal ephemeral Sandbox: %w", err)
	}
	return ctrl.Result{}, true, nil
}

// patchFailedStatus drives a Sandbox to the terminal Failed phase with a
// machine-readable reason. Terminal phases are where the reconciler stops
// recreating Pods (see handleMissingPod), so this is what actually ends a
// stuck object's lifecycle.
//
// SandboxStatus carries no free-text message field; the human-readable detail
// rides the Warning Event the caller has already recorded.
func (r *SandboxReconciler) patchFailedStatus(
	ctx context.Context,
	sb *setecv1alpha1.Sandbox,
	reason string,
) error {
	if sb.Status.Phase == setecv1alpha1.SandboxPhaseFailed && sb.Status.Reason == reason {
		return nil
	}
	original := sb.DeepCopy()
	sb.Status.Phase = setecv1alpha1.SandboxPhaseFailed
	sb.Status.Reason = reason
	now := metav1.NewTime(time.Now())
	sb.Status.LastTransitionTime = &now
	return errwrap.Wrap(r.Status().Patch(ctx, sb, client.MergeFrom(original)), "client.SubResourceWriter.Patch")
}

// patchPendingStatus writes a minimal Pending/<reason> status using the
// status subresource. It is idempotent: no-op if the live status already
// reflects the desired phase and reason.
func (r *SandboxReconciler) patchPendingStatus(
	ctx context.Context,
	sb *setecv1alpha1.Sandbox,
	reason string,
) error {
	if sb.Status.Phase == setecv1alpha1.SandboxPhasePending && sb.Status.Reason == reason {
		return nil
	}
	original := sb.DeepCopy()
	sb.Status.Phase = setecv1alpha1.SandboxPhasePending
	sb.Status.Reason = reason
	now := metav1.NewTime(time.Now())
	sb.Status.LastTransitionTime = &now
	return errwrap.Wrap(r.Status().Patch(ctx, sb, client.MergeFrom(original)), "client.SubResourceWriter.Patch")
}

// reconcilePhase3Lifecycle handles desiredState pause/resume and the
// one-shot snapshot.create flow. The reconciler calls it after status
// derivation so the phase values read here are authoritative for
// this reconcile tick.
//
// Idempotency: each branch short-circuits when the observed state
// already matches the desired state. Re-running this method against
// a stable Sandbox produces zero gRPC calls.
func (r *SandboxReconciler) reconcilePhase3Lifecycle(
	ctx context.Context,
	sb *setecv1alpha1.Sandbox,
	desired setecv1alpha1.SandboxStatus,
) (ctrl.Result, error) {
	// Pause: desiredState=Paused AND currently Running.
	if sb.Spec.DesiredState == setecv1alpha1.SandboxDesiredStatePaused &&
		desired.Phase == setecv1alpha1.SandboxPhaseRunning {
		if err := r.Coordinator.Pause(ctx, sb); err != nil {
			return r.recordAndReturnErr(sb, eventReasonReconcileError, fmt.Errorf("pause: %w", err))
		}
		if err := r.patchPhase(ctx, sb, setecv1alpha1.SandboxPhasePaused, "UserPaused", true); err != nil {
			return ctrl.Result{}, fmt.Errorf("patch Paused: %w", err)
		}
		r.Recorder.Eventf(sb, nil, corev1.EventTypeNormal, eventReasonPaused, actionPauseSandbox, "%s", "Sandbox paused on user request")
		return ctrl.Result{}, nil
	}

	// Resume: desiredState=Running AND currently Paused.
	if (sb.Spec.DesiredState == "" || sb.Spec.DesiredState == setecv1alpha1.SandboxDesiredStateRunning) &&
		desired.Phase == setecv1alpha1.SandboxPhasePaused {
		if err := r.Coordinator.Resume(ctx, sb); err != nil {
			return r.recordAndReturnErr(sb, eventReasonReconcileError, fmt.Errorf("resume: %w", err))
		}
		if err := r.patchPhase(ctx, sb, setecv1alpha1.SandboxPhaseRunning, "", false); err != nil {
			return ctrl.Result{}, fmt.Errorf("patch Running: %w", err)
		}
		r.Recorder.Eventf(sb, nil, corev1.EventTypeNormal, eventReasonResumed, actionResumeSandbox, "%s", "Sandbox resumed on user request")
		return ctrl.Result{}, nil
	}

	// Snapshot create: snapshot.create=true AND Sandbox stable (Running
	// or Paused) AND no Snapshot CR with the target name yet.
	if sb.Spec.Snapshot != nil && sb.Spec.Snapshot.Create && sb.Spec.Snapshot.Name != "" &&
		(desired.Phase == setecv1alpha1.SandboxPhaseRunning ||
			desired.Phase == setecv1alpha1.SandboxPhasePaused) {
		existing := &setecv1alpha1.Snapshot{}
		err := r.Get(ctx, types.NamespacedName{Namespace: sb.Namespace, Name: sb.Spec.Snapshot.Name}, existing)
		if err == nil {
			// A Snapshot with the target name exists. Which phase it is
			// in decides what happens next, and it has to be read.
			//
			// The Coordinator now creates the CR before the storage write
			// so that Creating and Failed are reportable (setec#129).
			// That means "the object exists" no longer implies "the state
			// is on disk". Treating existence as success would run the
			// AfterCreate intent on a failed snapshot, and with
			// afterCreate=Terminated that deletes the Sandbox whose state
			// was never saved.
			switch existing.Status.Phase {
			case setecv1alpha1.SnapshotPhaseCreating:
				// Another reconcile is mid-write, or one died mid-write.
				// Wait rather than racing a second RPC at the same name.
				return ctrl.Result{RequeueAfter: snapshotInFlightRequeue}, nil
			case setecv1alpha1.SnapshotPhaseFailed:
				// Terminal by design: a Failed snapshot never becomes
				// Ready, so re-snapshotting under the same name is not an
				// option and the AfterCreate intent must not run. The
				// user deletes the Snapshot and asks again.
				r.Recorder.Eventf(sb, nil, corev1.EventTypeWarning, eventReasonSnapshotCreateFailed, actionRequestSnapshot,
					"Snapshot %q is in phase Failed (%s); delete it to retry. Not applying afterCreate=%q",
					existing.Name, existing.Status.Reason, sb.Spec.Snapshot.AfterCreate)
				return ctrl.Result{}, r.patchPhase(ctx, sb, setecv1alpha1.SandboxPhaseRunning, "SnapshotCreateFailed", false)
			default:
				// Ready, Terminating, or a phase this version does not
				// know. Honor the AfterCreate intent without
				// re-snapshotting, as before.
				return ctrl.Result{}, nil
			}
		}
		if !apierrors.IsNotFound(err) {
			return r.recordAndReturnErr(sb, eventReasonReconcileError, fmt.Errorf("check Snapshot: %w", err))
		}

		// Mark Snapshotting before the RPC so status observers see the
		// transient phase.
		if err := r.patchPhase(ctx, sb, setecv1alpha1.SandboxPhaseSnapshotting, "SnapshotInProgress", true); err != nil {
			return ctrl.Result{}, fmt.Errorf("patch Snapshotting: %w", err)
		}
		r.Recorder.Eventf(sb, nil, corev1.EventTypeNormal, eventReasonSnapshotCreateStarted, actionRequestSnapshot,
			"creating Snapshot %q", sb.Spec.Snapshot.Name)
		if err := r.Coordinator.CreateSnapshot(ctx, sb); err != nil {
			// Roll back to Running if the Coordinator could not complete
			// (e.g. InsufficientStorage before VM pause).
			_ = r.patchPhase(ctx, sb, setecv1alpha1.SandboxPhaseRunning, "SnapshotCreateFailed", false)
			return r.recordAndReturnErr(sb, eventReasonReconcileError, fmt.Errorf("create snapshot: %w", err))
		}

		// After-create transition.
		after := sb.Spec.Snapshot.AfterCreate
		if after == "" {
			after = setecv1alpha1.SandboxSnapshotAfterCreateRunning
		}
		switch after {
		case setecv1alpha1.SandboxSnapshotAfterCreatePaused:
			return ctrl.Result{}, r.patchPhase(ctx, sb, setecv1alpha1.SandboxPhasePaused, "UserPaused", true)
		case setecv1alpha1.SandboxSnapshotAfterCreateTerminated:
			if err := r.Delete(ctx, sb); err != nil && !apierrors.IsNotFound(err) {
				return ctrl.Result{}, fmt.Errorf("delete sandbox after snapshot: %w", err)
			}
			return ctrl.Result{}, nil
		default:
			return ctrl.Result{}, r.patchPhase(ctx, sb, setecv1alpha1.SandboxPhaseRunning, "", false)
		}
	}

	return ctrl.Result{}, nil
}

// patchPhase mutates the Sandbox status to the given phase/reason
// pair. setPausedAt=true also stamps status.pausedAt with the current
// wall-clock; setPausedAt=false clears it.
func (r *SandboxReconciler) patchPhase(
	ctx context.Context,
	sb *setecv1alpha1.Sandbox,
	phase setecv1alpha1.SandboxPhase,
	reason string,
	setPausedAt bool,
) error {
	original := sb.DeepCopy()
	sb.Status.Phase = phase
	sb.Status.Reason = reason
	now := metav1.NewTime(time.Now())
	sb.Status.LastTransitionTime = &now
	if setPausedAt {
		sb.Status.PausedAt = &now
	} else {
		sb.Status.PausedAt = nil
	}
	return errwrap.Wrap(r.Status().Patch(ctx, sb, client.MergeFrom(original)), "client.SubResourceWriter.Patch")
}

// recordAndReturnErr emits a Warning Event for an unexpected error and
// returns the error so controller-runtime's exponential backoff re-queues
// the request. Keeping this in one helper guarantees every error path
// produces a visible Event.
func (r *SandboxReconciler) recordAndReturnErr(
	sb *setecv1alpha1.Sandbox,
	reason string,
	err error,
) (ctrl.Result, error) {
	if r.Recorder != nil {
		r.Recorder.Eventf(sb, nil, corev1.EventTypeWarning, reason, actionFinalizeSandbox, "%s", err.Error())
	}
	return ctrl.Result{}, err
}

// isTerminalPhase mirrors the guard in internal/status but is duplicated
// here so the controller file does not pull in the private helper. The two
// definitions must agree.
func isTerminalPhase(p setecv1alpha1.SandboxPhase) bool {
	return p == setecv1alpha1.SandboxPhaseCompleted || p == setecv1alpha1.SandboxPhaseFailed
}

// statusEqual reports whether two SandboxStatus values are deeply equal. It
// is a thin wrapper around reflect.DeepEqual; centralizing the call lets us
// swap the implementation later (e.g. for a cmp.Diff-based version) without
// touching Reconcile.
func statusEqual(a, b setecv1alpha1.SandboxStatus) bool {
	return reflect.DeepEqual(a, b)
}

// SetupWithManager registers the reconciler with the given controller
// manager. Owns(&corev1.Pod{}) installs a watch that re-queues the parent
// Sandbox whenever an owned Pod event fires, which is how Pod status
// transitions drive Sandbox status convergence. Phase 2 additionally
// Owns(&networkingv1.NetworkPolicy{}) so NetworkPolicy edits surface
// back to the parent Sandbox for reconcile.
func (r *SandboxReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return errwrap.Wrap(ctrl.NewControllerManagedBy(mgr).
		For(&setecv1alpha1.Sandbox{}).
		Owns(&corev1.Pod{}).
		Owns(&networkingv1.NetworkPolicy{}).
		Owns(&corev1.PersistentVolumeClaim{}).
		Watches(&corev1.Node{},
			handler.EnqueueRequestsFromMapFunc(r.sandboxesOnCordonedNode),
			builder.WithPredicates(nodeCordonPredicate())).
		Complete(r), "builder.TypedBuilder.Complete")
}

// nodeCordonPredicate admits only Node events where the node is (or
// just became) unschedulable — the checkpoint-on-drain trigger
// (setec#194). Create/delete/generic events for schedulable nodes are
// filtered out so ordinary node churn does not fan out reconciles.
func nodeCordonPredicate() predicate.Funcs {
	cordoned := func(obj client.Object) bool {
		node, ok := obj.(*corev1.Node)
		return ok && node.Spec.Unschedulable
	}
	return predicate.Funcs{
		CreateFunc:  func(e event.CreateEvent) bool { return cordoned(e.Object) },
		UpdateFunc:  func(e event.UpdateEvent) bool { return cordoned(e.ObjectNew) && !cordoned(e.ObjectOld) },
		DeleteFunc:  func(event.DeleteEvent) bool { return false },
		GenericFunc: func(e event.GenericEvent) bool { return cordoned(e.Object) },
	}
}

// sandboxesOnCordonedNode maps a cordoned Node to the session
// Sandboxes whose VM Pods run on it, so checkpoint-on-drain fires
// proactively at cordon time instead of waiting for the eviction to
// reach each Pod.
func (r *SandboxReconciler) sandboxesOnCordonedNode(ctx context.Context, obj client.Object) []reconcile.Request {
	node, ok := obj.(*corev1.Node)
	if !ok {
		return nil
	}
	podList := &corev1.PodList{}
	if err := r.List(ctx, podList, client.HasLabels{podspec.SandboxLabelKey}); err != nil {
		log.FromContext(ctx).Error(err, "list sandbox Pods for cordoned node", "node", node.Name)
		return nil
	}
	var reqs []reconcile.Request
	for i := range podList.Items {
		p := &podList.Items[i]
		if p.Spec.NodeName != node.Name {
			continue
		}
		if sbName, ok := p.Labels[podspec.SandboxLabelKey]; ok && sbName != "" {
			reqs = append(reqs, reconcile.Request{
				Namespace: p.Namespace, Name: sbName,
			})
		}
	}
	return reqs
}

// ctrlResult dereferences a result that ends a reconcile, or returns the
// zero result.
func ctrlResult(res *ctrl.Result) ctrl.Result {
	if res == nil {
		return ctrl.Result{}
	}
	return *res
}
