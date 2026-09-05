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
// Apply writes the probe outcome onto the Node's own metadata: one label per
// backend (which is what the operator's node affinity selects on) and one
// annotation carrying the diagnostic detail. The probe package performs the
// detection; this package owns the Kubernetes write path.
//
// # Why metadata only
//
// Apply used to publish the same detail as a SetecRuntimes condition on
// node.status, which required the agent's ServiceAccount to hold
// nodes/status: patch cluster-wide. That verb is not a reporting
// primitive — it lets its holder rewrite any node's allocatable capacity
// and any node's readiness conditions, on every node in the cluster,
// which is a cluster-wide scheduling and availability primitive rather
// than the "tell the operator what this node can run" the agent needs
// (GHSA-p8f8-3qpw-7h93). No code ever read the condition back, so the
// detail moved to an annotation under the setec.zeroroot.ai/ prefix and
// the grant was dropped. Annotations are inert: nothing schedules,
// evicts or admits on them.
//
// What remains — a metadata write on the agent's own Node — is narrowed
// outside Go, because RBAC cannot express "only these label keys". The
// chart ships a ValidatingAdmissionPolicy that holds the agent's
// ServiceAccount to exactly the keys named here, on the node its token
// says it runs on. See charts/setec/templates/runtime-agent-node-guard.yaml.
package runtimeagent

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/zeroroot-ai/setec/internal/runtimeagent/probe"
)

const (
	// LabelPrefix is the key prefix for all runtime capability labels
	// applied by the Setec node-agent. The operator's node affinity
	// selects on LabelPrefix+<backend>, so these keys are the scheduling
	// contract between agent and operator.
	LabelPrefix = "setec.zeroroot.ai/runtime."

	// ResultAnnotation carries the full probe outcome as JSON:
	// {"<backend>": {"available": bool, "reason": string, "details": {}}}.
	//
	// It replaces the SetecRuntimes node condition. `kubectl get node -o
	// yaml` still shows the same information, and writing it costs only a
	// metadata patch rather than a cluster-wide nodes/status grant.
	ResultAnnotation = "setec.zeroroot.ai/runtime-probe"
)

// labelPrefix is retained as an unexported alias so the many internal
// references read as before.
const labelPrefix = LabelPrefix

// Apply writes the Setec runtime capability labels and the probe-result
// annotation onto a Node.
//
// Label key format: setec.zeroroot.ai/runtime.<backend>
// Label value: "true" when Available, "false" otherwise.
//
// Apply only touches labels whose keys begin with labelPrefix plus the
// single ResultAnnotation key — it never removes or modifies other labels
// or annotations already present on the Node. Within its own prefix it
// reconciles rather than merges: a capability label for a backend this
// cycle did not probe is deleted, not left behind (see pruneStaleLabels).
// That restraint is mirrored
// by the admission policy the chart installs, so a build of this agent
// that stopped honouring it would be rejected by the API server rather
// than trusted.
//
// Apply is idempotent in the strong sense: when the Node already carries
// the desired keys and values it issues no write at all. The previous
// implementation refreshed a condition heartbeat on every cycle, so every
// node produced a write every probe interval whether or not anything had
// changed.
func Apply(
	ctx context.Context,
	c client.Client,
	nodeName string,
	results []probe.CapabilityResult,
) error {
	node := &corev1.Node{}
	if err := c.Get(ctx, types.NamespacedName{Name: nodeName}, node); err != nil {
		return fmt.Errorf("runtimeagent: get Node %q: %w", nodeName, err)
	}

	detail, err := buildResultJSON(results)
	if err != nil {
		return fmt.Errorf("runtimeagent: build probe result annotation: %w", err)
	}

	base := node.DeepCopy()
	if node.Labels == nil {
		node.Labels = make(map[string]string)
	}
	if node.Annotations == nil {
		node.Annotations = make(map[string]string)
	}
	for _, r := range results {
		node.Labels[labelPrefix+r.Backend] = boolLabel(r.Available)
	}
	pruneStaleLabels(node.Labels, results)
	node.Annotations[ResultAnnotation] = detail

	// Nothing to say: skip the write entirely rather than emit a patch the
	// API server would resolve to a no-op. On a large cluster this is the
	// difference between one Node write per node per probe interval and
	// none.
	if maps.Equal(base.Labels, node.Labels) && maps.Equal(base.Annotations, node.Annotations) {
		return nil
	}

	if err := c.Patch(ctx, node, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("runtimeagent: patch Node metadata %q: %w", nodeName, err)
	}
	return nil
}

// pruneStaleLabels deletes every Setec runtime capability label whose backend
// this probe cycle did not report on.
//
// A label the agent stops writing is not neutral — it keeps advertising its
// last value forever, and the operator has no way to tell a stale "true" from
// a fresh one. That happens whenever the set of probed backends shrinks: an
// operator disables a backend in the RuntimeConfig (the agent filters its
// probe list from it), or an upgrade drops one. The node then keeps steering
// Sandboxes onto a capability nothing has confirmed since (setec#243).
//
// Only keys under labelPrefix are touched, which is exactly the set the
// chart's ValidatingAdmissionPolicy permits the agent to add, change or
// remove.
func pruneStaleLabels(labels map[string]string, results []probe.CapabilityResult) {
	reported := make(map[string]struct{}, len(results))
	for _, r := range results {
		reported[r.Backend] = struct{}{}
	}
	for key := range labels {
		if !strings.HasPrefix(key, labelPrefix) {
			continue
		}
		if _, ok := reported[strings.TrimPrefix(key, labelPrefix)]; !ok {
			delete(labels, key)
		}
	}
}

// boolLabel renders a capability result as the label value the operator's
// node affinity matches on.
func boolLabel(available bool) string {
	if available {
		return "true"
	}
	return "false"
}

// buildResultJSON serialises the probe results into the JSON body of the
// ResultAnnotation: a map of backend name to its outcome.
//
// The value is deliberately free of timestamps. A heartbeat field would
// make every write differ from the last, defeating the no-op check in
// Apply and putting a Node update on the API server for every node on
// every probe cycle. The probe's freshness is observable from the agent's
// metrics, which is where a liveness question belongs.
func buildResultJSON(results []probe.CapabilityResult) (string, error) {
	type entry struct {
		Available bool              `json:"available"`
		Reason    string            `json:"reason,omitempty"`
		Details   map[string]string `json:"details,omitempty"`
	}

	m := make(map[string]entry, len(results))
	for _, r := range results {
		m[r.Backend] = entry{
			Available: r.Available,
			Reason:    r.Reason,
			Details:   r.Details,
		}
	}

	// encoding/json sorts map keys, so identical results always produce an
	// identical string and the no-op check in Apply holds.
	msg, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	return string(msg), nil
}

// HasSetecLabel reports whether key is a Setec runtime label key.
// Exported so tests outside the package can validate label filtering.
func HasSetecLabel(key string) bool {
	return strings.HasPrefix(key, labelPrefix)
}
