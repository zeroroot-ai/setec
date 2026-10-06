// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package controller

import (
	"context"
	"fmt"
	"math"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
	"github.com/zeroroot-ai/setec/internal/errwrap"
	"github.com/zeroroot-ai/setec/internal/snapshot"
)

const (
	// SnapshotSandboxRefIndex is the field-indexer key pointing
	// Sandbox CRs at their referenced Snapshot name. The
	// SnapshotReconciler uses it to compute Status.ReferenceCount in
	// O(references) time.
	SnapshotSandboxRefIndex = "spec.snapshotRef.name"
	// SnapshotParentIndex keys each Snapshot by the parent that its diff
	// builds on. A parent with a diff is in use.
	SnapshotParentIndex = "spec.parent"

	snapshotTTLRequeue     = 60 * time.Second
	snapshotErrorRequeue   = 30 * time.Second
	eventReasonSnapshotDel = "SnapshotDeleted"
)

// SnapshotReconciler drives the lifecycle of a Snapshot CR:
// finalizer management, TTL expiry, and delegation of the underlying
// on-disk erase to the Coordinator.
//
// The reconciler keeps its own field indexer so "how many Sandboxes
// reference this Snapshot" is answerable without a full List walk on
// every reconcile.
type SnapshotReconciler struct {
	client.Client
	Scheme      *runtime.Scheme
	Recorder    events.EventRecorder
	Coordinator *snapshot.Coordinator
}

// RBAC markers for the Snapshot controller.
//
// +kubebuilder:rbac:groups=setec.zeroroot.ai,resources=snapshots,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=setec.zeroroot.ai,resources=snapshots/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=setec.zeroroot.ai,resources=snapshots/finalizers,verbs=update

// Reconcile drives one Snapshot CR toward its desired state.
//
// Ordering:
//  1. Fetch. NotFound returns.
//  2. Recompute Status.ReferenceCount from the indexed Sandbox list.
//  3. If DeletionTimestamp != nil:
//     a. If ReferenceCount > 0, requeue (finalizer blocks).
//     b. Else, DeleteSnapshot via Coordinator, remove finalizer, return.
//  4. Ensure finalizer present on live Snapshots.
//  5. TTL: if Spec.TTL set and age > TTL and ReferenceCount == 0,
//     issue a Delete.
func (r *SnapshotReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("snapshot", req.NamespacedName)

	snap := &setecv1alpha1.Snapshot{}
	if err := r.Get(ctx, req.NamespacedName, snap); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("get Snapshot: %w", err)
	}

	// Step 1: compute the current reference count.
	count, err := r.referenceCount(ctx, snap)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("compute reference count: %w", err)
	}
	refs := int32(min(count, math.MaxInt32)) //nolint:gosec // min bounds count to MaxInt32
	if snap.Status.ReferenceCount != refs {
		original := snap.DeepCopy()
		snap.Status.ReferenceCount = refs
		now := metav1.NewTime(time.Now())
		snap.Status.LastTransitionTime = &now
		if err := r.Status().Patch(ctx, snap, client.MergeFrom(original)); err != nil {
			return ctrl.Result{}, fmt.Errorf("patch reference count: %w", err)
		}
	}

	// Step 2: deletion handling.
	if !snap.DeletionTimestamp.IsZero() {
		// Report Terminating for as long as the in-use finalizer is
		// held. Nothing wrote this phase before (setec#129), so a
		// snapshot blocked on a reference, or on a backend erase that
		// keeps failing, still read as Ready — indistinguishable from one
		// nobody had asked to delete. The write happens before the
		// reference check, because "deletion requested and blocked" is
		// exactly the state an operator needs to see.
		if snap.Status.Phase != setecv1alpha1.SnapshotPhaseTerminating {
			if err := r.markPhase(ctx, snap, setecv1alpha1.SnapshotPhaseTerminating, "DeletionInProgress"); err != nil {
				return ctrl.Result{}, fmt.Errorf("mark Snapshot Terminating: %w", err)
			}
		}
		if count > 0 {
			logger.V(1).Info("deletion blocked by referenceCount > 0",
				"referenceCount", count)
			return ctrl.Result{RequeueAfter: snapshotErrorRequeue}, nil
		}
		if r.Coordinator != nil {
			if err := r.Coordinator.DeleteSnapshot(ctx, snap); err != nil {
				// Retry on next reconcile. Finalizer remains so the
				// CR doesn't vanish with storage still present.
				if r.Recorder != nil {
					r.Recorder.Eventf(snap, nil, corev1.EventTypeWarning, eventReasonSnapshotDel, actionDeleteSnapshot, "%s", err.Error())
				}
				return ctrl.Result{RequeueAfter: snapshotErrorRequeue}, nil
			}
		}
		if controllerutil.RemoveFinalizer(snap, setecv1alpha1.SnapshotInUseFinalizer) {
			if err := r.Update(ctx, snap); err != nil {
				return ctrl.Result{}, fmt.Errorf("remove finalizer: %w", err)
			}
		}
		return ctrl.Result{}, nil
	}

	// Step 3: ensure finalizer present.
	if controllerutil.AddFinalizer(snap, setecv1alpha1.SnapshotInUseFinalizer) {
		if err := r.Update(ctx, snap); err != nil {
			return ctrl.Result{}, fmt.Errorf("add finalizer: %w", err)
		}
	}

	// Step 4: TTL check. TTL is only acted on when the snapshot has
	// no active Sandbox references; a referenced snapshot is kept
	// alive regardless of age.
	// A pinned kept Snapshot does not expire (setec#196).
	if snap.Spec.TTL != nil && snap.Spec.TTL.Duration > 0 && count == 0 && !snap.Spec.Pinned {
		age := time.Since(snap.CreationTimestamp.Time)
		if age >= snap.Spec.TTL.Duration {
			if err := r.Delete(ctx, snap); err != nil && !apierrors.IsNotFound(err) {
				return ctrl.Result{}, fmt.Errorf("ttl delete: %w", err)
			}
			return ctrl.Result{}, nil
		}
		// Requeue when TTL would fire. Use the remainder so we don't
		// hammer the API server.
		return ctrl.Result{RequeueAfter: snap.Spec.TTL.Duration - age}, nil
	}

	return ctrl.Result{RequeueAfter: snapshotTTLRequeue}, nil
}

// markPhase writes one phase/reason pair to the Snapshot status
// subresource and stamps the transition time. The status subresource is
// still writable while DeletionTimestamp is set, which is what makes
// Terminating reportable at all.
func (r *SnapshotReconciler) markPhase(ctx context.Context, snap *setecv1alpha1.Snapshot, phase setecv1alpha1.SnapshotPhase, reason string) error {
	original := snap.DeepCopy()
	snap.Status.Phase = phase
	snap.Status.Reason = reason
	now := metav1.NewTime(time.Now())
	snap.Status.LastTransitionTime = &now
	if err := r.Status().Patch(ctx, snap, client.MergeFrom(original)); err != nil {
		return fmt.Errorf("patch Snapshot status to %s: %w", phase, err)
	}
	return nil
}

// referenceCount returns the number of Sandboxes in the Snapshot's
// namespace whose spec.snapshotRef.name equals the Snapshot's name, plus
// the number of diff Snapshots whose spec.parent names it. Relies on the
// field indexers registered in SetupWithManager.
func (r *SnapshotReconciler) referenceCount(ctx context.Context, snap *setecv1alpha1.Snapshot) (int, error) {
	sbs := &setecv1alpha1.SandboxList{}
	if err := r.List(ctx, sbs,
		client.InNamespace(snap.Namespace),
		client.MatchingFields{SnapshotSandboxRefIndex: snap.Name},
	); err != nil {
		return 0, errwrap.Wrap(err, "client.Reader.List")
	}
	// A launcher Sandbox that has loaded the Snapshot no longer needs it,
	// so the TTL of a fork snapshot can end it (setec#195).
	inUse := 0
	for i := range sbs.Items {
		if sbs.Items[i].Annotations[RestoredAnnotation] != snap.Name {
			inUse++
		}
	}
	diffs := &setecv1alpha1.SnapshotList{}
	if err := r.List(ctx, diffs,
		client.InNamespace(snap.Namespace),
		client.MatchingFields{SnapshotParentIndex: snap.Name},
	); err != nil {
		return 0, errwrap.Wrap(err, "client.Reader.List")
	}
	return inUse + len(diffs.Items), nil
}

// SetupWithManager registers the reconciler and installs the field
// indexer the reference-count calculation depends on. Safe to call
// once per process; re-registering a duplicate indexer with the same
// name yields an error on purpose.
func (r *SnapshotReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(
		context.Background(),
		&setecv1alpha1.Sandbox{},
		SnapshotSandboxRefIndex,
		func(obj client.Object) []string {
			sb, ok := obj.(*setecv1alpha1.Sandbox)
			if !ok || sb.Spec.SnapshotRef == nil || sb.Spec.SnapshotRef.Name == "" {
				return nil
			}
			return []string{sb.Spec.SnapshotRef.Name}
		},
	); err != nil {
		return fmt.Errorf("index snapshotRef: %w", err)
	}
	if err := mgr.GetFieldIndexer().IndexField(
		context.Background(),
		&setecv1alpha1.Snapshot{},
		SnapshotParentIndex,
		func(obj client.Object) []string {
			s, ok := obj.(*setecv1alpha1.Snapshot)
			if !ok || s.Spec.Parent == "" {
				return nil
			}
			return []string{s.Spec.Parent}
		},
	); err != nil {
		return fmt.Errorf("index parent: %w", err)
	}

	// Enqueue the referenced Snapshot whenever a Sandbox changes so
	// ReferenceCount stays fresh without waiting for the ~60s
	// requeue-after. The mapping function reads spec.snapshotRef to
	// decide which Snapshot to notify; a Sandbox without a ref is a
	// no-op.
	return errwrap.Wrap(ctrl.NewControllerManagedBy(mgr).
		For(&setecv1alpha1.Snapshot{}, builder.WithPredicates()).
		WatchesRawSource(source.Kind(
			mgr.GetCache(),
			&setecv1alpha1.Sandbox{},
			handler.TypedEnqueueRequestsFromMapFunc(func(ctx context.Context, sb *setecv1alpha1.Sandbox) []reconcile.Request {
				if sb == nil || sb.Spec.SnapshotRef == nil || sb.Spec.SnapshotRef.Name == "" {
					return nil
				}
				return []reconcile.Request{{
					Namespace: sb.Namespace,
					Name:      sb.Spec.SnapshotRef.Name}}
			}),
		)).
		Complete(r), "builder.TypedBuilder.Complete")
}
