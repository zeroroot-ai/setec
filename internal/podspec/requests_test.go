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

package podspec

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
)

func quantityPtr(s string) *resource.Quantity {
	q := resource.MustParse(s)
	return &q
}

// buildWithRequests builds the demo Sandbox at 2 vCPU / 4Gi under the
// given class reservation and returns the workload container's
// resources.
func buildWithRequests(t *testing.T, req *setecv1alpha1.ResourceRequests) corev1.ResourceRequirements {
	t.Helper()
	sb := newSandbox(func(sb *setecv1alpha1.Sandbox) {
		sb.Spec.Resources.VCPU = 2
		sb.Spec.Resources.Memory = resource.MustParse("4Gi")
	})
	pod, err := BuildWithOptions(sb, defaultRuntimeClass, BuildOptions{Requests: req})
	if err != nil {
		t.Fatalf("BuildWithOptions: %v", err)
	}
	return pod.Spec.Containers[0].Resources
}

func expectQuantity(t *testing.T, what string, got resource.Quantity, want string) {
	t.Helper()
	if got.Cmp(resource.MustParse(want)) != 0 {
		t.Errorf("%s = %s, want %s", what, got.String(), want)
	}
}

// TestBuild_ClassRequests_SplitFromLimits proves the always-on member
// shape (setec#372): the class reservation lands on the Pod's requests
// while the Sandbox's own budget stays the limits, so an idle member
// reserves 250m / 768Mi and can burst to 2 vCPU / 4Gi.
func TestBuild_ClassRequests_SplitFromLimits(t *testing.T) {
	t.Parallel()
	res := buildWithRequests(t, &setecv1alpha1.ResourceRequests{
		CPU:    quantityPtr("250m"),
		Memory: quantityPtr("768Mi"),
	})
	expectQuantity(t, "request cpu", res.Requests[corev1.ResourceCPU], "250m")
	expectQuantity(t, "request memory", res.Requests[corev1.ResourceMemory], "768Mi")
	expectQuantity(t, "limit cpu", res.Limits[corev1.ResourceCPU], "2")
	expectQuantity(t, "limit memory", res.Limits[corev1.ResourceMemory], "4Gi")
}

// TestBuild_ClassRequests_NilKeepsGuaranteed pins the default: with no
// class reservation the requests equal the limits exactly.
func TestBuild_ClassRequests_NilKeepsGuaranteed(t *testing.T) {
	t.Parallel()
	res := buildWithRequests(t, nil)
	if diff := cmp.Diff(res.Limits, res.Requests); diff != "" {
		t.Errorf("requests != limits with no class reservation (-lim +req):\n%s", diff)
	}
}

// TestBuild_ClassRequests_PartialLeavesOtherEqual proves a reservation
// that names one resource leaves the other at its limit.
func TestBuild_ClassRequests_PartialLeavesOtherEqual(t *testing.T) {
	t.Parallel()
	res := buildWithRequests(t, &setecv1alpha1.ResourceRequests{CPU: quantityPtr("500m")})
	expectQuantity(t, "request cpu", res.Requests[corev1.ResourceCPU], "500m")
	expectQuantity(t, "request memory", res.Requests[corev1.ResourceMemory], "4Gi")
}

// TestBuild_ClassRequests_BoundedByLimits proves a reservation above
// the Sandbox's budget is bounded to the budget, never written above
// it: a request above its limit is an invalid Pod, and the class must
// not be able to make a small Sandbox unschedulable.
func TestBuild_ClassRequests_BoundedByLimits(t *testing.T) {
	t.Parallel()
	res := buildWithRequests(t, &setecv1alpha1.ResourceRequests{
		CPU:    quantityPtr("8"),
		Memory: quantityPtr("16Gi"),
	})
	if diff := cmp.Diff(res.Limits, res.Requests); diff != "" {
		t.Errorf("an over-budget reservation must be bounded to the limits (-lim +req):\n%s", diff)
	}
}
