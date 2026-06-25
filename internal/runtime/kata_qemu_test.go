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
	"errors"
	"maps"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestKataQEMUDispatcher_Name(t *testing.T) {
	t.Parallel()
	d := NewKataQEMUDispatcher(BackendConfig{RuntimeClassName: "kata-qemu"})
	if got := d.Name(); got != BackendKataQEMU {
		t.Errorf("Name() = %q, want %q", got, BackendKataQEMU)
	}
}

func TestKataQEMUDispatcher_RuntimeClassName(t *testing.T) {
	t.Parallel()
	d := NewKataQEMUDispatcher(BackendConfig{RuntimeClassName: "my-kata-qemu"})
	if got := d.RuntimeClassName(); got != "my-kata-qemu" {
		t.Errorf("RuntimeClassName() = %q, want %q", got, "my-kata-qemu")
	}
}

func TestKataQEMUDispatcher_NodeAffinity(t *testing.T) {
	t.Parallel()
	d := NewKataQEMUDispatcher(BackendConfig{RuntimeClassName: "kata-qemu"})
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

	wantLabel := runtimeAffinityLabel(BackendKataQEMU)
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

func TestKataQEMUDispatcher_Overhead_Default(t *testing.T) {
	t.Parallel()
	d := NewKataQEMUDispatcher(BackendConfig{Install: true, RuntimeClassName: "kata-qemu"})
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

func TestKataQEMUDispatcher_Overhead_Custom(t *testing.T) {
	t.Parallel()
	custom := corev1.ResourceList{
		corev1.ResourceMemory: resource.MustParse("512Mi"),
		corev1.ResourceCPU:    resource.MustParse("1"),
	}
	d := NewKataQEMUDispatcher(BackendConfig{
		Install:          true,
		RuntimeClassName: "kata-qemu",
		DefaultOverhead:  custom,
	})
	oh := d.Overhead()
	if mem := oh[corev1.ResourceMemory]; mem.Cmp(resource.MustParse("512Mi")) != 0 {
		t.Errorf("custom memory overhead = %v, want 512Mi", mem)
	}
	if cpu := oh[corev1.ResourceCPU]; cpu.Cmp(resource.MustParse("1")) != 0 {
		t.Errorf("custom CPU overhead = %v, want 1", cpu)
	}
}

func TestKataQEMUDispatcher_MutatePod(t *testing.T) {
	t.Parallel()

	cfg := BackendConfig{RuntimeClassName: "kata-qemu"}

	tests := []struct {
		name         string
		params       map[string]string
		wantAnno     map[string]string // expected annotations after mutation
		wantErrIs    error
		wantNoMutate bool // pod must remain unmutated when an error is expected
	}{
		{
			name:     "no params — no annotations added",
			params:   map[string]string{},
			wantAnno: map[string]string{},
		},
		{
			name:   "vcpus only",
			params: map[string]string{"vcpus": "2"},
			wantAnno: map[string]string{
				"io.katacontainers.config.hypervisor.default_vcpus": "2",
			},
		},
		{
			name:   "memory only",
			params: map[string]string{"memory": "1024"},
			wantAnno: map[string]string{
				"io.katacontainers.config.hypervisor.default_memory": "1024",
			},
		},
		{
			name:   "vcpus and memory",
			params: map[string]string{"vcpus": "4", "memory": "2048"},
			wantAnno: map[string]string{
				"io.katacontainers.config.hypervisor.default_vcpus":  "4",
				"io.katacontainers.config.hypervisor.default_memory": "2048",
			},
		},
		{
			name:         "unknown key — ErrUnknownKataParam",
			params:       map[string]string{"bogus": "val"},
			wantErrIs:    ErrUnknownKataParam,
			wantNoMutate: true,
		},
		{
			name:         "mixed valid and unknown keys — ErrUnknownKataParam",
			params:       map[string]string{"vcpus": "2", "bogus": "val"},
			wantErrIs:    ErrUnknownKataParam,
			wantNoMutate: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := NewKataQEMUDispatcher(cfg)
			pod := &corev1.Pod{}

			err := d.MutatePod(pod, tc.params)

			if tc.wantErrIs != nil {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				if !errors.Is(err, tc.wantErrIs) {
					t.Fatalf("error %v does not wrap %v", err, tc.wantErrIs)
				}
				if tc.wantNoMutate && len(pod.Annotations) != 0 {
					t.Errorf("pod was mutated despite error: %v", pod.Annotations)
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			for k, wantV := range tc.wantAnno {
				if got := pod.Annotations[k]; got != wantV {
					t.Errorf("annotation[%q] = %q, want %q", k, got, wantV)
				}
			}
			// No extra annotations beyond the expected set.
			for k := range pod.Annotations {
				if _, expected := tc.wantAnno[k]; !expected {
					t.Errorf("unexpected annotation[%q] = %q", k, pod.Annotations[k])
				}
			}
		})
	}
}

func TestKataQEMUDispatcher_MutatePod_Idempotent(t *testing.T) {
	t.Parallel()
	d := NewKataQEMUDispatcher(BackendConfig{RuntimeClassName: "kata-qemu"})
	pod := &corev1.Pod{}
	params := map[string]string{"vcpus": "4", "memory": "2048"}

	if err := d.MutatePod(pod, params); err != nil {
		t.Fatalf("first MutatePod: %v", err)
	}
	firstAnno := make(map[string]string, len(pod.Annotations))
	maps.Copy(firstAnno, pod.Annotations)

	if err := d.MutatePod(pod, params); err != nil {
		t.Fatalf("second MutatePod: %v", err)
	}

	if len(pod.Annotations) != len(firstAnno) {
		t.Errorf("second call changed annotation count: got %d, want %d", len(pod.Annotations), len(firstAnno))
	}
	for k, v := range firstAnno {
		if pod.Annotations[k] != v {
			t.Errorf("annotation[%q] changed: got %q, want %q", k, pod.Annotations[k], v)
		}
	}
}
