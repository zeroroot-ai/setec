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
	corev1 "k8s.io/api/core/v1"
)

// requiredRuntimeNodeAffinity returns a NodeAffinity with a single required
// NodeSelectorTerm containing two MatchExpressions:
//
//  1. label=value with operator In (the backend-specific capability label, e.g.
//     "setec.zeroroot.ai/runtime.kata-fc" = "true").
//  2. "kubernetes.io/os" In ["linux"] — all Setec backends require Linux nodes.
//
// The returned value is always non-nil and is freshly allocated so callers may
// embed it without aliasing concerns.
func requiredRuntimeNodeAffinity(label string) *corev1.NodeAffinity {
	return &corev1.NodeAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
			NodeSelectorTerms: []corev1.NodeSelectorTerm{
				{
					MatchExpressions: []corev1.NodeSelectorRequirement{
						{
							Key:      label,
							Operator: corev1.NodeSelectorOpIn,
							Values:   []string{"true"},
						},
						{
							Key:      "kubernetes.io/os",
							Operator: corev1.NodeSelectorOpIn,
							Values:   []string{"linux"},
						},
					},
				},
			},
		},
	}
}

// runtimeAffinityLabel returns the standard Setec node-capability label for a
// given backend name, e.g. "setec.zeroroot.ai/runtime.kata-fc".
func runtimeAffinityLabel(backend string) string {
	return "setec.zeroroot.ai/runtime." + backend
}
