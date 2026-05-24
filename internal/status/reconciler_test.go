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
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	setecv1alpha1 "github.com/zero-day-ai/setec/api/v1alpha1"
)

// fixed reference time used by every test case so diffs stay stable.
var (
	t0   = time.Date(2026, 4, 14, 12, 0, 0, 0, time.UTC)
	tMin = func(m int) time.Time { return t0.Add(time.Duration(m) * time.Minute) }
)

// ptrInt32 takes the address of an int32 value.
func ptrInt32(v int32) *int32 {
	return &v
}

// newSandbox builds a Sandbox with reasonable defaults. Mutators customize.
func newSandbox(mutators ...func(*setecv1alpha1.Sandbox)) *setecv1alpha1.Sandbox {
	sb := &setecv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "demo",
			Namespace: "default",
		},
		Spec: setecv1alpha1.SandboxSpec{
			Image:   "docker.io/library/python:3.12-slim",
			Command: []string{"python", "-c", "print('hi')"},
			Resources: setecv1alpha1.Resources{
				VCPU:   2,
				Memory: resource.MustParse("2Gi"),
			},
		},
	}
	for _, m := range mutators {
		m(sb)
	}
	return sb
}

// newPod builds a Pod skeleton. Mutators customize the status.
func newPod(mutators ...func(*corev1.Pod)) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "demo-vm",
			Namespace:         "default",
			CreationTimestamp: metav1.NewTime(t0),
		},
	}
	for _, m := range mutators {
		m(p)
	}
	return p
}

// withTimeout sets spec.lifecycle.timeout on the Sandbox.
func withTimeout(d time.Duration) func(*setecv1alpha1.Sandbox) {
	return func(sb *setecv1alpha1.Sandbox) {
		sb.Spec.Lifecycle = &setecv1alpha1.Lifecycle{
			Timeout: &metav1.Duration{Duration: d},
		}
	}
}

// withStatus sets the prior status on the Sandbox.
func withStatus(s setecv1alpha1.SandboxStatus) func(*setecv1alpha1.Sandbox) {
	return func(sb *setecv1alpha1.Sandbox) { sb.Status = s }
}

// TestDerive exercises every documented mapping rule in the deriver.
func TestDerive(t *testing.T) {
	t.Parallel()

	type tc struct {
		name    string
		sandbox *setecv1alpha1.Sandbox
		pod     *corev1.Pod
		now     time.Time
		want    setecv1alpha1.SandboxStatus
	}

	tests := []tc{
		{
			name:    "nil Pod while Sandbox is fresh returns Pending with now as transition time",
			sandbox: newSandbox(),
			pod:     nil,
			now:     t0,
			want: setecv1alpha1.SandboxStatus{
				Phase:              setecv1alpha1.SandboxPhasePending,
				LastTransitionTime: ptrTime(t0),
			},
		},
		{
			name: "Pending Pod stays Pending and does not refresh LastTransitionTime when phase unchanged",
			sandbox: newSandbox(withStatus(setecv1alpha1.SandboxStatus{
				Phase:              setecv1alpha1.SandboxPhasePending,
				PodName:            "demo-vm",
				LastTransitionTime: ptrTime(t0),
			})),
			pod: newPod(func(p *corev1.Pod) {
				p.Status.Phase = corev1.PodPending
			}),
			now: tMin(1),
			want: setecv1alpha1.SandboxStatus{
				Phase:              setecv1alpha1.SandboxPhasePending,
				PodName:            "demo-vm",
				LastTransitionTime: ptrTime(t0),
			},
		},
		{
			name:    "Pending -> Running populates startedAt from Pod.Status.StartTime",
			sandbox: newSandbox(),
			pod: newPod(func(p *corev1.Pod) {
				p.Status.Phase = corev1.PodRunning
				p.Status.StartTime = ptrTime(tMin(1))
			}),
			now: tMin(2),
			want: setecv1alpha1.SandboxStatus{
				Phase:              setecv1alpha1.SandboxPhaseRunning,
				PodName:            "demo-vm",
				StartedAt:          ptrTime(tMin(1)),
				LastTransitionTime: ptrTime(tMin(2)),
			},
		},
		{
			name:    "Running without Pod.StartTime falls back to now for startedAt",
			sandbox: newSandbox(),
			pod: newPod(func(p *corev1.Pod) {
				p.Status.Phase = corev1.PodRunning
			}),
			now: tMin(3),
			want: setecv1alpha1.SandboxStatus{
				Phase:              setecv1alpha1.SandboxPhaseRunning,
				PodName:            "demo-vm",
				StartedAt:          ptrTime(tMin(3)),
				LastTransitionTime: ptrTime(tMin(3)),
			},
		},
		{
			name: "already Running stays Running and preserves original startedAt",
			sandbox: newSandbox(withStatus(setecv1alpha1.SandboxStatus{
				Phase:              setecv1alpha1.SandboxPhaseRunning,
				PodName:            "demo-vm",
				StartedAt:          ptrTime(tMin(1)),
				LastTransitionTime: ptrTime(tMin(1)),
			})),
			pod: newPod(func(p *corev1.Pod) {
				p.Status.Phase = corev1.PodRunning
				p.Status.StartTime = ptrTime(tMin(1))
			}),
			now: tMin(4),
			want: setecv1alpha1.SandboxStatus{
				Phase:              setecv1alpha1.SandboxPhaseRunning,
				PodName:            "demo-vm",
				StartedAt:          ptrTime(tMin(1)),
				LastTransitionTime: ptrTime(tMin(1)),
			},
		},
		{
			name:    "Succeeded Pod -> Completed with exitCode 0",
			sandbox: newSandbox(),
			pod: newPod(func(p *corev1.Pod) {
				p.Status.Phase = corev1.PodSucceeded
				p.Status.StartTime = ptrTime(tMin(1))
				p.Status.ContainerStatuses = []corev1.ContainerStatus{{
					Name: "workload",
					State: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{ExitCode: 0},
					},
				}}
			}),
			now: tMin(5),
			want: setecv1alpha1.SandboxStatus{
				Phase:              setecv1alpha1.SandboxPhaseCompleted,
				PodName:            "demo-vm",
				ExitCode:           ptrInt32(0),
				StartedAt:          ptrTime(tMin(1)),
				LastTransitionTime: ptrTime(tMin(5)),
			},
		},
		{
			name:    "Failed Pod with non-zero exit surfaces exit code and kubelet reason",
			sandbox: newSandbox(),
			pod: newPod(func(p *corev1.Pod) {
				p.Status.Phase = corev1.PodFailed
				p.Status.StartTime = ptrTime(tMin(1))
				p.Status.ContainerStatuses = []corev1.ContainerStatus{{
					Name: "workload",
					State: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{
							ExitCode: 137,
							Reason:   "OOMKilled",
						},
					},
				}}
			}),
			now: tMin(6),
			want: setecv1alpha1.SandboxStatus{
				Phase:              setecv1alpha1.SandboxPhaseFailed,
				Reason:             "OOMKilled",
				PodName:            "demo-vm",
				ExitCode:           ptrInt32(137),
				StartedAt:          ptrTime(tMin(1)),
				LastTransitionTime: ptrTime(tMin(6)),
			},
		},
		{
			name:    "Failed Pod with empty kubelet reason falls back to ContainerExitedNonZero",
			sandbox: newSandbox(),
			pod: newPod(func(p *corev1.Pod) {
				p.Status.Phase = corev1.PodFailed
				p.Status.ContainerStatuses = []corev1.ContainerStatus{{
					Name: "workload",
					State: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{ExitCode: 2},
					},
				}}
			}),
			now: tMin(2),
			want: setecv1alpha1.SandboxStatus{
				Phase:              setecv1alpha1.SandboxPhaseFailed,
				Reason:             ReasonContainerExitedNonZero,
				PodName:            "demo-vm",
				ExitCode:           ptrInt32(2),
				LastTransitionTime: ptrTime(tMin(2)),
			},
		},
		{
			name:    "Failed Pod with terminated init container is used when workload is absent",
			sandbox: newSandbox(),
			pod: newPod(func(p *corev1.Pod) {
				p.Status.Phase = corev1.PodFailed
				p.Status.InitContainerStatuses = []corev1.ContainerStatus{{
					Name: "init",
					State: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{
							ExitCode: 1,
							Reason:   "InitError",
						},
					},
				}}
			}),
			now: tMin(2),
			want: setecv1alpha1.SandboxStatus{
				Phase:              setecv1alpha1.SandboxPhaseFailed,
				Reason:             "InitError",
				PodName:            "demo-vm",
				ExitCode:           ptrInt32(1),
				LastTransitionTime: ptrTime(tMin(2)),
			},
		},
		{
			name:    "ImagePullBackOff before grace period keeps Sandbox Pending",
			sandbox: newSandbox(),
			pod: newPod(func(p *corev1.Pod) {
				p.Status.Phase = corev1.PodPending
				p.Status.ContainerStatuses = []corev1.ContainerStatus{{
					Name: "workload",
					State: corev1.ContainerState{
						Waiting: &corev1.ContainerStateWaiting{
							Reason:  waitReasonImagePullBackOff,
							Message: "Back-off pulling image",
						},
					},
				}}
			}),
			now: tMin(2),
			want: setecv1alpha1.SandboxStatus{
				Phase:              setecv1alpha1.SandboxPhasePending,
				PodName:            "demo-vm",
				LastTransitionTime: ptrTime(tMin(2)),
			},
		},
		{
			name:    "ImagePullBackOff beyond grace period transitions to Failed/ImagePullFailure",
			sandbox: newSandbox(),
			pod: newPod(func(p *corev1.Pod) {
				p.Status.Phase = corev1.PodPending
				p.Status.ContainerStatuses = []corev1.ContainerStatus{{
					Name: "workload",
					State: corev1.ContainerState{
						Waiting: &corev1.ContainerStateWaiting{
							Reason:  waitReasonImagePullBackOff,
							Message: "Back-off pulling image",
						},
					},
				}}
			}),
			now: tMin(6),
			want: setecv1alpha1.SandboxStatus{
				Phase:              setecv1alpha1.SandboxPhaseFailed,
				Reason:             ReasonImagePullFailure,
				PodName:            "demo-vm",
				LastTransitionTime: ptrTime(tMin(6)),
			},
		},
		{
			name:    "ErrImagePull beyond grace period also transitions to Failed/ImagePullFailure",
			sandbox: newSandbox(),
			pod: newPod(func(p *corev1.Pod) {
				p.Status.Phase = corev1.PodPending
				p.Status.ContainerStatuses = []corev1.ContainerStatus{{
					Name: "workload",
					State: corev1.ContainerState{
						Waiting: &corev1.ContainerStateWaiting{
							Reason: waitReasonErrImagePull,
						},
					},
				}}
			}),
			now: tMin(10),
			want: setecv1alpha1.SandboxStatus{
				Phase:              setecv1alpha1.SandboxPhaseFailed,
				Reason:             ReasonImagePullFailure,
				PodName:            "demo-vm",
				LastTransitionTime: ptrTime(tMin(10)),
			},
		},
		{
			name:    "ImagePullBackOff uses lastTerminationState.FinishedAt when available",
			sandbox: newSandbox(),
			pod: newPod(func(p *corev1.Pod) {
				// Pod created only a moment ago, but the waiting
				// container has been retrying long enough via the
				// last-termination timestamp to exceed the grace
				// period.
				p.CreationTimestamp = metav1.NewTime(tMin(10))
				p.Status.Phase = corev1.PodPending
				p.Status.ContainerStatuses = []corev1.ContainerStatus{{
					Name: "workload",
					State: corev1.ContainerState{
						Waiting: &corev1.ContainerStateWaiting{
							Reason: waitReasonImagePullBackOff,
						},
					},
					LastTerminationState: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{
							FinishedAt: metav1.NewTime(t0),
						},
					},
				}}
			}),
			now: tMin(11),
			want: setecv1alpha1.SandboxStatus{
				Phase:              setecv1alpha1.SandboxPhaseFailed,
				Reason:             ReasonImagePullFailure,
				PodName:            "demo-vm",
				LastTransitionTime: ptrTime(tMin(11)),
			},
		},
		{
			name:    "ImagePullBackOff with no creation timestamp or last termination falls back to now",
			sandbox: newSandbox(),
			pod: newPod(func(p *corev1.Pod) {
				p.CreationTimestamp = metav1.Time{}
				p.Status.Phase = corev1.PodPending
				p.Status.ContainerStatuses = []corev1.ContainerStatus{{
					Name: "workload",
					State: corev1.ContainerState{
						Waiting: &corev1.ContainerStateWaiting{
							Reason: waitReasonImagePullBackOff,
						},
					},
				}}
			}),
			now: tMin(30),
			want: setecv1alpha1.SandboxStatus{
				Phase:              setecv1alpha1.SandboxPhasePending,
				PodName:            "demo-vm",
				LastTransitionTime: ptrTime(tMin(30)),
			},
		},
		{
			name:    "init container stuck on ImagePullBackOff also triggers ImagePullFailure",
			sandbox: newSandbox(),
			pod: newPod(func(p *corev1.Pod) {
				p.Status.Phase = corev1.PodPending
				p.Status.InitContainerStatuses = []corev1.ContainerStatus{{
					Name: "init",
					State: corev1.ContainerState{
						Waiting: &corev1.ContainerStateWaiting{
							Reason: waitReasonImagePullBackOff,
						},
					},
				}}
			}),
			now: tMin(6),
			want: setecv1alpha1.SandboxStatus{
				Phase:              setecv1alpha1.SandboxPhaseFailed,
				Reason:             ReasonImagePullFailure,
				PodName:            "demo-vm",
				LastTransitionTime: ptrTime(tMin(6)),
			},
		},
		{
			name:    "waiting with unrelated reason does not trigger ImagePullFailure",
			sandbox: newSandbox(),
			pod: newPod(func(p *corev1.Pod) {
				p.Status.Phase = corev1.PodPending
				p.Status.ContainerStatuses = []corev1.ContainerStatus{{
					Name: "workload",
					State: corev1.ContainerState{
						Waiting: &corev1.ContainerStateWaiting{
							Reason: "CreateContainerConfigError",
						},
					},
				}}
			}),
			now: tMin(60),
			want: setecv1alpha1.SandboxStatus{
				Phase:              setecv1alpha1.SandboxPhasePending,
				PodName:            "demo-vm",
				LastTransitionTime: ptrTime(tMin(60)),
			},
		},
		{
			name: "Running beyond lifecycle.timeout transitions to Failed/Timeout",
			sandbox: newSandbox(
				withTimeout(10*time.Minute),
				withStatus(setecv1alpha1.SandboxStatus{
					Phase:              setecv1alpha1.SandboxPhaseRunning,
					PodName:            "demo-vm",
					StartedAt:          ptrTime(t0),
					LastTransitionTime: ptrTime(t0),
				}),
			),
			pod: newPod(func(p *corev1.Pod) {
				p.Status.Phase = corev1.PodRunning
				p.Status.StartTime = ptrTime(t0)
			}),
			now: tMin(11),
			want: setecv1alpha1.SandboxStatus{
				Phase:              setecv1alpha1.SandboxPhaseFailed,
				Reason:             ReasonTimeout,
				PodName:            "demo-vm",
				StartedAt:          ptrTime(t0),
				LastTransitionTime: ptrTime(tMin(11)),
			},
		},
		{
			name: "Running just before lifecycle.timeout remains Running",
			sandbox: newSandbox(
				withTimeout(10*time.Minute),
				withStatus(setecv1alpha1.SandboxStatus{
					Phase:              setecv1alpha1.SandboxPhaseRunning,
					PodName:            "demo-vm",
					StartedAt:          ptrTime(t0),
					LastTransitionTime: ptrTime(t0),
				}),
			),
			pod: newPod(func(p *corev1.Pod) {
				p.Status.Phase = corev1.PodRunning
				p.Status.StartTime = ptrTime(t0)
			}),
			now: t0.Add(10 * time.Minute),
			want: setecv1alpha1.SandboxStatus{
				Phase:              setecv1alpha1.SandboxPhaseRunning,
				PodName:            "demo-vm",
				StartedAt:          ptrTime(t0),
				LastTransitionTime: ptrTime(t0),
			},
		},
		{
			name: "timeout of zero duration is treated as no timeout",
			sandbox: newSandbox(
				withTimeout(0),
				withStatus(setecv1alpha1.SandboxStatus{
					Phase:              setecv1alpha1.SandboxPhaseRunning,
					PodName:            "demo-vm",
					StartedAt:          ptrTime(t0),
					LastTransitionTime: ptrTime(t0),
				}),
			),
			pod: newPod(func(p *corev1.Pod) {
				p.Status.Phase = corev1.PodRunning
				p.Status.StartTime = ptrTime(t0)
			}),
			now: tMin(60),
			want: setecv1alpha1.SandboxStatus{
				Phase:              setecv1alpha1.SandboxPhaseRunning,
				PodName:            "demo-vm",
				StartedAt:          ptrTime(t0),
				LastTransitionTime: ptrTime(t0),
			},
		},
		{
			name: "terminal Completed is sticky even when Pod reports Running",
			sandbox: newSandbox(withStatus(setecv1alpha1.SandboxStatus{
				Phase:              setecv1alpha1.SandboxPhaseCompleted,
				PodName:            "demo-vm",
				ExitCode:           ptrInt32(0),
				StartedAt:          ptrTime(t0),
				LastTransitionTime: ptrTime(tMin(5)),
			})),
			pod: newPod(func(p *corev1.Pod) {
				p.Status.Phase = corev1.PodRunning
				p.Status.StartTime = ptrTime(t0)
			}),
			now: tMin(99),
			want: setecv1alpha1.SandboxStatus{
				Phase:              setecv1alpha1.SandboxPhaseCompleted,
				PodName:            "demo-vm",
				ExitCode:           ptrInt32(0),
				StartedAt:          ptrTime(t0),
				LastTransitionTime: ptrTime(tMin(5)),
			},
		},
		{
			name: "terminal Failed is sticky even when Pod reports Pending",
			sandbox: newSandbox(withStatus(setecv1alpha1.SandboxStatus{
				Phase:              setecv1alpha1.SandboxPhaseFailed,
				Reason:             ReasonTimeout,
				PodName:            "demo-vm",
				ExitCode:           ptrInt32(1),
				StartedAt:          ptrTime(t0),
				LastTransitionTime: ptrTime(tMin(5)),
			})),
			pod: newPod(func(p *corev1.Pod) {
				p.Status.Phase = corev1.PodPending
			}),
			now: tMin(99),
			want: setecv1alpha1.SandboxStatus{
				Phase:              setecv1alpha1.SandboxPhaseFailed,
				Reason:             ReasonTimeout,
				PodName:            "demo-vm",
				ExitCode:           ptrInt32(1),
				StartedAt:          ptrTime(t0),
				LastTransitionTime: ptrTime(tMin(5)),
			},
		},
		{
			name: "terminal Completed is sticky even with nil Pod observation",
			sandbox: newSandbox(withStatus(setecv1alpha1.SandboxStatus{
				Phase:              setecv1alpha1.SandboxPhaseCompleted,
				PodName:            "demo-vm",
				ExitCode:           ptrInt32(0),
				StartedAt:          ptrTime(t0),
				LastTransitionTime: ptrTime(tMin(5)),
			})),
			pod: nil,
			now: tMin(42),
			want: setecv1alpha1.SandboxStatus{
				Phase:              setecv1alpha1.SandboxPhaseCompleted,
				PodName:            "demo-vm",
				ExitCode:           ptrInt32(0),
				StartedAt:          ptrTime(t0),
				LastTransitionTime: ptrTime(tMin(5)),
			},
		},
		{
			name:    "unknown Pod phase is treated like Pending",
			sandbox: newSandbox(),
			pod: newPod(func(p *corev1.Pod) {
				p.Status.Phase = corev1.PodUnknown
			}),
			now: tMin(1),
			want: setecv1alpha1.SandboxStatus{
				Phase:              setecv1alpha1.SandboxPhasePending,
				PodName:            "demo-vm",
				LastTransitionTime: ptrTime(tMin(1)),
			},
		},
		{
			name:    "Pod with empty name does not overwrite existing PodName",
			sandbox: newSandbox(withStatus(setecv1alpha1.SandboxStatus{PodName: "demo-vm"})),
			pod: &corev1.Pod{
				// no Name
				Status: corev1.PodStatus{Phase: corev1.PodPending},
			},
			now: tMin(1),
			want: setecv1alpha1.SandboxStatus{
				Phase:              setecv1alpha1.SandboxPhasePending,
				PodName:            "demo-vm",
				LastTransitionTime: ptrTime(tMin(1)),
			},
		},
		{
			name:    "Succeeded Pod with no reported startTime leaves StartedAt nil when Sandbox had none",
			sandbox: newSandbox(),
			pod: newPod(func(p *corev1.Pod) {
				p.Status.Phase = corev1.PodSucceeded
				p.Status.ContainerStatuses = []corev1.ContainerStatus{{
					Name: "workload",
					State: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{ExitCode: 0},
					},
				}}
			}),
			now: tMin(7),
			want: setecv1alpha1.SandboxStatus{
				Phase:              setecv1alpha1.SandboxPhaseCompleted,
				PodName:            "demo-vm",
				ExitCode:           ptrInt32(0),
				LastTransitionTime: ptrTime(tMin(7)),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := Derive(tt.sandbox, tt.pod, tt.now)
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("Derive() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestDerive_Idempotent feeds the deriver's output back in as the Sandbox's
// prior status and asserts the second invocation is a no-op. This pins down
// the stability contract documented on SandboxStatus.
func TestDerive_Idempotent(t *testing.T) {
	t.Parallel()

	type tc struct {
		name    string
		sandbox *setecv1alpha1.Sandbox
		pod     *corev1.Pod
		now     time.Time
	}

	tests := []tc{
		{
			name:    "Pending stays identical on a second call",
			sandbox: newSandbox(),
			pod: newPod(func(p *corev1.Pod) {
				p.Status.Phase = corev1.PodPending
			}),
			now: tMin(1),
		},
		{
			name:    "Running stays identical on a second call",
			sandbox: newSandbox(),
			pod: newPod(func(p *corev1.Pod) {
				p.Status.Phase = corev1.PodRunning
				p.Status.StartTime = ptrTime(t0)
			}),
			now: tMin(4),
		},
		{
			name:    "Completed stays identical on a second call",
			sandbox: newSandbox(),
			pod: newPod(func(p *corev1.Pod) {
				p.Status.Phase = corev1.PodSucceeded
				p.Status.StartTime = ptrTime(t0)
				p.Status.ContainerStatuses = []corev1.ContainerStatus{{
					Name: "workload",
					State: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{ExitCode: 0},
					},
				}}
			}),
			now: tMin(5),
		},
		{
			name:    "Failed stays identical on a second call",
			sandbox: newSandbox(),
			pod: newPod(func(p *corev1.Pod) {
				p.Status.Phase = corev1.PodFailed
				p.Status.StartTime = ptrTime(t0)
				p.Status.ContainerStatuses = []corev1.ContainerStatus{{
					Name: "workload",
					State: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{
							ExitCode: 2,
							Reason:   "Error",
						},
					},
				}}
			}),
			now: tMin(5),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			first := Derive(tt.sandbox, tt.pod, tt.now)
			// Feed the result back in as prior status. A second call
			// at the same `now` must return an identical status.
			sb2 := tt.sandbox.DeepCopy()
			sb2.Status = first
			second := Derive(sb2, tt.pod, tt.now)
			if diff := cmp.Diff(first, second); diff != "" {
				t.Errorf("Derive is not idempotent (-first +second):\n%s", diff)
			}
			// A later call with a later `now` but no Pod state change
			// must still not move LastTransitionTime forward.
			third := Derive(sb2, tt.pod, tt.now.Add(3*time.Minute))
			if diff := cmp.Diff(first, third); diff != "" {
				t.Errorf("Derive refreshed status without a phase change (-first +third):\n%s", diff)
			}
		})
	}
}

// TestDerive_NoReverseTransitions asserts explicitly that a Sandbox already
// in a terminal phase never returns Pending or Running, regardless of the
// Pod state the deriver is fed. This complements the sticky-phase cases in
// TestDerive with a focused contract test.
func TestDerive_NoReverseTransitions(t *testing.T) {
	t.Parallel()

	terminal := []setecv1alpha1.SandboxPhase{
		setecv1alpha1.SandboxPhaseCompleted,
		setecv1alpha1.SandboxPhaseFailed,
	}
	pods := map[string]*corev1.Pod{
		"nil":     nil,
		"pending": newPod(func(p *corev1.Pod) { p.Status.Phase = corev1.PodPending }),
		"running": newPod(func(p *corev1.Pod) {
			p.Status.Phase = corev1.PodRunning
			p.Status.StartTime = ptrTime(t0)
		}),
		"succeeded": newPod(func(p *corev1.Pod) {
			p.Status.Phase = corev1.PodSucceeded
			p.Status.ContainerStatuses = []corev1.ContainerStatus{{
				Name: "workload",
				State: corev1.ContainerState{
					Terminated: &corev1.ContainerStateTerminated{ExitCode: 0},
				},
			}}
		}),
		"failed": newPod(func(p *corev1.Pod) {
			p.Status.Phase = corev1.PodFailed
			p.Status.ContainerStatuses = []corev1.ContainerStatus{{
				Name: "workload",
				State: corev1.ContainerState{
					Terminated: &corev1.ContainerStateTerminated{ExitCode: 1},
				},
			}}
		}),
		"imagepullbackoff": newPod(func(p *corev1.Pod) {
			p.Status.Phase = corev1.PodPending
			p.Status.ContainerStatuses = []corev1.ContainerStatus{{
				Name: "workload",
				State: corev1.ContainerState{
					Waiting: &corev1.ContainerStateWaiting{
						Reason: waitReasonImagePullBackOff,
					},
				},
			}}
		}),
	}

	for _, phase := range terminal {
		for podName, pod := range pods {
			t.Run(string(phase)+"/"+podName, func(t *testing.T) {
				t.Parallel()
				sb := newSandbox(withStatus(setecv1alpha1.SandboxStatus{
					Phase:              phase,
					PodName:            "demo-vm",
					ExitCode:           ptrInt32(0),
					StartedAt:          ptrTime(t0),
					LastTransitionTime: ptrTime(tMin(5)),
				}))
				got := Derive(sb, pod, tMin(99))
				if got.Phase != phase {
					t.Errorf("Derive flipped terminal phase %q to %q", phase, got.Phase)
				}
				if got.Phase == setecv1alpha1.SandboxPhasePending ||
					got.Phase == setecv1alpha1.SandboxPhaseRunning {
					t.Errorf("Derive returned non-terminal phase %q from terminal input", got.Phase)
				}
			})
		}
	}
}

// TestInternalHelpers exercises the defensive guards on the package-private
// helper functions that Derive's happy paths can never reach (nil Pod, no
// waiting state, nil startedAt). Covering these keeps the package at a
// tight coverage bar and documents the intended no-op behavior.
func TestInternalHelpers(t *testing.T) {
	t.Parallel()

	t.Run("terminatedExitAndReason handles nil Pod", func(t *testing.T) {
		t.Parallel()
		exit, reason := terminatedExitAndReason(nil)
		if exit != 0 || reason != "" {
			t.Errorf("want (0, \"\"), got (%d, %q)", exit, reason)
		}
	})

	t.Run("terminatedExitAndReason ignores containers with no terminated state", func(t *testing.T) {
		t.Parallel()
		pod := &corev1.Pod{
			Status: corev1.PodStatus{
				ContainerStatuses: []corev1.ContainerStatus{{
					Name:  "workload",
					State: corev1.ContainerState{}, // neither Waiting nor Terminated
				}},
			},
		}
		exit, reason := terminatedExitAndReason(pod)
		if exit != 0 || reason != "" {
			t.Errorf("want (0, \"\"), got (%d, %q)", exit, reason)
		}
	})

	t.Run("imagePullStuck handles nil Pod", func(t *testing.T) {
		t.Parallel()
		if imagePullStuck(nil, t0) {
			t.Error("imagePullStuck(nil) returned true, want false")
		}
	})

	t.Run("imagePullStuck ignores containers with no Waiting state", func(t *testing.T) {
		t.Parallel()
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				CreationTimestamp: metav1.NewTime(t0),
			},
			Status: corev1.PodStatus{
				ContainerStatuses: []corev1.ContainerStatus{{
					Name:  "workload",
					State: corev1.ContainerState{}, // no Waiting, no Terminated
				}},
			},
		}
		if imagePullStuck(pod, tMin(99)) {
			t.Error("imagePullStuck returned true for a container with no Waiting state")
		}
	})

	t.Run("timedOut returns false when startedAt is nil", func(t *testing.T) {
		t.Parallel()
		sb := newSandbox(withTimeout(1 * time.Minute))
		if timedOut(sb, nil, tMin(99)) {
			t.Error("timedOut returned true for nil startedAt")
		}
	})

	t.Run("timedOut returns false when Sandbox is nil", func(t *testing.T) {
		t.Parallel()
		if timedOut(nil, ptrTime(t0), tMin(99)) {
			t.Error("timedOut returned true for nil Sandbox")
		}
	})

	t.Run("timedOut returns false when Lifecycle is nil", func(t *testing.T) {
		t.Parallel()
		sb := newSandbox()
		if timedOut(sb, ptrTime(t0), tMin(99)) {
			t.Error("timedOut returned true for nil Lifecycle")
		}
	})
}

// ptrTime wraps a time.Time into a *metav1.Time for concise test fixtures.
func ptrTime(t time.Time) *metav1.Time {
	mt := metav1.NewTime(t)
	return &mt
}

// TestDerive_Phase3CoordinatorPhasesPassthrough asserts Derive leaves
// Paused, Snapshotting, and Restoring phases intact while the Pod is
// still running. The snapshot.Coordinator owns those transitions.
func TestDerive_Phase3CoordinatorPhasesPassthrough(t *testing.T) {
	t.Parallel()

	pod := newPod(func(p *corev1.Pod) {
		p.Status.Phase = corev1.PodRunning
		p.Status.StartTime = ptrTime(t0)
	})

	cases := []setecv1alpha1.SandboxPhase{
		setecv1alpha1.SandboxPhasePaused,
		setecv1alpha1.SandboxPhaseSnapshotting,
		setecv1alpha1.SandboxPhaseRestoring,
	}
	for _, phase := range cases {
		t.Run(string(phase), func(t *testing.T) {
			t.Parallel()
			sb := newSandbox(withStatus(setecv1alpha1.SandboxStatus{
				Phase:   phase,
				PodName: "demo-vm",
			}))
			out := Derive(sb, pod, tMin(1))
			if out.Phase != phase {
				t.Fatalf("Derive flipped %q to %q; want passthrough", phase, out.Phase)
			}
		})
	}
}

// TestDerive_Phase3_TerminalPodOverridesTransient asserts that a
// PodSucceeded or PodFailed state still pulls a transient phase to
// the appropriate terminal phase — the coordinator cannot reconcile
// a dead VM.
func TestDerive_Phase3_TerminalPodOverridesTransient(t *testing.T) {
	t.Parallel()

	pod := newPod(func(p *corev1.Pod) {
		p.Status.Phase = corev1.PodSucceeded
		p.Status.StartTime = ptrTime(t0)
	})
	sb := newSandbox(withStatus(setecv1alpha1.SandboxStatus{
		Phase: setecv1alpha1.SandboxPhasePaused,
	}))
	out := Derive(sb, pod, tMin(1))
	if out.Phase != setecv1alpha1.SandboxPhaseCompleted {
		t.Fatalf("Phase = %q, want Completed (terminal overrides transient)", out.Phase)
	}
}

// Ensure go-cmp still imports after the additions; keeps cmp
// available for richer diffs if the above tests grow. No-op.
var _ = cmp.Diff
