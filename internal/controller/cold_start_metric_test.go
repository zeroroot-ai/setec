// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package controller

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
	"github.com/zeroroot-ai/setec/internal/metrics"
)

// coldStartSamples returns the sample count and sum of
// setec_sandbox_cold_start_seconds for the runtime label.
func coldStartSamples(t *testing.T, reg *prometheus.Registry, runtime string) (uint64, float64) {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range families {
		if mf.GetName() != "setec_sandbox_cold_start_seconds" {
			continue
		}
		for _, m := range mf.GetMetric() {
			if labelValue(m, metrics.LabelRuntime) == runtime {
				return m.GetHistogram().GetSampleCount(), m.GetHistogram().GetSampleSum()
			}
		}
	}
	return 0, 0
}

func labelValue(m *dto.Metric, name string) string {
	for _, l := range m.GetLabel() {
		if l.GetName() == name {
			return l.GetValue()
		}
	}
	return ""
}

// The cold-start histogram measures Sandbox creation to Pod Running. It
// used to subtract the Sandbox creation time from pod.Status.StartTime, the
// moment the kubelet accepted the Pod. Both are second-precision and usually
// fall in the same second, so the difference was 0, and a 0 was dropped: the
// e2e suite found no sample for any runtime (setec#22). The workload
// container's running start is the Running moment.
func TestRecordTransitionObservesColdStartToContainerRunning(t *testing.T) {
	created := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	running := created.Add(2 * time.Second)

	reg := prometheus.NewRegistry()
	r := &SandboxReconciler{MetricsCollector: metrics.NewCollectorsWith(reg)}
	sb := &setecv1alpha1.Sandbox{
		Name: "sb", Namespace: "ns", CreationTimestamp: metav1.NewTime(created)}
	cls := &setecv1alpha1.SandboxClass{Name: "cls"}
	accepted := metav1.NewTime(created)
	pod := &corev1.Pod{
		CreationTimestamp: metav1.NewTime(created),
		Status: corev1.PodStatus{
			StartTime: &accepted,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:  "workload",
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(running)}},
			}},
		},
	}
	curr := setecv1alpha1.SandboxStatus{
		Phase:     setecv1alpha1.SandboxPhaseRunning,
		StartedAt: &accepted,
		Runtime:   &setecv1alpha1.SandboxRuntimeStatus{Chosen: "launcher"},
	}

	r.recordTransition(sb, cls, setecv1alpha1.SandboxPhasePending, curr, pod, "")

	count, sum := coldStartSamples(t, reg, "launcher")
	if count != 1 {
		t.Fatalf("cold-start samples for launcher = %d, want 1", count)
	}
	if sum != 2 {
		t.Errorf("cold-start = %gs, want 2s (Sandbox creation to container running)", sum)
	}
}

// A Sandbox that reaches Running within the second it was created still
// counts. Dropping it hides the fastest starts, which are the ones a warm
// pool exists to produce.
func TestRecordTransitionKeepsSubSecondColdStart(t *testing.T) {
	created := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	reg := prometheus.NewRegistry()
	r := &SandboxReconciler{MetricsCollector: metrics.NewCollectorsWith(reg)}
	sb := &setecv1alpha1.Sandbox{Name: "sb", CreationTimestamp: metav1.NewTime(created)}
	pod := &corev1.Pod{
		CreationTimestamp: metav1.NewTime(created),
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name:  "workload",
			State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(created)}},
		}}},
	}
	curr := setecv1alpha1.SandboxStatus{
		Phase:   setecv1alpha1.SandboxPhaseRunning,
		Runtime: &setecv1alpha1.SandboxRuntimeStatus{Chosen: "gvisor"},
	}

	r.recordTransition(sb, nil, setecv1alpha1.SandboxPhasePending, curr, pod, "")

	if count, _ := coldStartSamples(t, reg, "gvisor"); count != 1 {
		t.Fatalf("cold-start samples for gvisor = %d, want 1", count)
	}
}
