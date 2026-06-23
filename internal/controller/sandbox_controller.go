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

// Package controller wires the Sandbox custom resource to a backing Pod via
// the controller-runtime reconciler pattern. This file is the only place in
// the operator that performs Kubernetes I/O — all transformation logic lives
// in the pure packages (internal/podspec, internal/status, internal/prereq)
// that the reconciler composes.
package controller

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/go-logr/logr"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
	"github.com/zeroroot-ai/setec/internal/class"
	"github.com/zeroroot-ai/setec/internal/metrics"
	"github.com/zeroroot-ai/setec/internal/netpol"
	"github.com/zeroroot-ai/setec/internal/podspec"
	"github.com/zeroroot-ai/setec/internal/prereq"
	runtimepkg "github.com/zeroroot-ai/setec/internal/runtime"
	"github.com/zeroroot-ai/setec/internal/snapshot"
	"github.com/zeroroot-ai/setec/internal/status"
	"github.com/zeroroot-ai/setec/internal/tenancy"
	"github.com/zeroroot-ai/setec/internal/tracing"
)

const (
	// runtimeUnavailableRequeue is how long the reconciler waits before
	// re-checking for the Kata RuntimeClass when it is missing. The user
	// installs kata-deploy out of band; polling once a minute strikes a
	// balance between responsiveness and API-server load.
	runtimeUnavailableRequeue = 60 * time.Second

	// Event reasons. Kept as constants so tests and docs can reference them
	// by name rather than string-matching fragments of the message.
	eventReasonRuntimeUnavailable    = "RuntimeUnavailable"
	eventReasonPodCreateFailed       = "PodCreateFailed"
	eventReasonPodCreated            = "PodCreated"
	eventReasonTimeout               = "TimeoutExceeded"
	eventReasonReconcileError        = "ReconcileError"
	eventReasonClassNotFound         = "ClassNotFound"
	eventReasonConstraintViolated    = "ConstraintViolated"
	eventReasonTenantMissing         = "TenantLabelMissing"
	eventReasonNetworkPolicy         = "NetworkPolicyApplied"
	eventReasonSnapshotUnavailable   = "SnapshotUnavailable"
	eventReasonSnapshotIncompatible  = "SnapshotIncompatible"
	eventReasonPaused                = "Paused"
	eventReasonResumed               = "Resumed"
	eventReasonSnapshotCreateStarted = "SnapshotCreateStarted"

	// runtimeUnavailableMessage is the vendor-neutral remediation guidance
	// emitted when the configured RuntimeClass is missing. It links to the
	// project's own documentation rather than to any specific cloud or
	// distribution guide, per Requirement 5.4.
	runtimeUnavailableMessage = "Kata RuntimeClass %q is not installed in this cluster; install Kata Containers via kata-deploy and see project documentation for remediation"
)

// SandboxReconciler reconciles a Sandbox object. All fields are set at
// construction time in cmd/manager/main.go; the struct is immutable after
// SetupWithManager completes.
type SandboxReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder

	// Runtimes is the registry of enabled RuntimeDispatcher implementations.
	// It is used by selectRuntime to pick the appropriate backend for each Sandbox.
	// Set at construction time; replaces the old RuntimeClassName string field.
	Runtimes *runtimepkg.Registry

	// RuntimeCfg is the operator-wide runtime configuration loaded from
	// --runtimes-config (or synthesized from the legacy --runtime-class-name flag).
	// selectRuntime reads cluster defaults and fallback chains from this value.
	RuntimeCfg *runtimepkg.RuntimeConfig

	// NodeSelectorLabel is the label key Nodes must carry to be considered
	// Kata-capable (default "katacontainers.io/kata-runtime"). Used by
	// prereq.CheckMulti only; the reconciler itself does not select Nodes directly.
	NodeSelectorLabel string

	// --- Phase 2 optional dependencies ---
	//
	// All four of these may be nil. A nil value disables the
	// corresponding feature and the reconciler falls through to its
	// Phase 1 behaviour. This is the back-compat contract called out in
	// design.md Requirement 8.

	// ClassResolver maps a Sandbox to its effective SandboxClass. When
	// nil, Sandboxes are reconciled without class constraint validation
	// (Phase 1 behaviour).
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

	// --- Phase 3 optional dependencies ---
	//
	// Both fields may be nil. A nil Coordinator disables every Phase 3
	// reconcile branch (snapshot create, restore, pause/resume); the
	// reconciler then falls through to Phase 2 behaviour unchanged. The
	// SnapshotReadyIndex (see SetupWithManager) is populated only when
	// Coordinator is wired.

	// Coordinator orchestrates the operator-side snapshot work.
	Coordinator *snapshot.Coordinator
}

// RBAC markers. These are consumed by controller-gen to generate the
// ClusterRole at config/rbac/role.yaml. The markers live as a standalone
// comment block (not attached to a declaration) because controller-gen
// v0.20+ recognises +kubebuilder:rbac markers at package level; binding
// them to a func's doc comment suppresses generation.
//
// +kubebuilder:rbac:groups=setec.zeroroot.ai,resources=sandboxes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=setec.zeroroot.ai,resources=sandboxes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=setec.zeroroot.ai,resources=sandboxes/finalizers,verbs=update
// +kubebuilder:rbac:groups=setec.zeroroot.ai,resources=sandboxclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=core,resources=nodes,verbs=get;list
// +kubebuilder:rbac:groups=core,resources=namespaces,verbs=get;list;watch
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=node.k8s.io,resources=runtimeclasses,verbs=get;list

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
		return ctrl.Result{RequeueAfter: runtimeUnavailableRequeue}, nil
	}

	// (4) Phase 2: resolve the effective SandboxClass. A missing named
	// class is fatal; a missing default class with Phase 1-shaped Sandbox
	// (no class name, no multitenancy) is explicitly back-compat-tolerated.
	cls, classResolved, classErr := r.resolveClass(ctx, sb)
	if classErr != nil {
		r.Recorder.Eventf(sb, nil, corev1.EventTypeWarning, eventReasonClassNotFound, actionResolveSandboxClass, "%s", classErr.Error())
		if err := r.patchPendingStatus(ctx, sb, eventReasonClassNotFound); err != nil {
			return ctrl.Result{}, fmt.Errorf("patch ClassNotFound status: %w", err)
		}
		setSpanError(span, classErr.Error())
		return ctrl.Result{RequeueAfter: runtimeUnavailableRequeue}, nil
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
			return ctrl.Result{RequeueAfter: runtimeUnavailableRequeue}, nil
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

	// (7) Verify cluster prerequisites. Missing RuntimeClasses are not errors —
	// they are cluster-configuration issues the operator surfaces via Events
	// so `kubectl describe sandbox` shows remediation guidance.
	if res, err := r.checkPrereqs(ctx, sb); err != nil || res.RequeueAfter > 0 {
		return res, err
	}

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

// handleMissingPod is called when the owned Pod does not yet exist. It skips
// creation for terminal Sandboxes and otherwise selects a runtime, creates the
// Pod, and applies the NetworkPolicy.
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
	sel, selErr := r.selectRuntime(ctx, sb, cls)
	if selErr != nil {
		if errors.Is(selErr, runtimepkg.ErrNoEligibleRuntime) {
			return ctrl.Result{RequeueAfter: runtimeUnavailableRequeue}, nil
		}
		return r.recordAndReturnErr(sb, eventReasonReconcileError, fmt.Errorf("select runtime: %w", selErr))
	}
	if res, err := r.createPod(ctx, sb, cls, pinnedNode, sel); err != nil || res.RequeueAfter > 0 {
		return res, err
	}
	return r.applyNetworkPolicy(ctx, sb, cls)
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
	// (9) Ensure NetworkPolicy (idempotent).
	if _, err := r.applyNetworkPolicy(ctx, sb, cls); err != nil {
		return ctrl.Result{}, err
	}

	// (10) Derive status and patch when changed.
	desired := status.Derive(sb, pod, time.Now())
	if !statusEqual(sb.Status, desired) {
		original := sb.DeepCopy()
		sb.Status = desired
		if err := r.Status().Patch(ctx, sb, client.MergeFrom(original)); err != nil {
			return r.recordAndReturnErr(sb, eventReasonReconcileError, fmt.Errorf("patch Sandbox status: %w", err))
		}
	}

	// (11) Record phase transition metrics and span status.
	r.recordTransition(sb, cls, prevPhase, desired, pod, tenantID)

	// (11a) Phase 3: pause/resume lifecycle.
	if r.Coordinator != nil {
		if res, err := r.reconcilePhase3Lifecycle(ctx, sb, desired); err != nil || res.RequeueAfter > 0 {
			return res, err
		}
	}

	// (12) Delete timed-out Pod (guard against repeated deletes).
	if desired.Phase == setecv1alpha1.SandboxPhaseFailed &&
		desired.Reason == status.ReasonTimeout &&
		pod.DeletionTimestamp.IsZero() {
		r.Recorder.Eventf(sb, nil, corev1.EventTypeWarning, eventReasonTimeout, actionEnforceTimeout,
			"Sandbox exceeded lifecycle.timeout; deleting Pod %q", pod.Name)
		if err := r.Delete(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
			return r.recordAndReturnErr(sb, eventReasonReconcileError, fmt.Errorf("delete Pod after timeout: %w", err))
		}
	}

	span.SetAttributes(attribute.String("setec.sandbox.phase", string(desired.Phase)))
	if desired.Phase == setecv1alpha1.SandboxPhaseFailed {
		span.SetStatus(codes.Error, desired.Reason)
	}
	return ctrl.Result{}, nil
}

// checkPrereqs verifies that the required RuntimeClass(es) exist in the cluster.
// When Runtimes/RuntimeCfg are set (multi-backend path) it checks all enabled
// backends; otherwise it falls back to the legacy single-class check.
// Returns a non-zero RequeueAfter result when prerequisites are not yet met.
func (r *SandboxReconciler) checkPrereqs(ctx context.Context, sb *setecv1alpha1.Sandbox) (ctrl.Result, error) {
	if r.Runtimes != nil && r.RuntimeCfg != nil {
		classNames := make(map[string]string, len(r.RuntimeCfg.Runtimes))
		for name, bc := range r.RuntimeCfg.Runtimes {
			if bc.Enabled {
				classNames[name] = bc.RuntimeClassName
			}
		}
		prereqResult, err := prereq.CheckMulti(ctx, r.Client, r.RuntimeCfg.EnabledBackends(), classNames, r.NodeSelectorLabel)
		if err != nil {
			return r.recordAndReturnErr(sb, eventReasonReconcileError, fmt.Errorf("prereq check: %w", err))
		}
		if !prereqResult.RuntimeClassPresent {
			msg := runtimeUnavailableMessage
			if len(prereqResult.Warnings) > 0 {
				msg = prereqResult.Warnings[0]
			}
			r.Recorder.Eventf(sb, nil, corev1.EventTypeWarning, eventReasonRuntimeUnavailable, actionResolveRuntime, "%s", msg)
			if err := r.patchPendingStatus(ctx, sb, eventReasonRuntimeUnavailable); err != nil {
				return ctrl.Result{}, fmt.Errorf("patch RuntimeUnavailable status: %w", err)
			}
			return ctrl.Result{RequeueAfter: runtimeUnavailableRequeue}, nil
		}
		return ctrl.Result{}, nil
	}
	// Legacy path: single-class check with the operator-wide default.
	legacyClassName := ""
	if r.RuntimeCfg != nil {
		legacyClassName = r.RuntimeCfg.Defaults.Runtime.Backend
	}
	prereqResult, err := prereq.Check(ctx, r.Client, legacyClassName, r.NodeSelectorLabel)
	if err != nil {
		return r.recordAndReturnErr(sb, eventReasonReconcileError, fmt.Errorf("prereq check: %w", err))
	}
	if !prereqResult.RuntimeClassPresent {
		msg := fmt.Sprintf(runtimeUnavailableMessage, legacyClassName)
		r.Recorder.Eventf(sb, nil, corev1.EventTypeWarning, eventReasonRuntimeUnavailable, actionResolveRuntime, "%s", msg)
		if err := r.patchPendingStatus(ctx, sb, eventReasonRuntimeUnavailable); err != nil {
			return ctrl.Result{}, fmt.Errorf("patch RuntimeUnavailable status: %w", err)
		}
		return ctrl.Result{RequeueAfter: runtimeUnavailableRequeue}, nil
	}
	return ctrl.Result{}, nil
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
		return "", ctrl.Result{RequeueAfter: runtimeUnavailableRequeue}, nil
	case getErr != nil:
		res, rerr := r.recordAndReturnErr(sb, eventReasonReconcileError, fmt.Errorf("get Snapshot: %w", getErr))
		return "", res, rerr
	}
	if snap.Status.Phase != setecv1alpha1.SnapshotPhaseReady {
		if perr := r.patchPendingStatus(ctx, sb, eventReasonSnapshotUnavailable); perr != nil {
			return "", ctrl.Result{}, fmt.Errorf("patch Pending(SnapshotUnavailable): %w", perr)
		}
		return "", ctrl.Result{RequeueAfter: runtimeUnavailableRequeue}, nil
	}
	if snapViolations := snapshot.Validate(sb, snap, cls); len(snapViolations) > 0 {
		sv := snapViolations[0]
		r.Recorder.Eventf(sb, nil, corev1.EventTypeWarning, eventReasonSnapshotIncompatible, actionResolveSnapshot, "%s", sv.String())
		if perr := r.patchPendingStatus(ctx, sb, eventReasonSnapshotIncompatible); perr != nil {
			return "", ctrl.Result{}, fmt.Errorf("patch SnapshotIncompatible status: %w", perr)
		}
		return "", ctrl.Result{RequeueAfter: runtimeUnavailableRequeue}, nil
	}
	return snap.Spec.Node, ctrl.Result{}, nil
}

// selectRuntime picks the isolation backend for this Sandbox by gathering a
// cluster-wide view of node capabilities (via Node labels) and delegating to
// r.Runtimes.Select. It writes status.runtime.chosen on success and
// transitions the Sandbox to Failed with reason NoEligibleNode when no backend
// can be satisfied. It also emits a fallback metric when the chosen backend
// differs from the originally requested one.
//
// When Runtimes or RuntimeCfg are nil (legacy path) it synthesizes a minimal
// Selection from the class RuntimeClassName / defaults so existing code paths
// keep working unchanged.
func (r *SandboxReconciler) selectRuntime(
	ctx context.Context,
	sb *setecv1alpha1.Sandbox,
	cls *setecv1alpha1.SandboxClass,
) (*runtimepkg.Selection, error) {
	logger := log.FromContext(ctx)

	// Legacy path: no registry wired. Synthesize a Selection using the
	// class RuntimeClassName or the legacy operator default so the reconciler
	// stays backward-compatible without a registry.
	if r.Runtimes == nil || r.RuntimeCfg == nil {
		rcName := ""
		if cls != nil && cls.Spec.RuntimeClassName != "" { //nolint:staticcheck // back-compat: RuntimeClassName retained until v2
			rcName = cls.Spec.RuntimeClassName //nolint:staticcheck // back-compat: RuntimeClassName retained until v2
		}
		// Build a minimal inline dispatcher that only supplies the RuntimeClass name.
		return &runtimepkg.Selection{
			Backend:    runtimepkg.BackendKataFC,
			Dispatcher: runtimepkg.NewKataFCDispatcher(runtimepkg.BackendConfig{RuntimeClassName: rcName}),
		}, nil
	}

	// Apply local defaulting: when the SandboxClass has no Runtime struct
	// (legacy class applied before the webhook defaulter runs), treat the
	// class as requesting the cluster-default backend. This mirrors what the
	// defaulting webhook will eventually do.
	effectiveCls := cls
	if cls != nil && cls.Spec.Runtime == nil {
		clsCopy := *cls
		clsCopy.Spec.Runtime = &setecv1alpha1.SandboxClassRuntime{
			Backend: r.RuntimeCfg.Defaults.Runtime.Backend,
		}
		effectiveCls = &clsCopy
	}

	// Gather capabilities: list all Nodes and collect the union of backends
	// they advertise via setec.zeroroot.ai/runtime.<backend>=true labels.
	// We take a cluster-wide union because we cannot pre-pick a node (the
	// scheduler does that); we only need to know whether any capable node
	// exists for each candidate backend.
	nodeList := &corev1.NodeList{}
	if err := r.List(ctx, nodeList); err != nil {
		return nil, fmt.Errorf("list nodes for capability detection: %w", err)
	}
	capSet := make(map[string]bool)
	for _, node := range nodeList.Items {
		for _, backend := range runtimepkg.AllKnownBackends {
			label := "setec.zeroroot.ai/runtime." + backend
			if val, ok := node.Labels[label]; ok && val == "true" {
				capSet[backend] = true
			}
		}
	}
	nodeCapabilities := make([]string, 0, len(capSet))
	for backend := range capSet {
		nodeCapabilities = append(nodeCapabilities, backend)
	}

	sel, err := r.Runtimes.Select(effectiveCls, r.RuntimeCfg, nodeCapabilities)
	if err != nil {
		if errors.Is(err, runtimepkg.ErrNoEligibleRuntime) {
			logger.Info("no eligible runtime for Sandbox",
				"sandbox", sb.Name,
				"namespace", sb.Namespace,
				"nodeCapabilities", nodeCapabilities,
				"error", err.Error(),
			)
			// Transition Sandbox to Failed with a clear reason.
			reason := "NoEligibleNode"
			original := sb.DeepCopy()
			sb.Status.Phase = setecv1alpha1.SandboxPhaseFailed
			sb.Status.Reason = reason
			now := metav1.NewTime(time.Now())
			sb.Status.LastTransitionTime = &now
			if patchErr := r.Status().Patch(ctx, sb, client.MergeFrom(original)); patchErr != nil {
				return nil, fmt.Errorf("patch NoEligibleNode status: %w", patchErr)
			}
			return nil, runtimepkg.ErrNoEligibleRuntime
		}
		return nil, err
	}

	// Record fallback metric when the chosen backend differs from requested.
	if sel.FellBack {
		logger.Info("runtime fallback applied",
			"sandbox", sb.Name,
			"from", sel.FromBackend,
			"to", sel.Backend,
		)
		r.Recorder.Eventf(sb, nil, corev1.EventTypeWarning, "RuntimeFallback", actionRunRuntimeFallback,
			"runtime fallback: requested %q, using %q", sel.FromBackend, sel.Backend)
		if r.MetricsCollector != nil {
			r.MetricsCollector.IncFallback(sel.FromBackend, sel.Backend)
		}
	}

	// Persist chosen backend into status.runtime.chosen.
	original := sb.DeepCopy()
	sb.Status.Runtime = &setecv1alpha1.SandboxRuntimeStatus{Chosen: sel.Backend}
	if err := r.Status().Patch(ctx, sb, client.MergeFrom(original)); err != nil {
		// Non-fatal: status write failure should not block pod creation.
		logger.Info("warning: failed to write status.runtime.chosen; continuing",
			"error", err.Error(),
		)
	}

	return sel, nil
}

// resolveTenant returns the tenant ID of the Sandbox's namespace (when
// multi-tenancy is enabled). ok is false when the namespace has no tenant
// label; err is non-nil only for unexpected API errors. The Phase 1 path
// (multi-tenancy disabled) returns ("", true, nil) unconditionally.
func (r *SandboxReconciler) resolveTenant(ctx context.Context, sb *setecv1alpha1.Sandbox) (string, bool, error) {
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
// Sandbox's network intent (and the resolved SandboxClass default-deny
// posture) and creates or patches it. A nil desired policy (effective
// mode=full or network absent with no class default) results in a no-op —
// the namespace default policy applies. When the resolved class declares
// spec.defaultNetworkMode=none|egress-allow-list, a Sandbox that does not
// declare its own spec.network inherits that closed posture so egress is
// default-deny per class (ADR-0052, setec#66).
func (r *SandboxReconciler) applyNetworkPolicy(ctx context.Context, sb *setecv1alpha1.Sandbox, cls *setecv1alpha1.SandboxClass) (ctrl.Result, error) {
	desired, err := netpol.GenerateForClass(sb, cls)
	if err != nil {
		return r.recordAndReturnErr(sb, eventReasonReconcileError, fmt.Errorf("generate NetworkPolicy: %w", err))
	}
	if desired == nil {
		// mode=full or absent network: no policy to manage.
		return ctrl.Result{}, nil
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
				return ctrl.Result{}, nil
			}
			return r.recordAndReturnErr(sb, eventReasonReconcileError, fmt.Errorf("create NetworkPolicy: %w", err))
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
	vmm := ""
	if cls != nil {
		className = cls.Name
		vmm = string(cls.Spec.VMM) //nolint:staticcheck // back-compat: VMM retained until v2
	}
	// Determine the runtime label: prefer status.runtime.chosen (written by
	// selectRuntime), fall back to the legacy VMM field for backward compat.
	runtimeLabel := vmm
	if curr.Runtime != nil && curr.Runtime.Chosen != "" {
		runtimeLabel = curr.Runtime.Chosen
	} else if sb.Status.Runtime != nil && sb.Status.Runtime.Chosen != "" {
		runtimeLabel = sb.Status.Runtime.Chosen
	}

	if prev != curr.Phase {
		r.MetricsCollector.RecordPhaseTransition(tenantID, className, curr.Phase)

		// Cold-start: Pending → Running uses Pod's Running timestamp
		// minus Sandbox creation time. Emits with both new runtime label
		// and legacy vmm label during the dual-write transition period.
		if curr.Phase == setecv1alpha1.SandboxPhaseRunning && !sb.CreationTimestamp.IsZero() {
			startTime := pod.CreationTimestamp.Time
			if curr.StartedAt != nil {
				startTime = curr.StartedAt.Time
			}
			if d := startTime.Sub(sb.CreationTimestamp.Time); d > 0 {
				r.MetricsCollector.ObserveColdStart(runtimeLabel, vmm, className, d)
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

// createPod builds the Pod spec via the pure podspec.Build helper, reconciles
// the OwnerReference to the live Sandbox UID/APIVersion via controllerutil
// (podspec.Build sets the basic OwnerReference fields but has no access to
// the Scheme needed to stamp the correct APIVersion; we re-apply it here so
// the authoritative source of truth is controller-runtime), and creates the
// Pod. The next reconcile will observe the new Pod via the Owns watch.
//
// cls is optional: when non-nil the Pod uses the class's RuntimeClassName
// override (if set) and inherits the class's NodeSelector. nodeName, when
// non-empty, pins the Pod to a specific node — used by the Phase 3
// snapshot-restore flow to land on the node holding the snapshot state.
// sel carries the dispatcher-selected backend and is applied via
// podspec.WithRuntimeSelection as the last option in the build pipeline.
func (r *SandboxReconciler) createPod(
	ctx context.Context,
	sb *setecv1alpha1.Sandbox,
	cls *setecv1alpha1.SandboxClass,
	nodeName string,
	sel *runtimepkg.Selection,
) (ctrl.Result, error) {
	// Determine the runtimeClassName: prefer the dispatcher's value (from sel),
	// then the class override, then the legacy operator default.
	rcName := ""
	if sel != nil {
		rcName = sel.Dispatcher.RuntimeClassName()
	}
	if rcName == "" && cls != nil && cls.Spec.RuntimeClassName != "" { //nolint:staticcheck // back-compat: RuntimeClassName retained until v2
		rcName = cls.Spec.RuntimeClassName //nolint:staticcheck // back-compat: RuntimeClassName retained until v2
	}

	opts := podspec.BuildOptions{NodeName: nodeName}
	if sel != nil {
		opts.RuntimeSelection = sel
	}
	pod, err := podspec.BuildWithOptions(sb, rcName, opts)
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

	// podspec.Build already populates a basic OwnerReference, but UID and
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
	return r.Status().Patch(ctx, sb, client.MergeFrom(original))
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
			// Already created. Honour the AfterCreate intent without
			// re-snapshotting.
			return ctrl.Result{}, nil
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
		default: // Running
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
	return r.Status().Patch(ctx, sb, client.MergeFrom(original))
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
	return ctrl.NewControllerManagedBy(mgr).
		For(&setecv1alpha1.Sandbox{}).
		Owns(&corev1.Pod{}).
		Owns(&networkingv1.NetworkPolicy{}).
		Complete(r)
}
