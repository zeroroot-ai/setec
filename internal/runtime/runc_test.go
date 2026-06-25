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

package runtime

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestRuncDispatcher_Name(t *testing.T) {
	t.Parallel()
	d := NewRuncDispatcher(BackendConfig{RuntimeClassName: "runc"})
	if got := d.Name(); got != BackendRunc {
		t.Errorf("Name() = %q, want %q", got, BackendRunc)
	}
}

func TestRuncDispatcher_RuntimeClassName(t *testing.T) {
	t.Parallel()
	d := NewRuncDispatcher(BackendConfig{RuntimeClassName: "my-runc"})
	if got := d.RuntimeClassName(); got != "my-runc" {
		t.Errorf("RuntimeClassName() = %q, want my-runc", got)
	}
}

func TestRuncDispatcher_NodeAffinity(t *testing.T) {
	t.Parallel()
	d := NewRuncDispatcher(BackendConfig{RuntimeClassName: "runc"})
	aff := d.NodeAffinity()
	if aff == nil {
		t.Fatal("NodeAffinity() returned nil")
	}
	req := aff.RequiredDuringSchedulingIgnoredDuringExecution
	if req == nil {
		t.Fatal("RequiredDuringSchedulingIgnoredDuringExecution is nil")
	}
	if len(req.NodeSelectorTerms) != 1 {
		t.Fatalf("expected 1 NodeSelectorTerm, got %d", len(req.NodeSelectorTerms))
	}
	exprs := req.NodeSelectorTerms[0].MatchExpressions
	if len(exprs) != 2 {
		t.Fatalf("expected 2 MatchExpressions, got %d", len(exprs))
	}

	wantLabel := runtimeAffinityLabel(BackendRunc)
	if exprs[0].Key != wantLabel {
		t.Errorf("MatchExpressions[0].Key = %q, want %q", exprs[0].Key, wantLabel)
	}
	if exprs[0].Operator != corev1.NodeSelectorOpIn {
		t.Errorf("MatchExpressions[0].Operator = %q, want In", exprs[0].Operator)
	}
	if len(exprs[0].Values) != 1 || exprs[0].Values[0] != "true" {
		t.Errorf("MatchExpressions[0].Values = %v, want [true]", exprs[0].Values)
	}
	if exprs[1].Key != "kubernetes.io/os" {
		t.Errorf("MatchExpressions[1].Key = %q, want kubernetes.io/os", exprs[1].Key)
	}
	if len(exprs[1].Values) != 1 || exprs[1].Values[0] != "linux" {
		t.Errorf("MatchExpressions[1].Values = %v, want [linux]", exprs[1].Values)
	}
}

func TestRuncDispatcher_Overhead_Default(t *testing.T) {
	t.Parallel()
	d := NewRuncDispatcher(BackendConfig{Install: true, RuntimeClassName: "runc"})
	oh := d.Overhead()
	// Default is an empty (non-nil) ResourceList — zero overhead.
	if oh == nil {
		t.Fatal("Overhead() returned nil, want empty ResourceList")
	}
	if len(oh) != 0 {
		t.Errorf("Overhead() = %v, want empty ResourceList", oh)
	}
}

func TestRuncDispatcher_Overhead_Custom(t *testing.T) {
	t.Parallel()
	custom := corev1.ResourceList{
		corev1.ResourceMemory: resource.MustParse("10Mi"),
	}
	d := NewRuncDispatcher(BackendConfig{
		Install:          true,
		RuntimeClassName: "runc",
		DefaultOverhead:  custom,
	})
	oh := d.Overhead()
	if mem := oh[corev1.ResourceMemory]; mem.Cmp(resource.MustParse("10Mi")) != 0 {
		t.Errorf("custom memory overhead = %v, want 10Mi", mem)
	}
}

func TestRuncDispatcher_MutatePod_AddsIsolationLabel(t *testing.T) {
	t.Parallel()
	d := NewRuncDispatcher(BackendConfig{RuntimeClassName: "runc"})
	pod := &corev1.Pod{} // Labels is nil initially.

	if err := d.MutatePod(pod, nil); err != nil {
		t.Fatalf("MutatePod() returned unexpected error: %v", err)
	}
	if pod.Labels == nil {
		t.Fatal("pod.Labels is nil after MutatePod")
	}
	got, ok := pod.Labels[isolationLabel]
	if !ok {
		t.Fatalf("label %q not found in pod.Labels", isolationLabel)
	}
	if got != isolationContainerOnly {
		t.Errorf("label %q = %q, want %q", isolationLabel, got, isolationContainerOnly)
	}
}

func TestRuncDispatcher_MutatePod_Idempotent(t *testing.T) {
	t.Parallel()
	d := NewRuncDispatcher(BackendConfig{RuntimeClassName: "runc"})
	pod := &corev1.Pod{}

	for i := range 3 {
		if err := d.MutatePod(pod, nil); err != nil {
			t.Fatalf("MutatePod() call %d returned unexpected error: %v", i+1, err)
		}
	}

	if len(pod.Labels) != 1 {
		t.Errorf("expected exactly 1 label after 3 MutatePod calls, got %d: %v", len(pod.Labels), pod.Labels)
	}
	if pod.Labels[isolationLabel] != isolationContainerOnly {
		t.Errorf("label %q = %q, want %q", isolationLabel, pod.Labels[isolationLabel], isolationContainerOnly)
	}
}

func TestRuncDispatcher_MutatePod_PreservesExistingLabels(t *testing.T) {
	t.Parallel()
	d := NewRuncDispatcher(BackendConfig{RuntimeClassName: "runc"})
	pod := &corev1.Pod{}
	pod.Labels = map[string]string{"existing": "label"}

	if err := d.MutatePod(pod, nil); err != nil {
		t.Fatalf("MutatePod() returned unexpected error: %v", err)
	}
	if pod.Labels["existing"] != "label" {
		t.Error("MutatePod() removed existing label")
	}
	if pod.Labels[isolationLabel] != isolationContainerOnly {
		t.Errorf("isolation label not set: %v", pod.Labels)
	}
}
