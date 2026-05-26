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

// Package prereq contains the cluster-prerequisite checker Setec runs at
// startup and again during each reconciliation cycle. The checker is
// deliberately narrow: it answers two questions ("is the Kata RuntimeClass
// installed?" and "does at least one Node advertise the Kata-capable label?")
// and returns a structured result the caller may log as warnings, surface on
// /readyz, or translate into Kubernetes Events.
//
// The package performs read-only API calls via a controller-runtime client.
// NotFound responses are treated as informational — Setec never panics when a
// prerequisite is missing, because a misconfigured cluster should yield clear
// remediation guidance, not a crash loop.
package prereq

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// CheckResult is the structured output of Check. Callers inspect the boolean
// fields to drive readiness responses and the Warnings slice to produce
// human-readable log lines or Kubernetes Events.
//
// Warnings are vendor-neutral and point at the project's own documentation
// rather than at any specific cloud or distribution guide. Each missing
// prerequisite contributes exactly one Warning entry so callers can count
// them without parsing the message text.
type CheckResult struct {
	// RuntimeClassPresent reports whether the named RuntimeClass exists in
	// the cluster. False means either NotFound or the API server did not
	// surface the object for any other non-error reason.
	RuntimeClassPresent bool

	// KataCapableNodes reports whether at least one Node carries the
	// caller-supplied label key. The check is presence-only: any value
	// satisfies it, which matches how kata-deploy typically labels nodes.
	KataCapableNodes bool

	// Warnings holds one human-readable string per missing prerequisite,
	// in a stable order (RuntimeClass first, Nodes second). Empty when the
	// cluster satisfies all checks.
	Warnings []string
}

// Check verifies the cluster-level prerequisites Setec needs to function.
//
// It performs two read-only API calls via c:
//  1. Get on a nodev1.RuntimeClass named runtimeClassName. NotFound sets
//     RuntimeClassPresent=false and appends a vendor-neutral warning; other
//     errors are returned to the caller.
//  2. List on corev1.Node filtered by the presence of the nodeLabel key
//     (any value satisfies the check). Zero matches sets KataCapableNodes=
//     false and appends a vendor-neutral warning; other errors are returned.
//
// The nodeLabel argument is supplied by the caller rather than hard-coded so
// operators can rename the node label without a code change. Callers should
// pass the default "katacontainers.io/kata-runtime" when they have no reason
// to override it.
//
// Check never panics on NotFound. A fully misconfigured cluster returns a
// CheckResult with both boolean fields false, two Warnings, and a nil error.
func Check(
	ctx context.Context,
	c client.Client,
	runtimeClassName string,
	nodeLabel string,
) (CheckResult, error) {
	result := CheckResult{}

	rc := &nodev1.RuntimeClass{}
	err := c.Get(ctx, types.NamespacedName{Name: runtimeClassName}, rc)
	switch {
	case err == nil:
		result.RuntimeClassPresent = true
	case apierrors.IsNotFound(err):
		result.RuntimeClassPresent = false
		result.Warnings = append(result.Warnings, fmt.Sprintf(
			"RuntimeClass %q not found; install Kata Containers via kata-deploy — see project documentation",
			runtimeClassName,
		))
	default:
		return result, fmt.Errorf("prereq: get RuntimeClass %q: %w", runtimeClassName, err)
	}

	nodes := &corev1.NodeList{}
	if err := c.List(ctx, nodes, client.HasLabels{nodeLabel}); err != nil {
		return result, fmt.Errorf("prereq: list Nodes with label %q: %w", nodeLabel, err)
	}
	if len(nodes.Items) > 0 {
		result.KataCapableNodes = true
	} else {
		result.KataCapableNodes = false
		result.Warnings = append(result.Warnings, fmt.Sprintf(
			"no Nodes carry the %q label; label at least one KVM-capable Node to schedule Sandbox Pods — see project documentation",
			nodeLabel,
		))
	}

	return result, nil
}

// CheckMulti verifies cluster-level prerequisites for every enabled backend.
//
// For each backend in enabledBackends it performs:
//  1. Get on a nodev1.RuntimeClass named classNames[backend]. NotFound appends
//     a vendor-neutral warning; other errors are returned.
//  2. List on corev1.Node filtered by the label
//     "setec.zeroroot.ai/runtime.<backend>=true". Zero matches appends a
//     warning; other errors are returned.
//
// CheckResult.RuntimeClassPresent is true only when ALL enabled backends have
// a RuntimeClass. CheckResult.KataCapableNodes is true when at least one Node
// carries ANY enabled-backend label (a cluster-wide capability check).
//
// nodeLabel is the legacy single-backend label still used for the
// KataCapableNodes flag in the returned result so /readyz backward compatibility
// is preserved when only kata-fc is enabled.
//
// Disabled backends are silently skipped; they do not produce warnings.
// CheckMulti never panics on NotFound.
func CheckMulti(
	ctx context.Context,
	c client.Client,
	enabledBackends []string,
	classNames map[string]string, // backend → RuntimeClass name
	nodeLabel string,
) (CheckResult, error) {
	// When only kata-fc is enabled, delegate to the original Check so
	// the warning text remains identical (smoke-test compatibility per
	// task-11 requirements).
	if len(enabledBackends) == 1 && enabledBackends[0] == "kata-fc" {
		return Check(ctx, c, classNames["kata-fc"], nodeLabel)
	}

	result := CheckResult{}
	allPresent := true

	for _, backend := range enabledBackends {
		rcName, ok := classNames[backend]
		if !ok || rcName == "" {
			// No RuntimeClass name configured for this backend; treat as missing.
			allPresent = false
			result.Warnings = append(result.Warnings, fmt.Sprintf(
				"backend %q is enabled but has no runtimeClassName configured — enable it via Helm runtimes.%s.runtimeClassName",
				backend, backend,
			))
			continue
		}

		rc := &nodev1.RuntimeClass{}
		err := c.Get(ctx, types.NamespacedName{Name: rcName}, rc)
		switch {
		case err == nil:
			// RuntimeClass present for this backend.
		case apierrors.IsNotFound(err):
			allPresent = false
			result.Warnings = append(result.Warnings, fmt.Sprintf(
				"RuntimeClass %q (backend=%s) not found; install the runtime and register the RuntimeClass — see project documentation",
				rcName, backend,
			))
		default:
			return result, fmt.Errorf("prereq: get RuntimeClass %q (backend=%s): %w", rcName, backend, err)
		}

		// Check that at least one Node advertises this backend.
		backendLabel := "setec.zeroroot.ai/runtime." + backend
		nodes := &corev1.NodeList{}
		if err := c.List(ctx, nodes, client.MatchingLabels{backendLabel: "true"}); err != nil {
			return result, fmt.Errorf("prereq: list Nodes with label %q: %w", backendLabel, err)
		}
		if len(nodes.Items) > 0 {
			result.KataCapableNodes = true
		} else {
			result.Warnings = append(result.Warnings, fmt.Sprintf(
				"no Nodes carry the %q label; label at least one Node capable of backend %q — see project documentation",
				backendLabel, backend,
			))
		}
	}

	result.RuntimeClassPresent = allPresent
	return result, nil
}
