// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

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

// Session-checkpoint machinery (setec#194, docs/design/lifecycles.md L2 / docs/design/storage.md).
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
	eventReasonSessionRecycled        = "SessionRecycled"
	eventReasonNodeLost               = "SessionNodeLost"
)

// sessionCheckpointPolicy returns the class's checkpoint spec when the
// Sandbox is a session and the class enables checkpoints; nil
// otherwise.
// annotationValueTrue is the canonical truthy annotation value; Kubernetes
// annotations are strings, so this is written literally in several places.
const annotationValueTrue = "true"

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
		Name:      sessionKEKName(sb),
		Namespace: sb.Namespace,
		Labels:    map[string]string{podspec.SandboxLabelKey: sb.Name},
		Type:      corev1.SecretTypeOpaque,
		Data:      map[string][]byte{sessionKEKKey: kek},
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
) (res ctrl.Result, handled bool, err error) {
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
	// A launcher resume Pod waits for the checkpoint and is Ready only
	// after the load, so for it the running Pod, not the Running phase,
	// starts the restore.
	// The Pod must be a new one: a checkpoint never loads into the Pod
	// that wrote it. A stale cache can still show that Pod as Running
	// after the suspend deleted it, while the Sandbox already shows the
	// pending checkpoint (setec#220).
	if ck := sb.Status.Checkpoint; ck != nil && ck.PendingRestore &&
		(desired.Phase == setecv1alpha1.SandboxPhaseRunning || isLauncherSandbox(sb)) &&
		pod.Status.Phase == corev1.PodRunning && pod.DeletionTimestamp.IsZero() &&
		(ck.PodUID == "" || string(pod.UID) != ck.PodUID) {
		return r.restorePendingCheckpoint(ctx, logger, sb, policy)
	}

	// (c0) Node loss with no notice (setec#194): the node of a durable
	// session stopped reporting. No kubelet will confirm the end of the
	// Pod, so after nodeLossGrace it is removed with no grace, and the
	// session resumes from its last checkpoint on another node.
	if policy.Durable && pod.Spec.NodeName != "" && pod.Annotations[annotationSuspendedPod] == "" {
		if lostFor, lost := r.nodeLostFor(ctx, pod.Spec.NodeName); lost {
			if lostFor < nodeLossGrace {
				return ctrl.Result{RequeueAfter: nodeLossGrace - lostFor}, true, nil
			}
			res, err := r.resumeAfterNodeLoss(ctx, logger, sb, pod)
			return res, true, err
		}
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
	// microVM, keep the session recoverable (docs/design/lifecycles.md L2). The suspend
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
		policy.EffectiveInterval() > 0 {
		due := true
		if ck := sb.Status.Checkpoint; ck != nil && ck.TakenAt != nil {
			due = time.Since(ck.TakenAt.Time) >= policy.EffectiveInterval()
		} else if sb.Status.StartedAt != nil {
			// First checkpoint one full interval after the VM started.
			due = time.Since(sb.Status.StartedAt.Time) >= policy.EffectiveInterval()
		}
		if due {
			if err := r.takeCheckpoint(ctx, logger, sb, policy, false); err != nil {
				// A failed periodic checkpoint must not kill a healthy
				// session: record and retry on the next interval tick.
				logger.Error(err, "periodic session checkpoint failed; VM keeps running")
			}
			return ctrl.Result{RequeueAfter: policy.EffectiveInterval()}, true, nil
		}
		// Not due yet — make sure a quiet session still gets its next
		// checkpoint on time.
		next := max(time.Until(nextCheckpointDue(sb, policy)), time.Second)
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
	return base.Add(policy.EffectiveInterval())
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
	leavePaused bool,
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

	// A durable launcher session takes a diff on its last checkpoint when
	// the same machine wrote it (setec#194). The chain is kept to
	// maxDiffChain diffs; then a full checkpoint starts a new chain.
	podUID := r.sessionPodUID(ctx, sb)
	parent, parents := "", []string(nil)
	if policy.Durable && isLauncherSandbox(sb) && prev != nil && prev.Ref != "" && podUID != "" &&
		prev.PodUID == podUID && len(prev.Parents) < maxDiffChain {
		parent = prev.Ref
		parents = append(append([]string(nil), prev.Parents...), prev.Ref)
	}

	ref, size, err := r.Coordinator.CheckpointSession(ctx, sb, backend, seq, kek, leavePaused, parent)
	if err != nil {
		return fmt.Errorf("checkpoint session: %w", err)
	}

	// New checkpoint is durable; destroy the superseded one (its DEK
	// first, via the backend's Delete). Best-effort: a failed cleanup
	// never invalidates the fresh checkpoint. A diff keeps its chain.
	if prev != nil && prev.Ref != "" && parent == "" {
		r.deleteCheckpointChain(ctx, logger, sb, prev)
	}

	now := metav1.Now()
	original := sb.DeepCopy()
	ck := &setecv1alpha1.SandboxCheckpointStatus{
		Ref:       ref,
		Backend:   backend,
		Sequence:  seq,
		TakenAt:   &now,
		SizeBytes: size,
		PodUID:    podUID,
		Parents:   parents,
	}
	ck.CopyRecovery(prev)
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
	// A launcher machine stays paused after the suspend checkpoint until
	// its Pod ends, so its workspace never runs ahead of the memory.
	ckErr := r.takeCheckpoint(ctx, logger, sb, policy, isLauncherSandbox(sb))
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
		pod.Annotations[annotationSuspendedPod] = annotationValueTrue
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
	// A drain whose fresh checkpoint failed still resumes a durable
	// session from its last one (setec#194); any other session restarts
	// from the workspace.
	sb.Status.Checkpoint.PendingRestore = sb.Status.Checkpoint.Ref != "" && (ckErr == nil || policy.Durable)
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
// it ends (docs/design/isolation.md forbids restoring the same state twice): on
// success the session continues (ResumedFromCheckpoint); on failure
// the already-running cold-booted VM carries on against the durable
// workspace, surfaced as the distinct RestartedFromWorkspace
// condition. Either way the stored checkpoint objects are destroyed.
func (r *SandboxReconciler) restorePendingCheckpoint(
	ctx context.Context,
	logger logr.Logger,
	sb *setecv1alpha1.Sandbox,
	policy *setecv1alpha1.SessionCheckpointSpec,
) (res ctrl.Result, handled bool, err error) {
	ck := sb.Status.Checkpoint
	recovery := setecv1alpha1.SessionRecoveryResumedFromCheckpoint

	// The restore runs once. A reconcile on a stale cache still sees the
	// pending checkpoint after the restore consumed it, and a second
	// restore fails on the deleted checkpoint and ends a healthy session
	// (setec#197). So the live object decides.
	if pending, err := r.restoreStillPending(ctx, sb, ck.Ref); err != nil || !pending {
		return ctrl.Result{}, true, err
	}

	kek, kekErr := r.readSessionKEK(ctx, sb)
	var restoreErr error
	if kekErr != nil {
		restoreErr = kekErr
	} else {
		var takenAt time.Time
		if ck.TakenAt != nil {
			takenAt = ck.TakenAt.Time
		}
		restoreErr = r.Coordinator.RestoreSessionCheckpoint(ctx, sb, ck.Ref, ck.Backend, kek, takenAt)
	}
	switch {
	case errors.Is(restoreErr, snapshot.ErrInvariantGateViolation):
		// docs/design/isolation.md invariant gate refusal: the VM already holds the
		// checkpoint state but its verifications did not pass, so the
		// VM is DESTROYED — never served. The session itself survives
		// per docs/design/lifecycles.md: the deleted Pod is recreated and the fresh VM
		// cold-boots against the durable workspace (the checkpoint is
		// consumed below either way).
		recovery = setecv1alpha1.SessionRecoveryRestartedFromWorkspace
		r.Recorder.Eventf(sb, nil, corev1.EventTypeWarning, eventReasonInvariantGateViolation, actionEnforceInvariantGate,
			"docs/design/isolation.md invariant gate refused checkpoint resume #%d (%v); destroying the VM that received the unverified state — session restarts from durable workspace",
			ck.Sequence, restoreErr)
		logger.Error(restoreErr, "invariant gate refused session checkpoint resume; destroying VM")
		if err := r.deleteSessionPod(ctx, sb); err != nil {
			return ctrl.Result{}, true, fmt.Errorf("after invariant-gate refusal: %w", err)
		}
	case restoreErr != nil:
		recovery = setecv1alpha1.SessionRecoveryRestartedFromWorkspace
		// A launcher resume Pod waits for a checkpoint that will not
		// come. It goes, and the next Pod boots against the workspace.
		if isLauncherSandbox(sb) {
			if err := r.deleteSessionPod(ctx, sb); err != nil {
				return ctrl.Result{}, true, err
			}
		}
		r.Recorder.Eventf(sb, nil, corev1.EventTypeWarning, eventReasonRestartedFromWorkspace, actionManageCheckpoint,
			"Checkpoint restore failed (%v); session restarted from durable workspace — no data lost, process state since checkpoint #%d gone",
			restoreErr, ck.Sequence)
		logger.Error(restoreErr, "session checkpoint restore failed; degraded to restart-from-workspace")
	default:
		r.Recorder.Eventf(sb, nil, corev1.EventTypeNormal, eventReasonResumedFromCheckpoint, actionManageCheckpoint,
			"Session resumed from checkpoint #%d; process state intact", ck.Sequence)
	}

	// The checkpoint is consumed either way: destroy its objects.
	r.deleteCheckpointChain(ctx, logger, sb, ck)

	original := sb.DeepCopy()
	next := &setecv1alpha1.SandboxCheckpointStatus{Backend: ck.Backend, Sequence: ck.Sequence}
	next.CopyRecovery(ck)
	next.RecordRecovery(recovery, metav1.Now(), ck.TakenAt)
	sb.Status.Checkpoint = next
	if err := r.Status().Patch(ctx, sb, client.MergeFrom(original)); err != nil {
		return ctrl.Result{}, true, fmt.Errorf("patch post-restore checkpoint status: %w", err)
	}
	// Requeue so the periodic-interval branch re-establishes a fresh
	// checkpoint on its own schedule.
	if policy.EffectiveInterval() > 0 {
		return ctrl.Result{RequeueAfter: policy.EffectiveInterval()}, true, nil
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
		// Objects only: the KEK deletion above already crypto-erased them.
		r.deleteCheckpointChain(ctx, logger, sb, ck)
	}
}

// deleteSessionPod deletes the VM Pod of a session, if it exists.
func (r *SandboxReconciler) deleteSessionPod(ctx context.Context, sb *setecv1alpha1.Sandbox) error {
	podName := sb.Status.PodName
	if podName == "" {
		podName = sb.Name + "-vm"
	}
	pod := &corev1.Pod{}
	if perr := r.Get(ctx, types.NamespacedName{Namespace: sb.Namespace, Name: podName}, pod); perr == nil && pod.DeletionTimestamp.IsZero() {
		if derr := r.Delete(ctx, pod); derr != nil && !apierrors.IsNotFound(derr) {
			return fmt.Errorf("delete the session Pod: %w", derr)
		}
	}
	return nil
}

// pendingCheckpoint reports whether a session resumes from a checkpoint:
// its next launcher Pod loads it instead of a boot.
func pendingCheckpoint(sb *setecv1alpha1.Sandbox) bool {
	return sb.Status.Checkpoint != nil && sb.Status.Checkpoint.PendingRestore && sb.Status.Checkpoint.Ref != ""
}

// recycleIfExpired deletes a session that stayed suspended longer than the
// SuspendedTTL of its class (setec#193). The teardown of the Sandbox then
// deletes its checkpoint, its key and its workspace. A session within its
// time waits, and the reconcile returns at its deadline.
func (r *SandboxReconciler) recycleIfExpired(
	ctx context.Context, sb *setecv1alpha1.Sandbox, cls *setecv1alpha1.SandboxClass,
) (ctrl.Result, error) {
	since := sb.CreationTimestamp.Time
	if sb.Status.LastTransitionTime != nil {
		since = sb.Status.LastTransitionTime.Time
	}
	deadline := since.Add(sessionCheckpointPolicy(sb, cls).RecycleAfter())
	if wait := time.Until(deadline); wait > 0 {
		return ctrl.Result{RequeueAfter: wait}, nil
	}
	r.Recorder.Eventf(sb, nil, corev1.EventTypeNormal, eventReasonSessionRecycled, actionManageCheckpoint,
		"Session suspended since %s; recycled with its checkpoint, key and workspace", since.UTC().Format(time.RFC3339))
	if err := r.Delete(ctx, sb); err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, fmt.Errorf("recycle the suspended session: %w", err)
	}
	return ctrl.Result{}, nil
}

// maxDiffChain bounds the diffs on one full checkpoint.
const maxDiffChain = 8

// deleteCheckpointChain deletes a checkpoint and the chain below it.
// Best-effort: a failed delete is logged.
func (r *SandboxReconciler) deleteCheckpointChain(
	ctx context.Context, logger logr.Logger, sb *setecv1alpha1.Sandbox, ck *setecv1alpha1.SandboxCheckpointStatus,
) {
	for _, ref := range append(append([]string(nil), ck.Parents...), ck.Ref) {
		if ref == "" {
			continue
		}
		if err := r.Coordinator.DeleteSessionCheckpoint(ctx, sb, ref, ck.Backend); err != nil {
			logger.Error(err, "failed to delete a session checkpoint", "ref", ref)
		}
	}
}

// sessionPodUID returns the UID of the VM Pod of a session, or "".
func (r *SandboxReconciler) sessionPodUID(ctx context.Context, sb *setecv1alpha1.Sandbox) string {
	name := sb.Status.PodName
	if name == "" {
		name = sb.Name + "-vm"
	}
	pod := &corev1.Pod{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: sb.Namespace, Name: name}, pod); err != nil {
		return ""
	}
	return string(pod.UID)
}

// nodeLossGrace is how long the node of a durable session may stay not
// Ready before the session moves (setec#194). It is shorter than the
// default eviction of Kubernetes (5 minutes), which waits for a kubelet
// that is gone.
const nodeLossGrace = 90 * time.Second

// nodeLostFor reports whether a node is gone or not Ready, and for how
// long.
func (r *SandboxReconciler) nodeLostFor(ctx context.Context, name string) (time.Duration, bool) {
	node := &corev1.Node{}
	if err := r.Get(ctx, types.NamespacedName{Name: name}, node); err != nil {
		if apierrors.IsNotFound(err) {
			return nodeLossGrace, true
		}
		return 0, false
	}
	for _, c := range node.Status.Conditions {
		if c.Type == corev1.NodeReady {
			if c.Status == corev1.ConditionTrue {
				return 0, false
			}
			return time.Since(c.LastTransitionTime.Time), true
		}
	}
	return 0, false
}

// resumeAfterNodeLoss removes the Pod of a session whose node is lost and
// marks its last checkpoint for the restore. The next reconcile makes a
// Pod on another node that loads it.
func (r *SandboxReconciler) resumeAfterNodeLoss(
	ctx context.Context, logger logr.Logger, sb *setecv1alpha1.Sandbox, pod *corev1.Pod,
) (ctrl.Result, error) {
	r.Recorder.Eventf(sb, nil, corev1.EventTypeWarning, eventReasonNodeLost, actionManageCheckpoint,
		"Node %q stopped reporting; the session resumes from its last checkpoint on another node", pod.Spec.NodeName)
	logger.Info("session node lost; moving the session", "node", pod.Spec.NodeName)
	if ck := sb.Status.Checkpoint; ck != nil && ck.Ref != "" && !ck.PendingRestore {
		original := sb.DeepCopy()
		sb.Status.Checkpoint.PendingRestore = true
		if err := r.Status().Patch(ctx, sb, client.MergeFrom(original)); err != nil {
			return ctrl.Result{}, fmt.Errorf("mark the last checkpoint for the restore: %w", err)
		}
	}
	if err := r.Delete(ctx, pod, client.GracePeriodSeconds(0)); err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, fmt.Errorf("remove the Pod of the lost node: %w", err)
	}
	return ctrl.Result{RequeueAfter: suspendWaitRequeue}, nil
}

// restoreStillPending reports whether the live Sandbox still has the
// pending checkpoint ref.
func (r *SandboxReconciler) restoreStillPending(ctx context.Context, sb *setecv1alpha1.Sandbox, ref string) (bool, error) {
	var reader client.Reader = r.Client
	if r.APIReader != nil {
		reader = r.APIReader
	}
	live := &setecv1alpha1.Sandbox{}
	if err := reader.Get(ctx, client.ObjectKeyFromObject(sb), live); err != nil {
		return false, fmt.Errorf("read the live sandbox before the restore: %w", err)
	}
	lc := live.Status.Checkpoint
	return lc != nil && lc.PendingRestore && lc.Ref == ref, nil
}
