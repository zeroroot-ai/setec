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

const (
	testAffinityTrue  = "true"
	testAffinityOS    = "kubernetes.io/os"
	testAffinityLinux = "linux"
)

func TestGVisorDispatcher_Name(t *testing.T) {
	t.Parallel()
	d := NewGVisorDispatcher(BackendConfig{RuntimeClassName: "runsc"})
	if got := d.Name(); got != BackendGVisor {
		t.Errorf("Name() = %q, want %q", got, BackendGVisor)
	}
}

func TestGVisorDispatcher_RuntimeClassName(t *testing.T) {
	t.Parallel()
	d := NewGVisorDispatcher(BackendConfig{RuntimeClassName: "runsc"})
	if got := d.RuntimeClassName(); got != "runsc" {
		t.Errorf("RuntimeClassName() = %q, want runsc", got)
	}
}

func TestGVisorDispatcher_NodeAffinity(t *testing.T) {
	t.Parallel()
	d := NewGVisorDispatcher(BackendConfig{RuntimeClassName: "runsc"})
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

	wantLabel := runtimeAffinityLabel(BackendGVisor)
	if exprs[0].Key != wantLabel {
		t.Errorf("MatchExpressions[0].Key = %q, want %q", exprs[0].Key, wantLabel)
	}
	if exprs[0].Operator != corev1.NodeSelectorOpIn {
		t.Errorf("MatchExpressions[0].Operator = %q, want In", exprs[0].Operator)
	}
	if len(exprs[0].Values) != 1 || exprs[0].Values[0] != testAffinityTrue {
		t.Errorf("MatchExpressions[0].Values = %v, want [true]", exprs[0].Values)
	}
	if exprs[1].Key != testAffinityOS {
		t.Errorf("MatchExpressions[1].Key = %q, want kubernetes.io/os", exprs[1].Key)
	}
	if len(exprs[1].Values) != 1 || exprs[1].Values[0] != testAffinityLinux {
		t.Errorf("MatchExpressions[1].Values = %v, want [linux]", exprs[1].Values)
	}
}

func TestGVisorDispatcher_Overhead_Default(t *testing.T) {
	t.Parallel()
	d := NewGVisorDispatcher(BackendConfig{Install: true, RuntimeClassName: "runsc"})
	oh := d.Overhead()
	wantMem := resource.MustParse("40Mi")
	wantCPU := resource.MustParse("50m")

	if mem, ok := oh[corev1.ResourceMemory]; !ok || mem.Cmp(wantMem) != 0 {
		t.Errorf("default memory overhead = %v, want %v", oh[corev1.ResourceMemory], wantMem)
	}
	if cpu, ok := oh[corev1.ResourceCPU]; !ok || cpu.Cmp(wantCPU) != 0 {
		t.Errorf("default CPU overhead = %v, want %v", oh[corev1.ResourceCPU], wantCPU)
	}
}

func TestGVisorDispatcher_Overhead_Custom(t *testing.T) {
	t.Parallel()
	custom := corev1.ResourceList{
		corev1.ResourceMemory: resource.MustParse("80Mi"),
		corev1.ResourceCPU:    resource.MustParse("100m"),
	}
	d := NewGVisorDispatcher(BackendConfig{
		Install:          true,
		RuntimeClassName: "runsc",
		DefaultOverhead:  custom,
	})
	oh := d.Overhead()
	if mem := oh[corev1.ResourceMemory]; mem.Cmp(resource.MustParse("80Mi")) != 0 {
		t.Errorf("custom memory overhead = %v, want 80Mi", mem)
	}
	if cpu := oh[corev1.ResourceCPU]; cpu.Cmp(resource.MustParse("100m")) != 0 {
		t.Errorf("custom CPU overhead = %v, want 100m", cpu)
	}
}

func TestGVisorDispatcher_MutatePod_Noop(t *testing.T) {
	t.Parallel()
	d := NewGVisorDispatcher(BackendConfig{RuntimeClassName: "runsc"})
	pod := &corev1.Pod{}
	params := map[string]string{"anything": "ignored"}
	if err := d.MutatePod(pod, params); err != nil {
		t.Errorf("MutatePod() returned unexpected error: %v", err)
	}
	if len(pod.Annotations) != 0 {
		t.Errorf("MutatePod() modified pod.Annotations: %v", pod.Annotations)
	}
	if len(pod.Labels) != 0 {
		t.Errorf("MutatePod() modified pod.Labels: %v", pod.Labels)
	}
}
