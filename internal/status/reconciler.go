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

// Package status contains the pure deriver that maps observed Pod state onto
// the high-level Sandbox lifecycle phases. The deriver is deliberately free of
// side effects and of any Kubernetes-client dependency so every mapping rule
// can be exercised by table-driven unit tests using only in-memory objects and
// a caller-supplied clock value.
package status

import (
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
)

const (
	// ImagePullGracePeriod is the wall-clock grace window Setec allows a
	// container to remain in ImagePullBackOff or ErrImagePull before the
	// Sandbox is marked Failed with reason ImagePullFailure. The window is
	// long enough to tolerate transient registry hiccups without masking a
	// genuinely bad image reference.
	ImagePullGracePeriod = 5 * time.Minute

	// ReasonTimeout is recorded on Failed when the workload exceeded
	// Sandbox.spec.lifecycle.timeout.
	ReasonTimeout = "Timeout"

	// ReasonImagePullFailure is recorded on Failed when a container has
	// been stuck pulling its image for longer than ImagePullGracePeriod.
	ReasonImagePullFailure = "ImagePullFailure"

	// ReasonContainerExitedNonZero is recorded on Failed when the Pod
	// terminated with a non-zero container exit code and the kubelet did
	// not populate a more specific reason string.
	ReasonContainerExitedNonZero = "ContainerExitedNonZero"

	// waitReasonImagePullBackOff and waitReasonErrImagePull are the two
	// well-known ContainerStateWaiting reasons the kubelet uses while an
	// image pull is failing. They are strings, not exported constants in
	// k8s.io/api, which is why we centralize them here.
	waitReasonImagePullBackOff = "ImagePullBackOff"
	waitReasonErrImagePull     = "ErrImagePull"
)

// Derive returns the SandboxStatus the controller should persist given the
// current Sandbox spec, the observed Pod (may be nil if the Pod has not yet
// been created), and the current wall-clock time. The function is pure: it
// neither mutates its inputs nor performs any I/O.
//
// Terminal phases are sticky: once a Sandbox has reached Completed or Failed
// the returned status will preserve that phase, reason, and exitCode even if
// the Pod state looks like it has moved backwards (for example, because the
// Pod was deleted and a stale cache briefly surfaces it as missing or
// Pending). This matches the contract documented on SandboxStatus.Phase that
// the controller "will not roll it back to Pending or Running".
//
// LastTransitionTime is updated to `now` only when the phase actually changes
// relative to sb.Status.Phase. Other fields (reason, exitCode, podName,
// startedAt) are propagated forward from the previous status when the new
// observation does not contradict them, so repeated calls with the same
// inputs are idempotent once the phase has stabilized.
func Derive(
	sb *setecv1alpha1.Sandbox,
	pod *corev1.Pod,
	now time.Time,
) setecv1alpha1.SandboxStatus {
	// Start from the previous status. We copy by value so callers cannot
	// observe our intermediate mutations.
	out := sb.Status

	// Terminal phases are sticky. Return immediately so we cannot be pushed
	// backwards by a late Pod observation.
	if isTerminal(out.Phase) {
		return out
	}

	// When no Pod has been observed yet, remain Pending.
	if pod == nil {
		return setPhase(out, setecv1alpha1.SandboxPhasePending, "", now)
	}

	// Always surface the Pod name we are observing. This is cheap and
	// helps kubectl describe output line up with reality even before any
	// phase transition occurs.
	if pod.Name != "" {
		out.PodName = pod.Name
	}

	// Phase 3 transient states. Paused, Snapshotting, and Restoring are
	// driven by the snapshot.Coordinator — not by the Pod phase — so
	// the Derive function must leave them intact while the Pod is
	// still Running. A terminal Pod state (Succeeded/Failed) still
	// overrides because the coordinator cannot complete an operation
	// against a dead VM.
	if isCoordinatorPhase(out.Phase) && pod.Status.Phase == corev1.PodRunning {
		return out
	}

	switch pod.Status.Phase {
	case corev1.PodSucceeded:
		out = setPhase(out, setecv1alpha1.SandboxPhaseCompleted, "", now)
		zero := int32(0)
		out.ExitCode = &zero
		// Preserve startedAt if we ever observed it; otherwise fall back
		// to whatever the Pod reports.
		if out.StartedAt == nil && pod.Status.StartTime != nil {
			t := *pod.Status.StartTime
			out.StartedAt = &t
		}
		return out

	case corev1.PodFailed:
		exit, reason := terminatedExitAndReason(pod)
		if reason == "" {
			reason = ReasonContainerExitedNonZero
		}
		out = setPhase(out, setecv1alpha1.SandboxPhaseFailed, reason, now)
		e := exit
		out.ExitCode = &e
		if out.StartedAt == nil && pod.Status.StartTime != nil {
			t := *pod.Status.StartTime
			out.StartedAt = &t
		}
		return out

	case corev1.PodRunning:
		// Populate startedAt the first time we see the Pod Running.
		if out.StartedAt == nil {
			if pod.Status.StartTime != nil {
				t := *pod.Status.StartTime
				out.StartedAt = &t
			} else {
				t := metav1.NewTime(now)
				out.StartedAt = &t
			}
		}
		// Evaluate lifecycle timeout.
		if timedOut(sb, out.StartedAt, now) {
			out = setPhase(out, setecv1alpha1.SandboxPhaseFailed, ReasonTimeout, now)
			return out
		}
		out = setPhase(out, setecv1alpha1.SandboxPhaseRunning, "", now)
		return out

	default:
		// PodPending, PodUnknown, or an empty Phase. Check for a stuck
		// image pull first; otherwise remain Pending.
		if imagePullStuck(pod, now) {
			out = setPhase(out, setecv1alpha1.SandboxPhaseFailed, ReasonImagePullFailure, now)
			return out
		}
		out = setPhase(out, setecv1alpha1.SandboxPhasePending, "", now)
		return out
	}
}

// isTerminal returns true for phases the controller must not roll back from.
func isTerminal(p setecv1alpha1.SandboxPhase) bool {
	return p == setecv1alpha1.SandboxPhaseCompleted || p == setecv1alpha1.SandboxPhaseFailed
}

// isCoordinatorPhase returns true for the Phase 3 phases owned by
// the snapshot.Coordinator (Paused, Snapshotting, Restoring). Derive
// treats them as passthrough when the Pod is still Running so the
// Coordinator can transition them on its own schedule without racing
// against the Sandbox reconciler.
func isCoordinatorPhase(p setecv1alpha1.SandboxPhase) bool {
	switch p {
	case setecv1alpha1.SandboxPhasePaused,
		setecv1alpha1.SandboxPhaseSnapshotting,
		setecv1alpha1.SandboxPhaseRestoring:
		return true
	}
	return false
}

// setPhase centralizes the "bump LastTransitionTime only when the phase
// changes" rule. It returns the updated status by value.
func setPhase(
	in setecv1alpha1.SandboxStatus,
	phase setecv1alpha1.SandboxPhase,
	reason string,
	now time.Time,
) setecv1alpha1.SandboxStatus {
	out := in
	if out.Phase != phase {
		out.Phase = phase
		t := metav1.NewTime(now)
		out.LastTransitionTime = &t
	}
	out.Reason = reason
	return out
}

// terminatedExitAndReason walks the Pod's container statuses looking for a
// terminated state. It returns the first terminated exit code and reason it
// finds, or (0, "") if no terminated state is available.
func terminatedExitAndReason(pod *corev1.Pod) (int32, string) {
	if pod == nil {
		return 0, ""
	}
	// Prefer regular containers over init containers when both report
	// terminated state; the workload container is the authoritative signal
	// for Sandbox exit semantics.
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.State.Terminated != nil {
			return cs.State.Terminated.ExitCode, cs.State.Terminated.Reason
		}
	}
	for _, cs := range pod.Status.InitContainerStatuses {
		if cs.State.Terminated != nil {
			return cs.State.Terminated.ExitCode, cs.State.Terminated.Reason
		}
	}
	return 0, ""
}

// imagePullStuck reports whether any container has been waiting on an image
// pull for longer than ImagePullGracePeriod. The "since" timestamp is taken
// from the waiting container's last termination timestamp if available, or
// from the Pod's creation timestamp as a conservative fallback.
func imagePullStuck(pod *corev1.Pod, now time.Time) bool {
	if pod == nil {
		return false
	}
	check := func(statuses []corev1.ContainerStatus) bool {
		for _, cs := range statuses {
			w := cs.State.Waiting
			if w == nil {
				continue
			}
			if w.Reason != waitReasonImagePullBackOff && w.Reason != waitReasonErrImagePull {
				continue
			}
			// Establish the earliest timestamp we know the wait began.
			var since time.Time
			switch {
			case cs.LastTerminationState.Terminated != nil &&
				!cs.LastTerminationState.Terminated.FinishedAt.IsZero():
				since = cs.LastTerminationState.Terminated.FinishedAt.Time
			case !pod.CreationTimestamp.IsZero():
				since = pod.CreationTimestamp.Time
			default:
				since = now
			}
			if now.Sub(since) >= ImagePullGracePeriod {
				return true
			}
		}
		return false
	}
	if check(pod.Status.ContainerStatuses) {
		return true
	}
	return check(pod.Status.InitContainerStatuses)
}

// timedOut reports whether a Running Sandbox has exceeded its configured
// lifecycle timeout. A nil timeout (either Lifecycle omitted or Timeout unset
// or zero) means "no timeout".
func timedOut(sb *setecv1alpha1.Sandbox, startedAt *metav1.Time, now time.Time) bool {
	if sb == nil || sb.Spec.Lifecycle == nil || sb.Spec.Lifecycle.Timeout == nil {
		return false
	}
	if startedAt == nil {
		return false
	}
	d := sb.Spec.Lifecycle.Timeout.Duration
	if d <= 0 {
		return false
	}
	return now.Sub(startedAt.Time) > d
}
