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

// Package runtimeagent provides the node-local runtime capability detection
// and node labelling logic for the Setec node-agent DaemonSet.
//
// Apply patches Node objects with per-backend labels and a SetecRuntimes
// status condition. The probe package performs the detection; this package
// owns the Kubernetes write path.
package runtimeagent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/zeroroot-ai/setec/internal/runtimeagent/probe"
)

const (
	// labelPrefix is the key prefix for all runtime capability labels
	// applied by the Setec node-agent.
	labelPrefix = "setec.zeroroot.ai/runtime."

	// conditionType is the Kubernetes Node condition type set by Apply.
	conditionType = "SetecRuntimes"
)

// Apply patches a Node with Setec runtime capability labels and a SetecRuntimes
// status condition derived from results.
//
// Label key format: setec.zeroroot.ai/runtime.<backend>
// Label value: "true" when Available, "false" otherwise.
//
// Apply only touches labels whose keys begin with labelPrefix — it never
// removes or modifies other labels already present on the Node. The
// SetecRuntimes condition message is a JSON object mapping backend name to
// its CapabilityResult (available, reason, details).
//
// LastTransitionTime on the condition is updated only when the condition
// Status (True/False) actually changes, making repeated calls with identical
// input idempotent.
func Apply(
	ctx context.Context,
	c client.Client,
	nodeName string,
	results []probe.CapabilityResult,
) error {
	// 1. Fetch the current Node.
	node := &corev1.Node{}
	if err := c.Get(ctx, types.NamespacedName{Name: nodeName}, node); err != nil {
		return fmt.Errorf("runtimeagent: get Node %q: %w", nodeName, err)
	}

	// 2. Compute desired labels, preserving all non-setec labels.
	// Save a copy before mutation so MergeFrom can compute the label patch.
	labelBase := node.DeepCopy()
	if node.Labels == nil {
		node.Labels = make(map[string]string)
	}
	for _, r := range results {
		key := labelPrefix + r.Backend
		val := "false"
		if r.Available {
			val = "true"
		}
		node.Labels[key] = val
	}

	// 3. Patch metadata (labels). Node labels live on the object metadata.
	if err := c.Patch(ctx, node, client.MergeFrom(labelBase)); err != nil {
		return fmt.Errorf("runtimeagent: patch Node labels %q: %w", nodeName, err)
	}

	// 4. Build the JSON condition message: map[backend]CapabilityResult.
	conditionStatus, conditionMsg, err := buildCondition(results)
	if err != nil {
		return fmt.Errorf("runtimeagent: build condition message: %w", err)
	}

	// 5. Patch status (conditions). Status is a sub-resource on Node.
	// Re-fetch the node after the metadata patch so the status base
	// reflects the current server state (ResourceVersion may have advanced).
	statusBase := &corev1.Node{}
	if err := c.Get(ctx, types.NamespacedName{Name: nodeName}, statusBase); err != nil {
		return fmt.Errorf("runtimeagent: re-fetch Node for status patch %q: %w", nodeName, err)
	}
	now := metav1.NewTime(time.Now().UTC().Truncate(time.Second))
	statusNode := statusBase.DeepCopy()
	statusNode.Status.Conditions = upsertCondition(statusNode.Status.Conditions, corev1.NodeCondition{
		Type:               conditionType,
		Status:             conditionStatus,
		Reason:             "RuntimeProbe",
		Message:            conditionMsg,
		LastTransitionTime: now,
		LastHeartbeatTime:  now,
	})
	if err := c.Status().Patch(ctx, statusNode, client.MergeFrom(statusBase)); err != nil {
		return fmt.Errorf("runtimeagent: patch Node status %q: %w", nodeName, err)
	}

	return nil
}

// buildCondition constructs the condition Status and JSON message body for the
// SetecRuntimes condition. Status is True when all backends are available, False
// otherwise.
func buildCondition(results []probe.CapabilityResult) (corev1.ConditionStatus, string, error) {
	// Build a map for JSON serialisation. We need ordered, reproducible JSON
	// so tests can assert on content; use a dedicated struct per backend.
	type entry struct {
		Available bool              `json:"available"`
		Reason    string            `json:"reason,omitempty"`
		Details   map[string]string `json:"details,omitempty"`
	}

	allAvailable := true
	m := make(map[string]entry, len(results))
	for _, r := range results {
		if !r.Available {
			allAvailable = false
		}
		m[r.Backend] = entry{
			Available: r.Available,
			Reason:    r.Reason,
			Details:   r.Details,
		}
	}

	msg, err := json.Marshal(m)
	if err != nil {
		return "", "", err
	}

	status := corev1.ConditionFalse
	if allAvailable {
		status = corev1.ConditionTrue
	}
	return status, string(msg), nil
}

// upsertCondition inserts or updates the condition in the slice. When an
// existing condition with the same Type is found:
//   - LastTransitionTime is preserved when Status has NOT changed (idempotent).
//   - LastTransitionTime is updated to the incoming value when Status flips.
//   - LastHeartbeatTime is always updated to the incoming value.
func upsertCondition(conditions []corev1.NodeCondition, desired corev1.NodeCondition) []corev1.NodeCondition {
	for i, c := range conditions {
		if c.Type != desired.Type {
			continue
		}
		// Preserve transition time when status is unchanged.
		if c.Status == desired.Status {
			desired.LastTransitionTime = c.LastTransitionTime
		}
		conditions[i] = desired
		return conditions
	}
	// Not found — append.
	return append(conditions, desired)
}

// HasSetecLabel reports whether key is a Setec runtime label key.
// Exported so tests outside the package can validate label filtering.
func HasSetecLabel(key string) bool {
	return strings.HasPrefix(key, labelPrefix)
}
