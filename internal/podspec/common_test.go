// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package podspec

import (
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
)

// newSandbox returns a valid Sandbox for the builder tests, changed by each
// mutator in order.
func newSandbox(mutators ...func(*setecv1alpha1.Sandbox)) *setecv1alpha1.Sandbox {
	sb := &setecv1alpha1.Sandbox{
		APIVersion: setecv1alpha1.GroupVersion.String(),
		Kind:       "Sandbox",
		Name:       "demo",
		Namespace:  "default",
		UID:        types.UID("11111111-2222-3333-4444-555555555555"),
		Spec: setecv1alpha1.SandboxSpec{
			Image:   "docker.io/library/python:3.12-slim",
			Command: []string{"python", "-c", "print('hi')"},
			Resources: setecv1alpha1.Resources{
				VCPU:   2,
				Memory: resource.MustParse("2Gi"),
			},
		},
	}
	for _, m := range mutators {
		m(sb)
	}
	return sb
}
