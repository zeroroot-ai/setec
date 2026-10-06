// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package controller

import (
	"context"
	"errors"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
	runtimepkg "github.com/zeroroot-ai/setec/internal/runtime"
	"github.com/zeroroot-ai/setec/internal/snapshot"
	"github.com/zeroroot-ai/setec/internal/status"
)

// RestoredAnnotation records on a Sandbox the Snapshot that its launcher
// loaded and that passed the invariant gate. Its presence ends the restore.
const RestoredAnnotation = "setec.zeroroot.ai/restored-from"

// Reasons of a launcher restore.
const (
	ReasonRestoring     = "Restoring"
	ReasonRestoreFailed = "RestoreFailed"
)

// isLocalSnapshot reports whether a Snapshot lives on the disk of one node.
// Such a Snapshot pins the restore Pod to that node. A Snapshot in the
// S3-compatible store loads on any node of the class.
func isLocalSnapshot(snap *setecv1alpha1.Snapshot) bool {
	return snap.Spec.StorageBackend == "" || snap.Spec.StorageBackend == "local-disk"
}

// needsLauncherRestore reports whether sb is a launcher Sandbox that must
// load its snapshotRef before a caller gets it.
func needsLauncherRestore(sb *setecv1alpha1.Sandbox) bool {
	return sb.Spec.SnapshotRef != nil && sb.Spec.SnapshotRef.Name != "" &&
		sb.Status.Runtime != nil && sb.Status.Runtime.Chosen == runtimepkg.BackendLauncher &&
		sb.Annotations[RestoredAnnotation] != sb.Spec.SnapshotRef.Name
}

// maybeRestoreLauncher is the restore step of a launcher Sandbox with a
// snapshotRef (setec#105). Its launcher Pod waits for the snapshot. Once
// the Pod runs, the node agent stages the snapshot, the launcher loads it
// and confirms the guest, and the invariant gate decides. The Sandbox is
// Running only after all of that. A failure fails the Sandbox, and step
// (12) of the reconcile deletes the Pod.
func (r *SandboxReconciler) maybeRestoreLauncher(
	ctx context.Context,
	sb *setecv1alpha1.Sandbox,
	desired setecv1alpha1.SandboxStatus,
) setecv1alpha1.SandboxStatus {
	if !needsLauncherRestore(sb) || desired.Phase != setecv1alpha1.SandboxPhaseRunning {
		return desired
	}
	fail := func(reason string, err error) setecv1alpha1.SandboxStatus {
		log.FromContext(ctx).Error(err, "launcher restore failed", "sandbox", sb.Name)
		if r.Recorder != nil {
			r.Recorder.Eventf(sb, nil, corev1.EventTypeWarning, reason, actionResolveSnapshot, "%s", err.Error())
		}
		desired.Phase = setecv1alpha1.SandboxPhaseFailed
		desired.Reason = reason
		return desired
	}
	if r.Coordinator == nil {
		return fail(ReasonRestoreFailed, errors.New("the snapshot coordinator is off, so no Sandbox can load a snapshot"))
	}
	snap := &setecv1alpha1.Snapshot{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: sb.Namespace, Name: sb.Spec.SnapshotRef.Name}, snap); err != nil {
		return fail(ReasonRestoreFailed, err)
	}
	if err := r.Coordinator.RestoreSandbox(ctx, sb, snap); err != nil {
		if errors.Is(err, snapshot.ErrInvariantGateViolation) {
			return fail(status.ReasonInvariantGateViolation, err)
		}
		return fail(ReasonRestoreFailed, err)
	}
	original := sb.DeepCopy()
	if sb.Annotations == nil {
		sb.Annotations = map[string]string{}
	}
	sb.Annotations[RestoredAnnotation] = snap.Name
	if err := r.Patch(ctx, sb, client.MergeFrom(original)); err != nil {
		// The restore passed. The next reconcile finds no annotation and
		// would restore again into a Pod whose launcher no longer waits,
		// so the Sandbox fails instead.
		return fail(ReasonRestoreFailed, err)
	}
	return desired
}

// holdUntilRestored keeps a launcher Sandbox with a snapshotRef Pending
// while its Pod has not started yet.
func holdUntilRestored(sb *setecv1alpha1.Sandbox, desired setecv1alpha1.SandboxStatus) setecv1alpha1.SandboxStatus {
	if needsLauncherRestore(sb) && desired.Phase == setecv1alpha1.SandboxPhasePending && desired.Reason == "" {
		desired.Reason = ReasonRestoring
	}
	return desired
}
