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

package controller

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
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
	"github.com/zeroroot-ai/setec/internal/snapshot"
)

const (
	// SnapshotSandboxRefIndex is the field-indexer key pointing
	// Sandbox CRs at their referenced Snapshot name. The
	// SnapshotReconciler uses it to compute Status.ReferenceCount in
	// O(references) time.
	SnapshotSandboxRefIndex = "spec.snapshotRef.name"

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
	if snap.Status.ReferenceCount != int32(count) {
		original := snap.DeepCopy()
		snap.Status.ReferenceCount = int32(count)
		now := metav1.NewTime(time.Now())
		snap.Status.LastTransitionTime = &now
		if err := r.Status().Patch(ctx, snap, client.MergeFrom(original)); err != nil {
			return ctrl.Result{}, fmt.Errorf("patch reference count: %w", err)
		}
	}

	// Step 2: deletion handling.
	if !snap.DeletionTimestamp.IsZero() {
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
	if snap.Spec.TTL != nil && snap.Spec.TTL.Duration > 0 && count == 0 {
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

// referenceCount returns the number of Sandboxes in the Snapshot's
// namespace whose spec.snapshotRef.name equals the Snapshot's name.
// Relies on the field indexer registered in SetupWithManager; the
// indexer keeps the lookup cheap even for namespaces with many
// Sandboxes.
func (r *SnapshotReconciler) referenceCount(ctx context.Context, snap *setecv1alpha1.Snapshot) (int, error) {
	sbs := &setecv1alpha1.SandboxList{}
	if err := r.List(ctx, sbs,
		client.InNamespace(snap.Namespace),
		client.MatchingFields{SnapshotSandboxRefIndex: snap.Name},
	); err != nil {
		return 0, err
	}
	return len(sbs.Items), nil
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

	// Enqueue the referenced Snapshot whenever a Sandbox changes so
	// ReferenceCount stays fresh without waiting for the ~60s
	// requeue-after. The mapping function reads spec.snapshotRef to
	// decide which Snapshot to notify; a Sandbox without a ref is a
	// no-op.
	return ctrl.NewControllerManagedBy(mgr).
		For(&setecv1alpha1.Snapshot{}, builder.WithPredicates()).
		WatchesRawSource(source.Kind(
			mgr.GetCache(),
			&setecv1alpha1.Sandbox{},
			handler.TypedEnqueueRequestsFromMapFunc(func(ctx context.Context, sb *setecv1alpha1.Sandbox) []reconcile.Request {
				if sb == nil || sb.Spec.SnapshotRef == nil || sb.Spec.SnapshotRef.Name == "" {
					return nil
				}
				return []reconcile.Request{{NamespacedName: types.NamespacedName{
					Namespace: sb.Namespace,
					Name:      sb.Spec.SnapshotRef.Name,
				}}}
			}),
		)).
		Complete(r)
}
