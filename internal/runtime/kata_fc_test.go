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

func TestKataFCDispatcher_Name(t *testing.T) {
	t.Parallel()
	d := NewKataFCDispatcher(BackendConfig{RuntimeClassName: "kata-fc"})
	if got := d.Name(); got != BackendKataFC {
		t.Errorf("Name() = %q, want %q", got, BackendKataFC)
	}
}

func TestKataFCDispatcher_RuntimeClassName(t *testing.T) {
	t.Parallel()
	d := NewKataFCDispatcher(BackendConfig{RuntimeClassName: "my-kata-fc"})
	if got := d.RuntimeClassName(); got != "my-kata-fc" {
		t.Errorf("RuntimeClassName() = %q, want %q", got, "my-kata-fc")
	}
}

func TestKataFCDispatcher_NodeAffinity(t *testing.T) {
	t.Parallel()
	d := NewKataFCDispatcher(BackendConfig{RuntimeClassName: "kata-fc"})
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

	// First expression: backend capability label.
	wantLabel := runtimeAffinityLabel(BackendKataFC)
	if exprs[0].Key != wantLabel {
		t.Errorf("MatchExpressions[0].Key = %q, want %q", exprs[0].Key, wantLabel)
	}
	if exprs[0].Operator != corev1.NodeSelectorOpIn {
		t.Errorf("MatchExpressions[0].Operator = %q, want In", exprs[0].Operator)
	}
	if len(exprs[0].Values) != 1 || exprs[0].Values[0] != "true" {
		t.Errorf("MatchExpressions[0].Values = %v, want [true]", exprs[0].Values)
	}

	// Second expression: OS constraint.
	if exprs[1].Key != "kubernetes.io/os" {
		t.Errorf("MatchExpressions[1].Key = %q, want kubernetes.io/os", exprs[1].Key)
	}
	if len(exprs[1].Values) != 1 || exprs[1].Values[0] != "linux" {
		t.Errorf("MatchExpressions[1].Values = %v, want [linux]", exprs[1].Values)
	}
}

func TestKataFCDispatcher_Overhead_Default(t *testing.T) {
	t.Parallel()
	d := NewKataFCDispatcher(BackendConfig{Install: true, RuntimeClassName: "kata-fc"})
	oh := d.Overhead()
	wantMem := resource.MustParse("128Mi")
	wantCPU := resource.MustParse("250m")

	if mem, ok := oh[corev1.ResourceMemory]; !ok || mem.Cmp(wantMem) != 0 {
		t.Errorf("default memory overhead = %v, want %v", oh[corev1.ResourceMemory], wantMem)
	}
	if cpu, ok := oh[corev1.ResourceCPU]; !ok || cpu.Cmp(wantCPU) != 0 {
		t.Errorf("default CPU overhead = %v, want %v", oh[corev1.ResourceCPU], wantCPU)
	}
}

func TestKataFCDispatcher_Overhead_Custom(t *testing.T) {
	t.Parallel()
	custom := corev1.ResourceList{
		corev1.ResourceMemory: resource.MustParse("256Mi"),
		corev1.ResourceCPU:    resource.MustParse("500m"),
	}
	d := NewKataFCDispatcher(BackendConfig{
		Install:          true,
		RuntimeClassName: "kata-fc",
		DefaultOverhead:  custom,
	})
	oh := d.Overhead()
	wantMem := resource.MustParse("256Mi")
	wantCPU := resource.MustParse("500m")

	if mem := oh[corev1.ResourceMemory]; mem.Cmp(wantMem) != 0 {
		t.Errorf("custom memory overhead = %v, want %v", mem, wantMem)
	}
	if cpu := oh[corev1.ResourceCPU]; cpu.Cmp(wantCPU) != 0 {
		t.Errorf("custom CPU overhead = %v, want %v", cpu, wantCPU)
	}
}

func TestKataFCDispatcher_Overhead_ExternalRuntimeClass_Nil(t *testing.T) {
	t.Parallel()
	// Install:false → the RuntimeClass is externally managed; even with a
	// DefaultOverhead set, Overhead() returns nil so the operator does not stamp
	// a (possibly mismatching) overhead — admission applies the class's own
	// (setec#78).
	d := NewKataFCDispatcher(BackendConfig{
		RuntimeClassName: "kata-fc",
		DefaultOverhead: corev1.ResourceList{
			corev1.ResourceMemory: resource.MustParse("128Mi"),
		},
	})
	if oh := d.Overhead(); oh != nil {
		t.Errorf("Overhead() = %v, want nil for an externally-managed RuntimeClass", oh)
	}
}

func TestKataFCDispatcher_MutatePod_Noop(t *testing.T) {
	t.Parallel()
	d := NewKataFCDispatcher(BackendConfig{RuntimeClassName: "kata-fc"})
	pod := &corev1.Pod{}
	params := map[string]string{"somekey": "somevalue"}
	if err := d.MutatePod(pod, params); err != nil {
		t.Errorf("MutatePod() returned unexpected error: %v", err)
	}
	// Pod must remain entirely unmodified.
	if len(pod.Annotations) != 0 {
		t.Errorf("MutatePod() modified pod.Annotations: %v", pod.Annotations)
	}
	if len(pod.Labels) != 0 {
		t.Errorf("MutatePod() modified pod.Labels: %v", pod.Labels)
	}
}
