// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package frontend

import (
	"testing"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
)

// TestSnapshotVCPU proves the vCPU annotation, which a user of the
// namespace can write, is bounded to the 32 bits of SandboxSpec.VCPU
// (setec#306, code-scanning go/incorrect-integer-conversion).
func TestSnapshotVCPU(t *testing.T) {
	for in, want := range map[string]int32{"1": 1, "4": 4, "2147483647": 2147483647} {
		snap := &setecv1alpha1.Snapshot{}
		snap.Annotations = map[string]string{setecv1alpha1.SnapshotVCPUAnnotation: in}
		if got, ok := snapshotVCPU(snap); !ok || got != want {
			t.Errorf("%q: got %d, %v, want %d", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "0", "-1", "abc", "2147483648", "4294967297"} {
		snap := &setecv1alpha1.Snapshot{}
		snap.Annotations = map[string]string{setecv1alpha1.SnapshotVCPUAnnotation: in}
		if got, ok := snapshotVCPU(snap); ok {
			t.Errorf("%q: got %d, want refused", in, got)
		}
	}
}
