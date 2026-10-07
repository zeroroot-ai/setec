// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package snapshot

import (
	"testing"

	"github.com/google/go-cmp/cmp"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
)

// newSandbox is a small builder keeping test cases readable.
func newSandbox(image string) *setecv1alpha1.Sandbox {
	return &setecv1alpha1.Sandbox{
		Namespace: "t-a", Name: "s",
		Spec: setecv1alpha1.SandboxSpec{
			Image: image,
		},
	}
}

func newSnapshot(ns, name, class, image string) *setecv1alpha1.Snapshot {
	return &setecv1alpha1.Snapshot{
		Namespace: ns, Name: name,
		Spec: setecv1alpha1.SnapshotSpec{
			SandboxClass: class,
			ImageRef:     image,
			Node:         "node-a",
			StorageRef:   name,
		},
	}
}

func newClass(name string) *setecv1alpha1.SandboxClass {
	return &setecv1alpha1.SandboxClass{
		Name: name,
		Spec: setecv1alpha1.SandboxClassSpec{},
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
			snap:  newSnapshot("t-a", "snap-1", "standard", "ghcr.io/org/app:v1"),
			class: newClass("standard"),
			want:  nil,
		},
		{
			name: "cpu template mismatch",
			sb:   newSandbox(""),
			snap: func() *setecv1alpha1.Snapshot {
				s := newSnapshot("t-a", "snap-1", "standard", "ghcr.io/org/app:v1")
				s.Spec.CPUTemplate = "fleet-v1"
				return s
			}(),
			class: newClass("standard"),
			want: []ConstraintViolation{{
				Field:   "spec.sandboxClassName",
				Message: `Snapshot "snap-1" was captured with CPU template "fleet-v1" but the resolved class uses ""`,
			}},
		},
		{
			name:  "sandbox image empty is accepted",
			sb:    newSandbox(""),
			snap:  newSnapshot("t-a", "snap-1", "standard", "ghcr.io/org/app:v1"),
			class: newClass("standard"),
			want:  nil,
		},
		{
			name:  "nil sandbox",
			sb:    nil,
			snap:  newSnapshot("t-a", "s", "c", "i"),
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
			snap:  newSnapshot("t-b", "snap-1", "standard", "ghcr.io/org/app:v1"),
			class: newClass("standard"),
			want: []ConstraintViolation{{
				Field:   "spec.snapshotRef.name",
				Message: `Snapshot "snap-1" is in namespace "t-b" but Sandbox is in namespace "t-a"; cross-namespace restore is not permitted`,
			}},
		},
		{
			name:  "class mismatch",
			sb:    newSandbox(""),
			snap:  newSnapshot("t-a", "snap-1", "fast", "ghcr.io/org/app:v1"),
			class: newClass("standard"),
			want: []ConstraintViolation{{
				Field:   "spec.sandboxClassName",
				Message: `Snapshot "snap-1" was captured under SandboxClass "fast" but the resolved class is "standard"`,
			}},
		},
		{
			name:  "image mismatch",
			sb:    newSandbox("ghcr.io/org/app:v2"),
			snap:  newSnapshot("t-a", "snap-1", "standard", "ghcr.io/org/app:v1"),
			class: newClass("standard"),
			want: []ConstraintViolation{{
				Field:   "spec.image",
				Message: `Sandbox requests image "ghcr.io/org/app:v2" but Snapshot "snap-1" was captured from image "ghcr.io/org/app:v1"`,
			}},
		},
		{
			name:  "multiple violations combine",
			sb:    newSandbox("ghcr.io/org/app:v2"),
			snap:  newSnapshot("t-b", "snap-1", "fast", "ghcr.io/org/app:v1"),
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
			},
		},
		{
			name:  "nil class: only non-class checks run",
			sb:    newSandbox("ghcr.io/org/app:v2"),
			snap:  newSnapshot("t-a", "snap-1", "standard", "ghcr.io/org/app:v1"),
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
