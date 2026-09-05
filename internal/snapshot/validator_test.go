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

package snapshot

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
)

// newSandbox is a small builder keeping test cases readable.
func newSandbox(image string) *setecv1alpha1.Sandbox {
	return &setecv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Namespace: "t-a", Name: "s"},
		Spec: setecv1alpha1.SandboxSpec{
			Image: image,
		},
	}
}

func newSnapshot(ns, name, class, image string, vmm setecv1alpha1.VMM) *setecv1alpha1.Snapshot {
	return &setecv1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: setecv1alpha1.SnapshotSpec{
			SandboxClass: class,
			ImageRef:     image,
			VMM:          vmm,
			Node:         "node-a",
			StorageRef:   name,
		},
	}
}

func newClass(name string) *setecv1alpha1.SandboxClass {
	return &setecv1alpha1.SandboxClass{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: setecv1alpha1.SandboxClassSpec{
			VMM: setecv1alpha1.VMMFirecracker, //nolint:staticcheck // back-compat: VMM retained until v2
		},
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name  string
		sb    *setecv1alpha1.Sandbox
		snap  *setecv1alpha1.Snapshot
		class *setecv1alpha1.SandboxClass
		want  []ConstraintViolation
	}{
		{
			name:  "happy path: all fields match",
			sb:    newSandbox("ghcr.io/org/app:v1"),
			snap:  newSnapshot("t-a", "snap-1", "standard", "ghcr.io/org/app:v1", setecv1alpha1.VMMFirecracker),
			class: newClass("standard"),
			want:  nil,
		},
		{
			name:  "sandbox image empty is accepted",
			sb:    newSandbox(""),
			snap:  newSnapshot("t-a", "snap-1", "standard", "ghcr.io/org/app:v1", setecv1alpha1.VMMFirecracker),
			class: newClass("standard"),
			want:  nil,
		},
		{
			name:  "nil sandbox",
			sb:    nil,
			snap:  newSnapshot("t-a", "s", "c", "i", setecv1alpha1.VMMFirecracker),
			class: newClass("c"),
			want:  []ConstraintViolation{{Field: "", Message: "sandbox is nil"}},
		},
		{
			name:  "nil snapshot",
			sb:    newSandbox(""),
			snap:  nil,
			class: newClass("c"),
			want:  []ConstraintViolation{{Field: "spec.snapshotRef.name", Message: "snapshot is nil"}},
		},
		{
			name:  "cross-namespace rejected",
			sb:    newSandbox(""),
			snap:  newSnapshot("t-b", "snap-1", "standard", "ghcr.io/org/app:v1", setecv1alpha1.VMMFirecracker),
			class: newClass("standard"),
			want: []ConstraintViolation{{
				Field:   "spec.snapshotRef.name",
				Message: `Snapshot "snap-1" is in namespace "t-b" but Sandbox is in namespace "t-a"; cross-namespace restore is not permitted`,
			}},
		},
		{
			name:  "class mismatch",
			sb:    newSandbox(""),
			snap:  newSnapshot("t-a", "snap-1", "fast", "ghcr.io/org/app:v1", setecv1alpha1.VMMFirecracker),
			class: newClass("standard"),
			want: []ConstraintViolation{{
				Field:   "spec.sandboxClassName",
				Message: `Snapshot "snap-1" was captured under SandboxClass "fast" but the resolved class is "standard"`,
			}},
		},
		{
			name:  "image mismatch",
			sb:    newSandbox("ghcr.io/org/app:v2"),
			snap:  newSnapshot("t-a", "snap-1", "standard", "ghcr.io/org/app:v1", setecv1alpha1.VMMFirecracker),
			class: newClass("standard"),
			want: []ConstraintViolation{{
				Field:   "spec.image",
				Message: `Sandbox requests image "ghcr.io/org/app:v2" but Snapshot "snap-1" was captured from image "ghcr.io/org/app:v1"`,
			}},
		},
		{
			name:  "vmm mismatch",
			sb:    newSandbox("ghcr.io/org/app:v1"),
			snap:  newSnapshot("t-a", "snap-1", "standard", "ghcr.io/org/app:v1", setecv1alpha1.VMMQEMU),
			class: newClass("standard"),
			want: []ConstraintViolation{{
				Field:   "spec.sandboxClassName",
				Message: `Snapshot "snap-1" was captured on VMM "qemu" but the resolved class uses VMM "firecracker"`,
			}},
		},
		{
			name:  "multiple violations combine",
			sb:    newSandbox("ghcr.io/org/app:v2"),
			snap:  newSnapshot("t-b", "snap-1", "fast", "ghcr.io/org/app:v1", setecv1alpha1.VMMQEMU),
			class: newClass("standard"),
			want: []ConstraintViolation{
				{
					Field:   "spec.snapshotRef.name",
					Message: `Snapshot "snap-1" is in namespace "t-b" but Sandbox is in namespace "t-a"; cross-namespace restore is not permitted`,
				},
				{
					Field:   "spec.sandboxClassName",
					Message: `Snapshot "snap-1" was captured under SandboxClass "fast" but the resolved class is "standard"`,
				},
				{
					Field:   "spec.image",
					Message: `Sandbox requests image "ghcr.io/org/app:v2" but Snapshot "snap-1" was captured from image "ghcr.io/org/app:v1"`,
				},
				{
					Field:   "spec.sandboxClassName",
					Message: `Snapshot "snap-1" was captured on VMM "qemu" but the resolved class uses VMM "firecracker"`,
				},
			},
		},
		{
			name:  "nil class: only non-class checks run",
			sb:    newSandbox("ghcr.io/org/app:v2"),
			snap:  newSnapshot("t-a", "snap-1", "standard", "ghcr.io/org/app:v1", setecv1alpha1.VMMFirecracker),
			class: nil,
			want: []ConstraintViolation{{
				Field:   "spec.image",
				Message: `Sandbox requests image "ghcr.io/org/app:v2" but Snapshot "snap-1" was captured from image "ghcr.io/org/app:v1"`,
			}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Validate(tc.sb, tc.snap, tc.class)
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Fatalf("Validate mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestConstraintViolationString(t *testing.T) {
	cv := ConstraintViolation{Field: "spec.foo", Message: "boom"}
	if got, want := cv.String(), "spec.foo: boom"; got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
}
