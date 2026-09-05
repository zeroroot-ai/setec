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

package status

import (
	"time"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
)

// ReasonIdleTimeout is recorded on Failed when a Running session
// Sandbox exceeded its SandboxClass's sessionIdleTimeout with no
// recorded caller activity (ADR-0006 idle eviction). It is the session
// counterpart of ReasonTimeout: Timeout bounds total runtime, IdleTimeout
// bounds unattended runtime.
const ReasonIdleTimeout = "IdleTimeout"

// LastSessionActivity returns the most recent caller activity known for
// the Sandbox. It prefers the frontend-stamped last-activity annotation,
// falls back to status.startedAt (the session became "active" when its
// VM first ran), and finally to the creation timestamp, so a session
// that has never seen an Attach still gets a full idle window before
// eviction rather than being evicted at birth. A malformed annotation is
// ignored rather than trusted.
func LastSessionActivity(sb *setecv1alpha1.Sandbox) time.Time {
	if sb == nil {
		return time.Time{}
	}
	last := sb.CreationTimestamp.Time
	if sb.Status.StartedAt != nil && sb.Status.StartedAt.Time.After(last) {
		last = sb.Status.StartedAt.Time
	}
	if raw, ok := sb.Annotations[setecv1alpha1.AnnotationLastActivity]; ok {
		if t, err := time.Parse(time.RFC3339, raw); err == nil && t.After(last) {
			last = t
		}
	}
	return last
}

// SessionIdleDeadline returns the instant at which the session Sandbox
// becomes evictable for idleness, and whether an idle policy applies at
// all. No policy applies when the Sandbox is not a session, when no
// class resolved, or when the class does not state a positive
// sessionIdleTimeout — in each of those cases sessions are never
// idle-evicted (fail-open by design: eviction is a cost policy, not a
// safety control).
func SessionIdleDeadline(
	sb *setecv1alpha1.Sandbox,
	cls *setecv1alpha1.SandboxClass,
) (time.Time, bool) {
	if sb == nil || !sb.Spec.IsSession() {
		return time.Time{}, false
	}
	if cls == nil || cls.Spec.SessionIdleTimeout == nil ||
		cls.Spec.SessionIdleTimeout.Duration <= 0 {
		return time.Time{}, false
	}
	return LastSessionActivity(sb).Add(cls.Spec.SessionIdleTimeout.Duration), true
}

// ApplySessionIdlePolicy layers ADR-0006 idle eviction on top of a
// Derive result. A Running session past its idle deadline transitions
// to Failed with reason IdleTimeout; every other status passes through
// untouched. Only Running is evaluated: a Pending session is not
// consuming a microVM, terminal phases are already over, and the
// coordinator-owned phases (Paused/Snapshotting/Restoring) belong to
// the suspend machinery (setec#194). Like Derive, the function is pure.
//
// When the class enables sessionCheckpoint, idleness is owned by the
// suspend machinery instead (setec#194): the same deadline suspends
// the session — checkpoint, release the microVM, resume on reattach —
// rather than hard-failing it, so this policy passes the status
// through untouched.
func ApplySessionIdlePolicy(
	sb *setecv1alpha1.Sandbox,
	cls *setecv1alpha1.SandboxClass,
	in setecv1alpha1.SandboxStatus,
	now time.Time,
) setecv1alpha1.SandboxStatus {
	if in.Phase != setecv1alpha1.SandboxPhaseRunning {
		return in
	}
	if cls != nil && cls.Spec.SessionCheckpoint != nil {
		return in
	}
	deadline, ok := SessionIdleDeadline(sb, cls)
	if !ok || now.Before(deadline) {
		return in
	}
	return setPhase(in, setecv1alpha1.SandboxPhaseFailed, ReasonIdleTimeout, now)
}
