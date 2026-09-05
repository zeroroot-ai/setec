// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package status

import (
	"time"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
)

// ReasonPauseTimeout is recorded on Failed when a Paused Sandbox
// exceeded its SandboxClass's maxPauseDuration (setec#202). A paused
// microVM keeps its full memory reservation, so the cap bounds how
// long that reservation may sit idle: Timeout bounds total runtime,
// IdleTimeout bounds unattended runtime, PauseTimeoutExceeded bounds
// paused residency.
const ReasonPauseTimeout = "PauseTimeoutExceeded"

// PauseDeadline returns the instant at which a Paused Sandbox exceeds
// its class's maxPauseDuration, and whether such a bound applies at
// all. No bound applies when the status carries no pausedAt stamp,
// when no class resolved, or when the class does not state a positive
// maxPauseDuration — in each of those cases pauses are unbounded
// (fail-open by design: the cap is a cost policy, not a safety
// control). The deadline is a pure function of the status and class,
// independent of the current phase; callers decide which phases it
// governs.
func PauseDeadline(
	st setecv1alpha1.SandboxStatus,
	cls *setecv1alpha1.SandboxClass,
) (time.Time, bool) {
	if st.PausedAt == nil {
		return time.Time{}, false
	}
	if cls == nil || cls.Spec.MaxPauseDuration == nil ||
		cls.Spec.MaxPauseDuration.Duration <= 0 {
		return time.Time{}, false
	}
	return st.PausedAt.Add(cls.Spec.MaxPauseDuration.Duration), true
}

// ApplyPausePolicy layers maxPauseDuration enforcement (setec#202) on
// top of a Derive result. A Paused Sandbox past its pause deadline
// transitions to Failed with reason PauseTimeoutExceeded — exactly
// what the field has always documented; every other status passes
// through untouched. Only Paused is evaluated: no other phase holds a
// paused microVM's memory reservation. In particular Suspended is NOT
// bounded — a suspended session has already released its microVM
// (checkpoint retained), so the cost the cap exists to bound is gone.
// Like Derive, the function is pure.
//
// When the class enables sessionCheckpoint, a Paused session past the
// deadline is owned by the suspend machinery instead (setec#194,
// ADR-0006): the session suspends — checkpoint, release the microVM,
// resume when desiredState returns to Running — rather than
// hard-failing, so this policy passes the status through untouched.
// An ephemeral Sandbox in such a class still hard-fails here: the
// suspend machinery only serves sessions.
func ApplyPausePolicy(
	sb *setecv1alpha1.Sandbox,
	cls *setecv1alpha1.SandboxClass,
	in setecv1alpha1.SandboxStatus,
	now time.Time,
) setecv1alpha1.SandboxStatus {
	if in.Phase != setecv1alpha1.SandboxPhasePaused {
		return in
	}
	if sb != nil && sb.Spec.IsSession() && cls != nil && cls.Spec.SessionCheckpoint != nil {
		return in
	}
	deadline, ok := PauseDeadline(in, cls)
	if !ok || now.Before(deadline) {
		return in
	}
	return setPhase(in, setecv1alpha1.SandboxPhaseFailed, ReasonPauseTimeout, now)
}
