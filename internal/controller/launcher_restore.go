// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"

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

// restoreSource returns the Snapshot that the launcher of sb loads: its
// snapshotRef, or the warm pool base the operator chose for it. It returns
// "" when sb boots.
func restoreSource(sb *setecv1alpha1.Sandbox) types.NamespacedName {
	if sb.Spec.SnapshotRef != nil && sb.Spec.SnapshotRef.Name != "" {
		return types.NamespacedName{Namespace: sb.Namespace, Name: sb.Spec.SnapshotRef.Name}
	}
	if ns, name, ok := strings.Cut(sb.Annotations[WarmBaseAnnotation], "/"); ok {
		return types.NamespacedName{Namespace: ns, Name: name}
	}
	return types.NamespacedName{}
}

// restoredValue is the value of RestoredAnnotation for a source: the
// Snapshot name for a snapshotRef, <namespace>/<name> for a base.
func restoredValue(sb *setecv1alpha1.Sandbox, src types.NamespacedName) string {
	if src.Namespace == sb.Namespace {
		return src.Name
	}
	return src.String()
}

// needsLauncherRestore reports whether sb is a launcher Sandbox that must
// load a snapshot before a caller gets it.
func needsLauncherRestore(sb *setecv1alpha1.Sandbox) bool {
	src := restoreSource(sb)
	return src.Name != "" &&
		sb.Status.Runtime != nil && sb.Status.Runtime.Chosen == runtimepkg.BackendLauncher &&
		sb.Annotations[RestoredAnnotation] != restoredValue(sb, src)
}

// markWarmBase records the base that the launcher of sb loads, or removes
// a stale record when there is no base.
func (r *SandboxReconciler) markWarmBase(ctx context.Context, sb *setecv1alpha1.Sandbox, base *setecv1alpha1.Snapshot) error {
	want := ""
	if base != nil {
		want = base.Namespace + "/" + base.Name
	}
	if sb.Annotations[WarmBaseAnnotation] == want {
		return nil
	}
	original := sb.DeepCopy()
	if want == "" {
		delete(sb.Annotations, WarmBaseAnnotation)
	} else {
		if sb.Annotations == nil {
			sb.Annotations = map[string]string{}
		}
		sb.Annotations[WarmBaseAnnotation] = want
	}
	if err := r.Patch(ctx, sb, client.MergeFrom(original)); err != nil {
		return fmt.Errorf("record the warm pool base: %w", err)
	}
	return nil
}

// maybeRestoreLauncher is the restore step of a launcher Sandbox with a
// snapshotRef (setec#105). Its launcher Pod waits for the snapshot. Once
// the Pod runs, the node agent stages the snapshot, the launcher loads it
// and confirms the guest, and the invariant gate decides. The Sandbox is
// Running only after all of that and after the Pod is Ready. A failure fails the Sandbox, and step
// (12) of the reconcile deletes the Pod.
func (r *SandboxReconciler) maybeRestoreLauncher(
	ctx context.Context,
	sb *setecv1alpha1.Sandbox,
	pod *corev1.Pod,
	desired setecv1alpha1.SandboxStatus,
) setecv1alpha1.SandboxStatus {
	// The launcher container runs and waits for the snapshot. The Pod is
	// not Ready until the guest answers, so the Pod phase, not the
	// Sandbox phase, starts the restore.
	if !needsLauncherRestore(sb) || pod == nil || pod.Status.Phase != corev1.PodRunning ||
		desired.Phase == setecv1alpha1.SandboxPhaseFailed {
		return desired
	}
	var fail func(reason string, err error) setecv1alpha1.SandboxStatus
	fail = func(reason string, err error) setecv1alpha1.SandboxStatus {
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
	src := restoreSource(sb)
	warm := src.Namespace != sb.Namespace
	if warm {
		fail = r.failWarm(ctx, sb, src, fail)
	}
	snap := &setecv1alpha1.Snapshot{}
	if err := r.Get(ctx, src, snap); err != nil {
		return fail(ReasonRestoreFailed, err)
	}
	if err := r.Coordinator.RestoreSandbox(ctx, sb, snap); err != nil {
		if errors.Is(err, snapshot.ErrInvariantGateViolation) {
			return fail(status.ReasonInvariantGateViolation, err)
		}
		return fail(ReasonRestoreFailed, err)
	}
	if warm {
		desired.WarmStart = &setecv1alpha1.SandboxWarmStartStatus{
			Outcome: setecv1alpha1.SandboxWarmStartPoolRestored, EntryID: src.String(),
		}
		r.countWarmStart(r.classOrNil(ctx, sb), "restored")
	}
	original := sb.DeepCopy()
	if sb.Annotations == nil {
		sb.Annotations = map[string]string{}
	}
	sb.Annotations[RestoredAnnotation] = restoredValue(sb, src)
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

// classCPUTemplate returns the CPU template of a class, or "".
func classCPUTemplate(cls *setecv1alpha1.SandboxClass) string {
	if cls == nil {
		return ""
	}
	return cls.Spec.CPUTemplate
}

// classRequests is the scheduler reservation of the class, or nil.
func classRequests(cls *setecv1alpha1.SandboxClass) *setecv1alpha1.ResourceRequests {
	if cls == nil {
		return nil
	}
	return cls.Spec.Requests
}

// restoreInstanceType returns the instance type that a restore Pod needs:
// the one of the source node of its snapshot, when the class has no CPU
// template. A template shows every node the same CPU, so it needs none.
func (r *SandboxReconciler) restoreInstanceType(
	ctx context.Context, sb *setecv1alpha1.Sandbox, cls *setecv1alpha1.SandboxClass,
) string {
	if sb.Spec.SnapshotRef == nil || sb.Spec.SnapshotRef.Name == "" || classCPUTemplate(cls) != "" {
		return ""
	}
	snap := &setecv1alpha1.Snapshot{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: sb.Namespace, Name: sb.Spec.SnapshotRef.Name}, snap); err != nil {
		return ""
	}
	return snap.Spec.InstanceType
}

// failWarm wraps fail for a warm start: the Sandbox records the rejected
// base. Its machine already holds the state of the base, so a cold boot
// is not a safe fallback, and the Sandbox fails.
func (r *SandboxReconciler) failWarm(
	_ context.Context, sb *setecv1alpha1.Sandbox, src types.NamespacedName,
	fail func(string, error) setecv1alpha1.SandboxStatus,
) func(string, error) setecv1alpha1.SandboxStatus {
	return func(reason string, err error) setecv1alpha1.SandboxStatus {
		r.countWarmStart(r.classOrNil(context.Background(), sb), "error")
		desired := fail(reason, err)
		desired.WarmStart = &setecv1alpha1.SandboxWarmStartStatus{
			Outcome: setecv1alpha1.SandboxWarmStartRejected, EntryID: src.String(), Reason: reason,
		}
		return desired
	}
}

// classOrNil returns the SandboxClass of sb, or nil.
func (r *SandboxReconciler) classOrNil(ctx context.Context, sb *setecv1alpha1.Sandbox) *setecv1alpha1.SandboxClass {
	if sb.Spec.SandboxClassName == "" {
		return nil
	}
	cls := &setecv1alpha1.SandboxClass{}
	if err := r.Get(ctx, types.NamespacedName{Name: sb.Spec.SandboxClassName}, cls); err != nil {
		return nil
	}
	return cls
}

// isLauncherSandbox reports whether the machine of sb runs in a launcher
// Pod.
func isLauncherSandbox(sb *setecv1alpha1.Sandbox) bool {
	return sb.Status.Runtime != nil && sb.Status.Runtime.Chosen == runtimepkg.BackendLauncher
}
