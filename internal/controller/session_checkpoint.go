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
	"crypto/rand"
	"errors"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
	"github.com/zeroroot-ai/setec/internal/podspec"
	"github.com/zeroroot-ai/setec/internal/snapshot"
	"github.com/zeroroot-ai/setec/internal/snapshot/atrest"
	"github.com/zeroroot-ai/setec/internal/status"
)

// Session-checkpoint machinery (setec#194, ADR-0006 L2 / ADR-0007).
//
// A session Sandbox whose class enables spec.sessionCheckpoint gets:
//
//   - a cluster-scoped per-session KEK Secret, created with the
//     session and deleted at session end (deleting it crypto-erases
//     every checkpoint it sealed);
//   - periodic memory checkpoints while Running (class-tunable
//     interval; infrequent by design — the durable workspace already
//     provides continuous data safety);
//   - suspend-on-idle: the class's sessionIdleTimeout deadline
//     (defined by setec#193) suspends the session — checkpoint,
//     release the microVM — instead of hard-failing it. Reattach
//     activity resumes it transparently;
//   - checkpoint-on-drain: a cordoned node or an evicted VM Pod
//     triggers an immediate checkpoint; the session resumes on
//     whichever node the scheduler picks next;
//   - degraded recovery as a DISTINCT condition: a VM lost with no
//     usable checkpoint restarts from the durable workspace and says
//     so (RestartedFromWorkspace) — data is never lost, only process
//     state since the last checkpoint.
const (
	// sessionKEKSuffix names the per-session KEK Secret
	// "<sandbox>-session-kek" in the Sandbox's namespace.
	sessionKEKSuffix = "-session-kek"

	// sessionKEKKey is the Secret data key holding the 32-byte KEK.
	sessionKEKKey = "kek"

	// annotationSuspendedPod marks a VM Pod the suspend machinery
	// itself deleted (checkpoint already taken). The drain detector
	// ignores Pods carrying it, so the controller's own suspend can
	// never be mistaken for a node drain — reconciles racing a stale
	// cache would otherwise double-checkpoint.
	annotationSuspendedPod = "setec.zeroroot.ai/suspended"

	// suspendWaitRequeue is how long a suspended Sandbox waits for its
	// terminating Pod to disappear between checks.
	suspendWaitRequeue = 2 * time.Second

	// Suspend reasons recorded on status.reason while Suspended. They
	// drive the resume policy: an idle suspend resumes on fresh
	// activity, a drain suspend resumes immediately (elsewhere), a
	// user suspend resumes only when desiredState says Running.
	reasonSuspendedIdle     = "SuspendedIdle"
	reasonUserSuspended     = "UserSuspended"
	reasonCheckpointOnDrain = "CheckpointOnDrain"
	// reasonSuspendedPauseTimeout marks a session suspended because it
	// sat Paused past the class maxPauseDuration (setec#202): the cap
	// bounds paused microVM residency, and with checkpoints enabled the
	// bound suspends — checkpoint retained, VM released — instead of
	// hard-failing. The session resumes when desiredState returns to
	// Running; while desiredState stays Paused it holds Suspended, so
	// the cap cannot ping-pong it through pause/suspend cycles.
	reasonSuspendedPauseTimeout = "SuspendedPauseTimeout"

	// Event reasons.
	eventReasonSuspended              = "SessionSuspended"
	eventReasonResumedFromCheckpoint  = "SessionResumedFromCheckpoint"
	eventReasonRestartedFromWorkspace = "SessionRestartedFromWorkspace"
	eventReasonCheckpointTaken        = "SessionCheckpointTaken"
	eventReasonSessionKEKCreated      = "SessionKEKCreated"
	eventReasonSessionKEKDeleted      = "SessionKEKDeleted"
)

// sessionCheckpointPolicy returns the class's checkpoint spec when the
// Sandbox is a session and the class enables checkpoints; nil
// otherwise.
func sessionCheckpointPolicy(sb *setecv1alpha1.Sandbox, cls *setecv1alpha1.SandboxClass) *setecv1alpha1.SessionCheckpointSpec {
	if sb == nil || cls == nil || !sb.Spec.IsSession() {
		return nil
	}
	return cls.Spec.SessionCheckpoint
}

// sessionKEKName renders the per-session KEK Secret name.
func sessionKEKName(sb *setecv1alpha1.Sandbox) string {
	return sb.Name + sessionKEKSuffix
}

// ensureSessionKEK creates the per-session KEK Secret when it does not
// exist. The Secret is owner-referenced to the Sandbox as defense in
// depth, but its authoritative teardown is teardownWorkspace, which
// deletes it BEFORE releasing the finalizer — that deletion is the
// cryptographic erasure of every checkpoint sealed under it.
func (r *SandboxReconciler) ensureSessionKEK(ctx context.Context, sb *setecv1alpha1.Sandbox) error {
	existing := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Namespace: sb.Namespace, Name: sessionKEKName(sb)}, existing)
	if err == nil {
		if !existing.DeletionTimestamp.IsZero() {
			return fmt.Errorf("session KEK Secret %q is terminating; a session key is never reused", sessionKEKName(sb))
		}
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get session KEK Secret: %w", err)
	}

	kek := make([]byte, atrest.KeySize)
	if _, err := rand.Read(kek); err != nil {
		return fmt.Errorf("generate session KEK: %w", err)
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sessionKEKName(sb),
			Namespace: sb.Namespace,
			Labels:    map[string]string{podspec.SandboxLabelKey: sb.Name},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{sessionKEKKey: kek},
	}
	if err := controllerutil.SetControllerReference(sb, secret, r.Scheme); err != nil {
		return fmt.Errorf("set owner on session KEK Secret: %w", err)
	}
	if err := r.Create(ctx, secret); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create session KEK Secret: %w", err)
	}
	r.Recorder.Eventf(sb, nil, corev1.EventTypeNormal, eventReasonSessionKEKCreated, actionManageCheckpoint,
		"Created per-session checkpoint KEK Secret %q", sessionKEKName(sb))
	return nil
}

// readSessionKEK returns the per-session KEK bytes.
func (r *SandboxReconciler) readSessionKEK(ctx context.Context, sb *setecv1alpha1.Sandbox) ([]byte, error) {
	secret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: sb.Namespace, Name: sessionKEKName(sb)}, secret); err != nil {
		return nil, fmt.Errorf("read session KEK Secret: %w", err)
	}
	kek := secret.Data[sessionKEKKey]
	if len(kek) != atrest.KeySize {
		return nil, fmt.Errorf("session KEK Secret %q holds %d bytes, want %d", sessionKEKName(sb), len(kek), atrest.KeySize)
	}
	return kek, nil
}

// reconcileSessionCheckpoint drives the checkpoint/suspend/resume state
// machine for one session Sandbox whose class enables checkpoints. It
// runs after status derivation. handled=true means this reconcile is
// complete (an action was taken or a wait is in progress) and the
// caller must return the result.
func (r *SandboxReconciler) reconcileSessionCheckpoint(
	ctx context.Context,
	logger logr.Logger,
	sb *setecv1alpha1.Sandbox,
	cls *setecv1alpha1.SandboxClass,
	pod *corev1.Pod,
	desired setecv1alpha1.SandboxStatus,
) (ctrl.Result, bool, error) {
	policy := sessionCheckpointPolicy(sb, cls)
	if policy == nil || r.Coordinator == nil {
		return ctrl.Result{}, false, nil
	}

	// (The "suspended while the old Pod terminates" wait lives in
	// reconcileExistingPod step 9a, ahead of status derivation.)

	// A Pod our own suspend already condemned is not signal for
	// anything below — a stale cache can rerun this reconcile before
	// the Suspended status lands, and acting on the dying Pod again
	// would double-checkpoint.
	if pod.Annotations[annotationSuspendedPod] != "" {
		return ctrl.Result{RequeueAfter: suspendWaitRequeue}, true, nil
	}

	// (b) Restore-on-running-edge: the fresh VM is up and a restore is
	// pending — load the checkpoint into it (transparent resume), or
	// degrade to restart-from-workspace as a distinct condition.
	if ck := sb.Status.Checkpoint; ck != nil && ck.PendingRestore &&
		desired.Phase == setecv1alpha1.SandboxPhaseRunning &&
		pod.Status.Phase == corev1.PodRunning && pod.DeletionTimestamp.IsZero() {
		return r.restorePendingCheckpoint(ctx, logger, sb, policy)
	}

	// (c) Checkpoint-on-drain: the VM Pod is being evicted (node
	// drain, preemption) or its node was cordoned. Checkpoint while
	// the VM is still alive and suspend; the resume policy brings the
	// session back immediately on another node.
	if desired.Phase == setecv1alpha1.SandboxPhaseRunning || desired.Phase == setecv1alpha1.SandboxPhasePaused {
		// Only an eviction we did NOT initiate counts as a drain.
		draining := !pod.DeletionTimestamp.IsZero()
		if !draining && pod.Spec.NodeName != "" {
			node := &corev1.Node{}
			if err := r.Get(ctx, types.NamespacedName{Name: pod.Spec.NodeName}, node); err == nil && node.Spec.Unschedulable {
				draining = true
			}
		}
		if draining {
			res, err := r.suspendSession(ctx, logger, sb, policy, pod, reasonCheckpointOnDrain)
			return res, true, err
		}
	}

	// (d) Explicit suspend request.
	if sb.Spec.DesiredState == setecv1alpha1.SandboxDesiredStateSuspended &&
		(desired.Phase == setecv1alpha1.SandboxPhaseRunning || desired.Phase == setecv1alpha1.SandboxPhasePaused) {
		res, err := r.suspendSession(ctx, logger, sb, policy, pod, reasonUserSuspended)
		return res, true, err
	}

	// (d2) Pause-timeout suspend (setec#202): a session sitting Paused
	// past the class maxPauseDuration has held a paused microVM's full
	// memory reservation for the whole cap. With checkpoints enabled
	// the cap suspends instead of hard-failing: checkpoint, release the
	// microVM, keep the session recoverable (ADR-0006 L2). The suspend
	// holds until desiredState returns to Running.
	if desired.Phase == setecv1alpha1.SandboxPhasePaused {
		if deadline, ok := status.PauseDeadline(desired, cls); ok && !time.Now().Before(deadline) {
			res, err := r.suspendSession(ctx, logger, sb, policy, pod, reasonSuspendedPauseTimeout)
			return res, true, err
		}
	}

	// (e) Suspend-on-idle: consume setec#193's idle signal — the
	// last-activity clock against the class sessionIdleTimeout. With
	// checkpoints enabled the deadline suspends instead of evicting.
	if desired.Phase == setecv1alpha1.SandboxPhaseRunning {
		if deadline, ok := status.SessionIdleDeadline(sb, cls); ok && !time.Now().Before(deadline) {
			res, err := r.suspendSession(ctx, logger, sb, policy, pod, reasonSuspendedIdle)
			return res, true, err
		}
	}

	// (f) Periodic checkpoint while Running.
	if desired.Phase == setecv1alpha1.SandboxPhaseRunning &&
		policy.Interval != nil && policy.Interval.Duration > 0 {
		due := true
		if ck := sb.Status.Checkpoint; ck != nil && ck.TakenAt != nil {
			due = time.Since(ck.TakenAt.Time) >= policy.Interval.Duration
		} else if sb.Status.StartedAt != nil {
			// First checkpoint one full interval after the VM started.
			due = time.Since(sb.Status.StartedAt.Time) >= policy.Interval.Duration
		}
		if due {
			if err := r.takeCheckpoint(ctx, logger, sb, policy); err != nil {
				// A failed periodic checkpoint must not kill a healthy
				// session: record and retry on the next interval tick.
				logger.Error(err, "periodic session checkpoint failed; VM keeps running")
			}
			return ctrl.Result{RequeueAfter: policy.Interval.Duration}, true, nil
		}
		// Not due yet — make sure a quiet session still gets its next
		// checkpoint on time.
		next := time.Until(nextCheckpointDue(sb, policy))
		if next < time.Second {
			next = time.Second
		}
		return ctrl.Result{RequeueAfter: next}, false, nil
	}

	return ctrl.Result{}, false, nil
}

// nextCheckpointDue computes when the next periodic checkpoint is due.
func nextCheckpointDue(sb *setecv1alpha1.Sandbox, policy *setecv1alpha1.SessionCheckpointSpec) time.Time {
	base := time.Now()
	if ck := sb.Status.Checkpoint; ck != nil && ck.TakenAt != nil {
		base = ck.TakenAt.Time
	} else if sb.Status.StartedAt != nil {
		base = sb.Status.StartedAt.Time
	}
	return base.Add(policy.Interval.Duration)
}

// takeCheckpoint persists a fresh memory checkpoint of the Running
// session VM (which resumes immediately after the state files land)
// and replaces the previous checkpoint: the new one is saved first,
// then the old one is destroyed, so there is never a moment with zero
// checkpoints because a replacement failed.
func (r *SandboxReconciler) takeCheckpoint(
	ctx context.Context,
	logger logr.Logger,
	sb *setecv1alpha1.Sandbox,
	policy *setecv1alpha1.SessionCheckpointSpec,
) error {
	if err := r.ensureSessionKEK(ctx, sb); err != nil {
		return err
	}
	kek, err := r.readSessionKEK(ctx, sb)
	if err != nil {
		return err
	}

	prev := sb.Status.Checkpoint
	seq := int64(1)
	if prev != nil {
		seq = prev.Sequence + 1
	}
	backend := policy.CheckpointBackend()

	ref, size, err := r.Coordinator.CheckpointSession(ctx, sb, backend, seq, kek)
	if err != nil {
		return fmt.Errorf("checkpoint session: %w", err)
	}

	// New checkpoint is durable; destroy the superseded one (its DEK
	// first, via the backend's Delete). Best-effort: a failed cleanup
	// never invalidates the fresh checkpoint.
	if prev != nil && prev.Ref != "" {
		if delErr := r.Coordinator.DeleteSessionCheckpoint(ctx, sb, prev.Ref, prev.Backend); delErr != nil {
			logger.Error(delErr, "failed to delete superseded session checkpoint", "ref", prev.Ref)
		}
	}

	now := metav1.Now()
	original := sb.DeepCopy()
	ck := &setecv1alpha1.SandboxCheckpointStatus{
		Ref:       ref,
		Backend:   backend,
		Sequence:  seq,
		TakenAt:   &now,
		SizeBytes: size,
	}
	if prev != nil {
		ck.LastRecovery = prev.LastRecovery
	}
	sb.Status.Checkpoint = ck
	if err := r.Status().Patch(ctx, sb, client.MergeFrom(original)); err != nil {
		return fmt.Errorf("patch checkpoint status: %w", err)
	}
	r.Recorder.Eventf(sb, nil, corev1.EventTypeNormal, eventReasonCheckpointTaken, actionManageCheckpoint,
		"Session checkpoint #%d persisted (%d bytes)", seq, size)
	return nil
}

// suspendSession checkpoints the session VM and releases it: take a
// fresh checkpoint, delete the Pod, and mark the Sandbox Suspended
// with a PendingRestore checkpoint. When the checkpoint cannot be
// taken on a drain (the VM may already be dying), the suspend still
// proceeds — the session then resumes degraded from its durable
// workspace, surfaced as the distinct RestartedFromWorkspace
// condition. For idle/user suspends a failed checkpoint aborts the
// suspend instead: the VM is healthy, so losing process state for a
// cost optimization is never acceptable.
func (r *SandboxReconciler) suspendSession(
	ctx context.Context,
	logger logr.Logger,
	sb *setecv1alpha1.Sandbox,
	policy *setecv1alpha1.SessionCheckpointSpec,
	pod *corev1.Pod,
	reason string,
) (ctrl.Result, error) {
	ckErr := r.takeCheckpoint(ctx, logger, sb, policy)
	if ckErr != nil {
		if reason != reasonCheckpointOnDrain {
			return r.recordAndReturnErr(sb, eventReasonReconcileError,
				fmt.Errorf("suspend aborted, checkpoint failed: %w", ckErr))
		}
		logger.Error(ckErr, "checkpoint-on-drain failed; session will restart from durable workspace on another node")
	}

	if pod.DeletionTimestamp.IsZero() {
		// Stamp the Pod as suspend-condemned BEFORE deleting it so no
		// later reconcile can mistake the deletion for a node drain.
		original := pod.DeepCopy()
		if pod.Annotations == nil {
			pod.Annotations = map[string]string{}
		}
		pod.Annotations[annotationSuspendedPod] = "true"
		if err := r.Patch(ctx, pod, client.MergeFrom(original)); err != nil && !apierrors.IsNotFound(err) {
			return r.recordAndReturnErr(sb, eventReasonReconcileError, fmt.Errorf("mark Pod for suspend: %w", err))
		}
		if err := r.Delete(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
			return r.recordAndReturnErr(sb, eventReasonReconcileError, fmt.Errorf("delete Pod for suspend: %w", err))
		}
	}

	original := sb.DeepCopy()
	if sb.Status.Checkpoint == nil {
		sb.Status.Checkpoint = &setecv1alpha1.SandboxCheckpointStatus{Backend: policy.CheckpointBackend()}
	}
	sb.Status.Checkpoint.PendingRestore = sb.Status.Checkpoint.Ref != "" && ckErr == nil
	sb.Status.Phase = setecv1alpha1.SandboxPhaseSuspended
	sb.Status.Reason = reason
	// A Suspended session holds no microVM, so it is outside the
	// maxPauseDuration bound: clear the pause stamp a suspend-from-
	// Paused would otherwise carry along (a later re-pause restamps it).
	sb.Status.PausedAt = nil
	now := metav1.Now()
	sb.Status.LastTransitionTime = &now
	if err := r.Status().Patch(ctx, sb, client.MergeFrom(original)); err != nil {
		return ctrl.Result{}, fmt.Errorf("patch Suspended status: %w", err)
	}
	r.Recorder.Eventf(sb, nil, corev1.EventTypeNormal, eventReasonSuspended, actionManageCheckpoint,
		"Session suspended (%s); microVM released, checkpoint retained", reason)
	logger.Info("session suspended", "reason", reason, "checkpointed", ckErr == nil)
	return ctrl.Result{RequeueAfter: suspendWaitRequeue}, nil
}

// restorePendingCheckpoint loads the pending checkpoint into the fresh
// session VM. The checkpoint is CONSUMED by the attempt no matter how
// it ends (ADR-0005 forbids restoring the same state twice): on
// success the session continues (ResumedFromCheckpoint); on failure
// the already-running cold-booted VM carries on against the durable
// workspace, surfaced as the distinct RestartedFromWorkspace
// condition. Either way the stored checkpoint objects are destroyed.
func (r *SandboxReconciler) restorePendingCheckpoint(
	ctx context.Context,
	logger logr.Logger,
	sb *setecv1alpha1.Sandbox,
	policy *setecv1alpha1.SessionCheckpointSpec,
) (ctrl.Result, bool, error) {
	ck := sb.Status.Checkpoint
	recovery := setecv1alpha1.SessionRecoveryResumedFromCheckpoint

	kek, kekErr := r.readSessionKEK(ctx, sb)
	var restoreErr error
	if kekErr != nil {
		restoreErr = kekErr
	} else {
		restoreErr = r.Coordinator.RestoreSessionCheckpoint(ctx, sb, ck.Ref, ck.Backend, kek)
	}
	switch {
	case errors.Is(restoreErr, snapshot.ErrInvariantGateViolation):
		// ADR-0005 invariant gate refusal: the VM already holds the
		// checkpoint state but its verifications did not pass, so the
		// VM is DESTROYED — never served. The session itself survives
		// per ADR-0006: the deleted Pod is recreated and the fresh VM
		// cold-boots against the durable workspace (the checkpoint is
		// consumed below either way).
		recovery = setecv1alpha1.SessionRecoveryRestartedFromWorkspace
		r.Recorder.Eventf(sb, nil, corev1.EventTypeWarning, eventReasonInvariantGateViolation, actionEnforceInvariantGate,
			"ADR-0005 invariant gate refused checkpoint resume #%d (%v); destroying the VM that received the unverified state — session restarts from durable workspace",
			ck.Sequence, restoreErr)
		logger.Error(restoreErr, "invariant gate refused session checkpoint resume; destroying VM")
		podName := sb.Status.PodName
		if podName == "" {
			podName = sb.Name + "-vm"
		}
		pod := &corev1.Pod{}
		if perr := r.Get(ctx, types.NamespacedName{Namespace: sb.Namespace, Name: podName}, pod); perr == nil && pod.DeletionTimestamp.IsZero() {
			if derr := r.Delete(ctx, pod); derr != nil && !apierrors.IsNotFound(derr) {
				return ctrl.Result{}, true, fmt.Errorf("delete Pod after invariant-gate refusal: %w", derr)
			}
		}
	case restoreErr != nil:
		recovery = setecv1alpha1.SessionRecoveryRestartedFromWorkspace
		r.Recorder.Eventf(sb, nil, corev1.EventTypeWarning, eventReasonRestartedFromWorkspace, actionManageCheckpoint,
			"Checkpoint restore failed (%v); session restarted from durable workspace — no data lost, process state since checkpoint #%d gone",
			restoreErr, ck.Sequence)
		logger.Error(restoreErr, "session checkpoint restore failed; degraded to restart-from-workspace")
	default:
		r.Recorder.Eventf(sb, nil, corev1.EventTypeNormal, eventReasonResumedFromCheckpoint, actionManageCheckpoint,
			"Session resumed from checkpoint #%d; process state intact", ck.Sequence)
	}

	// The checkpoint is consumed either way: destroy its objects.
	if delErr := r.Coordinator.DeleteSessionCheckpoint(ctx, sb, ck.Ref, ck.Backend); delErr != nil {
		logger.Error(delErr, "failed to delete consumed session checkpoint", "ref", ck.Ref)
	}

	original := sb.DeepCopy()
	sb.Status.Checkpoint = &setecv1alpha1.SandboxCheckpointStatus{
		Backend:      ck.Backend,
		Sequence:     ck.Sequence,
		LastRecovery: recovery,
	}
	if err := r.Status().Patch(ctx, sb, client.MergeFrom(original)); err != nil {
		return ctrl.Result{}, true, fmt.Errorf("patch post-restore checkpoint status: %w", err)
	}
	// Requeue so the periodic-interval branch re-establishes a fresh
	// checkpoint on its own schedule.
	if policy.Interval != nil && policy.Interval.Duration > 0 {
		return ctrl.Result{RequeueAfter: policy.Interval.Duration}, true, nil
	}
	return ctrl.Result{}, true, nil
}

// suspendedSandboxAction decides what handleMissingPod does with a
// Suspended session that currently has no Pod: stay suspended or
// resume (create a fresh Pod and restore).
//
//   - explicit desiredState=Suspended → stay;
//   - reason UserSuspended → resume once desiredState is Running;
//   - reason CheckpointOnDrain → resume immediately (the suspend was
//     never a caller intent — the node went away);
//   - reason SuspendedPauseTimeout → resume only once desiredState is
//     Running: while it stays Paused, resuming would just re-pause and
//     re-suspend in a loop at the maxPauseDuration cadence (setec#202);
//   - reason SuspendedIdle → resume when fresh activity arrived
//     (setec#193's last-activity clock moved past the suspend time):
//     a reattach or new call transparently wakes the session.
func suspendedSandboxAction(sb *setecv1alpha1.Sandbox) (resume bool) {
	if sb.Spec.DesiredState == setecv1alpha1.SandboxDesiredStateSuspended {
		return false
	}
	switch sb.Status.Reason {
	case reasonCheckpointOnDrain, reasonUserSuspended:
		return true
	case reasonSuspendedPauseTimeout:
		return sb.Spec.DesiredState == "" ||
			sb.Spec.DesiredState == setecv1alpha1.SandboxDesiredStateRunning
	case reasonSuspendedIdle:
		// Fresh activity at or after the suspend instant wakes the
		// session. The comparison is >= because both clocks serialize
		// at second granularity (RFC 3339 / metav1.Time): a reattach
		// landing within the same second as the suspend must still
		// wake it, and a pre-suspend activity stamp can never tie —
		// the session only suspended because that stamp was at least
		// a full idle window old.
		suspendedAt := time.Time{}
		if sb.Status.LastTransitionTime != nil {
			suspendedAt = sb.Status.LastTransitionTime.Time
		}
		return !status.LastSessionActivity(sb).Before(suspendedAt)
	default:
		return true
	}
}

// teardownSessionCheckpoint runs during session teardown: it deletes
// the per-session KEK Secret FIRST — the moment it is gone every
// checkpoint sealed under it is cryptographically erased — and then
// best-effort deletes the stored checkpoint objects. The ciphertext
// delete failing (store unreachable, no node-agent left) never blocks
// teardown, because the crypto-erase already happened.
func (r *SandboxReconciler) teardownSessionCheckpoint(
	ctx context.Context,
	logger logr.Logger,
	sb *setecv1alpha1.Sandbox,
) {
	secret := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Namespace: sb.Namespace, Name: sessionKEKName(sb)}, secret)
	switch {
	case err == nil:
		if secret.DeletionTimestamp.IsZero() {
			if delErr := r.Delete(ctx, secret); delErr != nil && !apierrors.IsNotFound(delErr) {
				logger.Error(delErr, "failed to delete session KEK Secret", "secret", sessionKEKName(sb))
			} else {
				r.Recorder.Eventf(sb, nil, corev1.EventTypeNormal, eventReasonSessionKEKDeleted, actionManageCheckpoint,
					"Deleted per-session checkpoint KEK Secret %q; all session checkpoints are cryptographically erased", sessionKEKName(sb))
			}
		}
	case !apierrors.IsNotFound(err):
		logger.Error(err, "failed to read session KEK Secret during teardown")
	}

	if ck := sb.Status.Checkpoint; ck != nil && ck.Ref != "" && r.Coordinator != nil {
		if delErr := r.Coordinator.DeleteSessionCheckpoint(ctx, sb, ck.Ref, ck.Backend); delErr != nil {
			logger.Info("best-effort checkpoint object delete failed (already crypto-erased via KEK deletion)",
				"ref", ck.Ref, "error", delErr.Error())
		}
	}
}
