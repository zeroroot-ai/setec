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

package runtimeagent

import (
	"context"
	"encoding/json"
	"maps"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/zeroroot-ai/setec/internal/runtimeagent/probe"
)

const (
	testNodeName   = "test-node-0"
	testLabelTrue  = "true"
	testLabelFalse = "false"
)

// newScheme returns a Scheme with corev1 registered, which is all the fake
// client needs for Node operations.
func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("add corev1 to scheme: %v", err)
	}
	return s
}

// newNode returns a bare Node with the given extra labels pre-populated.
func newNode(extraLabels map[string]string) *corev1.Node {
	labels := make(map[string]string, len(extraLabels))
	maps.Copy(labels, extraLabels)
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   testNodeName,
			Labels: labels,
		},
	}
}

// newFakeClient builds a fake controller-runtime client pre-seeded with
// objects. Node is listed as a status sub-resource so Status().Patch works.
func newFakeClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(newScheme(t)).
		WithObjects(objs...).
		WithStatusSubresource(&corev1.Node{}).
		Build()
}

// getNode fetches the named Node from the fake client.
func getNode(t *testing.T, c client.Client) *corev1.Node {
	t.Helper()
	n := &corev1.Node{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: testNodeName}, n); err != nil {
		t.Fatalf("get Node: %v", err)
	}
	return n
}

// allFourResults returns a canonical set of four CapabilityResults for testing.
func allFourResults(kataFCAvail, kataQEMUAvail, gvisorAvail bool) []probe.CapabilityResult {
	return []probe.CapabilityResult{
		{Backend: "kata-fc", Available: kataFCAvail},
		{Backend: "kata-qemu", Available: kataQEMUAvail},
		{Backend: "gvisor", Available: gvisorAvail},
		{Backend: "runc", Available: true},
	}
}

// probeEntry mirrors the JSON shape Apply writes into ResultAnnotation.
type probeEntry struct {
	Available bool              `json:"available"`
	Reason    string            `json:"reason,omitempty"`
	Details   map[string]string `json:"details,omitempty"`
}

// decodeAnnotation parses the probe-result annotation off a Node, failing
// the test when it is absent or not the documented JSON shape. The
// annotation is the only place the probe detail is published now that the
// SetecRuntimes node condition is gone, so its shape is a contract.
func decodeAnnotation(t *testing.T, node *corev1.Node) map[string]probeEntry {
	t.Helper()
	raw, ok := node.Annotations[ResultAnnotation]
	if !ok {
		t.Fatalf("annotation %q not found on Node; annotations = %v", ResultAnnotation, node.Annotations)
	}
	var out map[string]probeEntry
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("annotation %q is not valid JSON (%v): %s", ResultAnnotation, err, raw)
	}
	return out
}

// TestApply_FirstApply verifies that the first Apply sets one label per
// backend and publishes the full probe detail on the annotation.
func TestApply_FirstApply(t *testing.T) {
	t.Parallel()

	c := newFakeClient(t, newNode(nil))
	results := allFourResults(true, true, true)

	if err := Apply(context.Background(), c, testNodeName, results); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	node := getNode(t, c)

	for _, r := range results {
		key := labelPrefix + r.Backend
		val, ok := node.Labels[key]
		if !ok {
			t.Errorf("label %q missing", key)
			continue
		}
		want := testLabelFalse
		if r.Available {
			want = testLabelTrue
		}
		if val != want {
			t.Errorf("label %q = %q, want %q", key, val, want)
		}
	}

	decoded := decodeAnnotation(t, node)
	for _, r := range results {
		entry, ok := decoded[r.Backend]
		if !ok {
			t.Errorf("annotation missing backend %q", r.Backend)
			continue
		}
		if entry.Available != r.Available {
			t.Errorf("annotation backend %q available = %v, want %v", r.Backend, entry.Available, r.Available)
		}
	}
}

// TestApply_NoStatusWrite is the regression test for GHSA-p8f8-3qpw-7h93.
//
// The agent's ServiceAccount no longer holds nodes/status: patch, so an
// Apply that still tried to write node.status would fail against a real
// API server while passing silently against a fake client that ignores
// RBAC. Asserting that status stays empty is what catches a reintroduced
// status write here rather than in a cluster.
func TestApply_NoStatusWrite(t *testing.T) {
	t.Parallel()

	c := newFakeClient(t, newNode(nil))

	if err := Apply(context.Background(), c, testNodeName, allFourResults(true, false, true)); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	node := getNode(t, c)
	if len(node.Status.Conditions) != 0 {
		t.Errorf("Apply wrote %d node status condition(s); it must touch metadata only: %+v",
			len(node.Status.Conditions), node.Status.Conditions)
	}
}

// TestApply_TouchesOnlySetecKeys pins the second half of the containment
// story. The admission policy the chart installs permits the agent to
// change setec.zeroroot.ai/runtime.* labels and the probe annotation and
// nothing else; an agent that wrote outside that set would be rejected by
// the API server. This asserts the agent stays inside it.
func TestApply_TouchesOnlySetecKeys(t *testing.T) {
	t.Parallel()

	node := newNode(map[string]string{
		"example.com/foo":        "bar",
		"kubernetes.io/hostname": testNodeName,
	})
	node.Annotations = map[string]string{
		"example.com/owner": "platform-team",
	}
	c := newFakeClient(t, node)

	if err := Apply(context.Background(), c, testNodeName, allFourResults(false, false, false)); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	got := getNode(t, c)

	for k, want := range map[string]string{
		"example.com/foo":        "bar",
		"kubernetes.io/hostname": testNodeName,
	} {
		if v, ok := got.Labels[k]; !ok || v != want {
			t.Errorf("label %q = %q (present=%v), want %q preserved", k, v, ok, want)
		}
	}
	if v, ok := got.Annotations["example.com/owner"]; !ok || v != "platform-team" {
		t.Errorf("annotation example.com/owner = %q (present=%v), want preserved", v, ok)
	}

	for k := range got.Labels {
		if strings.HasPrefix(k, labelPrefix) {
			continue
		}
		if k == "example.com/foo" || k == "kubernetes.io/hostname" {
			continue
		}
		t.Errorf("Apply introduced unexpected label %q", k)
	}
	for k := range got.Annotations {
		if k == ResultAnnotation || k == "example.com/owner" {
			continue
		}
		t.Errorf("Apply introduced unexpected annotation %q", k)
	}
}

// TestApply_UnchangedResultsIssueNoWrite verifies the strong form of
// idempotence: a repeat Apply with identical probe results must not touch
// the object at all.
//
// The old implementation refreshed a condition heartbeat every cycle, so
// every node in the cluster produced a Node write every probe interval
// whether or not anything had changed. ResourceVersion is the observable
// that distinguishes "wrote the same thing again" from "did not write".
func TestApply_UnchangedResultsIssueNoWrite(t *testing.T) {
	t.Parallel()

	c := newFakeClient(t, newNode(nil))
	results := allFourResults(true, true, false)

	if err := Apply(context.Background(), c, testNodeName, results); err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	afterFirst := getNode(t, c).ResourceVersion

	if err := Apply(context.Background(), c, testNodeName, results); err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	afterSecond := getNode(t, c).ResourceVersion

	if afterFirst != afterSecond {
		t.Errorf("no-op Apply wrote to the Node: resourceVersion %q -> %q", afterFirst, afterSecond)
	}
}

// TestApply_Transition verifies that a changed probe outcome updates both
// the backend label and the annotation detail.
func TestApply_Transition(t *testing.T) {
	t.Parallel()

	c := newFakeClient(t, newNode(nil))

	if err := Apply(context.Background(), c, testNodeName, allFourResults(true, true, false)); err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	node := getNode(t, c)
	if node.Labels[labelPrefix+"gvisor"] != testLabelFalse {
		t.Errorf("gvisor label should be false initially, got %q", node.Labels[labelPrefix+"gvisor"])
	}
	if decodeAnnotation(t, node)["gvisor"].Available {
		t.Error("annotation should report gvisor unavailable initially")
	}

	if err := Apply(context.Background(), c, testNodeName, allFourResults(true, true, true)); err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	node = getNode(t, c)
	if node.Labels[labelPrefix+"gvisor"] != testLabelTrue {
		t.Errorf("gvisor label should be true after transition, got %q", node.Labels[labelPrefix+"gvisor"])
	}
	if !decodeAnnotation(t, node)["gvisor"].Available {
		t.Error("annotation should report gvisor available after transition")
	}
}

// TestBuildResultJSON_Deterministic guards the no-op check in Apply: if
// the serialisation were not stable for identical input, every probe cycle
// would look like a change and write to every Node in the cluster.
func TestBuildResultJSON_Deterministic(t *testing.T) {
	t.Parallel()

	results := allFourResults(true, false, true)
	first, err := buildResultJSON(results)
	if err != nil {
		t.Fatalf("buildResultJSON: %v", err)
	}
	for i := range 8 {
		again, err := buildResultJSON(results)
		if err != nil {
			t.Fatalf("buildResultJSON (iteration %d): %v", i, err)
		}
		if again != first {
			t.Fatalf("buildResultJSON is not deterministic:\n  %s\n  %s", first, again)
		}
	}
}

// TestApply_PrunesStaleCapabilityLabels is the setec#243 regression test for
// the other half of an optimistic label: one the agent stops writing.
//
// A backend disabled in the RuntimeConfig drops out of the probe list, so
// nothing overwrites its label — the Node would keep advertising the last
// value it ever had, and the operator would keep steering Sandboxes at a
// capability no probe has confirmed since. Apply must delete it.
func TestApply_PrunesStaleCapabilityLabels(t *testing.T) {
	t.Parallel()

	node := newNode(map[string]string{
		labelPrefix + "kata-fc":   testLabelTrue,
		labelPrefix + "kata-qemu": testLabelTrue,
		"example.com/foo":         "bar",
	})
	c := newFakeClient(t, node)

	// This cycle probes only kata-qemu (kata-fc has been disabled).
	results := []probe.CapabilityResult{{Backend: "kata-qemu", Available: true}}
	if err := Apply(context.Background(), c, testNodeName, results); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	got := getNode(t, c)
	if _, ok := got.Labels[labelPrefix+"kata-fc"]; ok {
		t.Errorf("stale label %q survived; labels = %v", labelPrefix+"kata-fc", got.Labels)
	}
	if got.Labels[labelPrefix+"kata-qemu"] != testLabelTrue {
		t.Errorf("probed label %q = %q, want %q",
			labelPrefix+"kata-qemu", got.Labels[labelPrefix+"kata-qemu"], testLabelTrue)
	}
	if got.Labels["example.com/foo"] != "bar" {
		t.Errorf("Apply removed a label outside its prefix; labels = %v", got.Labels)
	}
}

// TestApply_UnavailableBackendLabelIsFalse pins the value the operator's node
// affinity keys on: selectRuntime counts a node as capable only when the label
// reads exactly "true", so an unavailable backend must publish "false" rather
// than any other value.
func TestApply_UnavailableBackendLabelIsFalse(t *testing.T) {
	t.Parallel()

	c := newFakeClient(t, newNode(map[string]string{labelPrefix + "kata-qemu": testLabelTrue}))

	results := []probe.CapabilityResult{{
		Backend:   "kata-qemu",
		Available: false,
		Reason:    `containerd has no runtime handler "kata-qemu" configured`,
	}}
	if err := Apply(context.Background(), c, testNodeName, results); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	got := getNode(t, c)
	if got.Labels[labelPrefix+"kata-qemu"] != testLabelFalse {
		t.Errorf("label %q = %q, want %q",
			labelPrefix+"kata-qemu", got.Labels[labelPrefix+"kata-qemu"], testLabelFalse)
	}
}
