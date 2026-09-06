// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package runtime

import (
	corev1 "k8s.io/api/core/v1"
)

// requiredRuntimeNodeAffinity returns a NodeAffinity with a single required
// NodeSelectorTerm containing three MatchExpressions:
//
//  1. label=value with operator In (the backend-specific capability label, e.g.
//     "setec.zeroroot.ai/runtime.kata-fc" = "true").
//  2. "kubernetes.io/os" In ["linux"] — all Setec backends require Linux nodes.
//  3. "kubernetes.io/arch" In ["amd64"] — the sandbox substrate is x86 only
//     (ADR-0001): every published image is linux/amd64 single-arch, so a
//     Sandbox Pod must never land on an arm64 node.
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
						{
							Key:      "kubernetes.io/arch",
							Operator: corev1.NodeSelectorOpIn,
							Values:   []string{"amd64"},
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
