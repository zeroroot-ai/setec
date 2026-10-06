// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package podspec

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
)

// scratchVolume returns the scratch emptyDir of pod.
func scratchVolume(t *testing.T, pod *corev1.Pod) *corev1.EmptyDirVolumeSource {
	t.Helper()
	for _, v := range pod.Spec.Volumes {
		if v.Name == scratchVolumeName {
			return v.EmptyDir
		}
	}
	t.Fatal("the Pod has no scratch volume")
	return nil
}

// TestBuild_DiskLimits covers the scratch size limit and the
// ephemeral-storage limit of ADR-0146 (setec#172).
func TestBuild_DiskLimits(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		own         string // the Sandbox's own scratch, or ""
		opt         string // BuildOptions.Scratch as the controller resolves it, or ""
		wantScratch string
		wantLimit   string
	}{
		{name: "the default", wantScratch: "10Gi", wantLimit: "11Gi"},
		{name: "the Sandbox asks for less", own: "2Gi", wantScratch: "2Gi", wantLimit: "3Gi"},
		{name: "the controller resolved a class value", opt: "20Gi", wantScratch: "20Gi", wantLimit: "21Gi"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sb := newSandbox(func(sb *setecv1alpha1.Sandbox) {
				if tc.own != "" {
					q := resource.MustParse(tc.own)
					sb.Spec.Resources.Scratch = &q
				}
			})
			opts := BuildOptions{}
			if tc.opt != "" {
				opts.Scratch = resource.MustParse(tc.opt)
			}
			pod, err := BuildWithOptions(sb, defaultRuntimeClass, opts)
			if err != nil {
				t.Fatalf("BuildWithOptions: %v", err)
			}
			ed := scratchVolume(t, pod)
			if ed.SizeLimit == nil {
				t.Fatal("the scratch volume has no size limit")
			}
			expectQuantity(t, "scratch size limit", *ed.SizeLimit, tc.wantScratch)
			res := pod.Spec.Containers[0].Resources
			expectQuantity(t, "ephemeral-storage limit", res.Limits[corev1.ResourceEphemeralStorage], tc.wantLimit)
			expectQuantity(t, "ephemeral-storage request", res.Requests[corev1.ResourceEphemeralStorage], "1Gi")
		})
	}
}
