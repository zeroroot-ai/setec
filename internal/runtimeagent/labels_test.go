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
	"testing"
	"time"

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

// findCondition returns the first condition with the given type, or nil.
func findCondition(conditions []corev1.NodeCondition) *corev1.NodeCondition {
	for i := range conditions {
		if conditions[i].Type == conditionType {
			return &conditions[i]
		}
	}
	return nil
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

// TestApply_FirstApply verifies that the first Apply call sets all four backend
// labels and adds the SetecRuntimes condition.
func TestApply_FirstApply(t *testing.T) {
	t.Parallel()

	c := newFakeClient(t, newNode(nil))
	results := allFourResults(true, true, true)

	if err := Apply(context.Background(), c, testNodeName, results); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	node := getNode(t, c)

	// All four backend labels must be present.
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

	// SetecRuntimes condition must be present.
	cond := findCondition(node.Status.Conditions)
	if cond == nil {
		t.Fatalf("SetecRuntimes condition not found on Node")
	}
	if cond.Status != corev1.ConditionTrue {
		t.Errorf("condition Status = %q, want True (all available)", cond.Status)
	}

	// Message must be valid JSON with all four backends.
	var msg map[string]json.RawMessage
	if err := json.Unmarshal([]byte(cond.Message), &msg); err != nil {
		t.Fatalf("condition message is not valid JSON: %v", err)
	}
	for _, r := range results {
		if _, ok := msg[r.Backend]; !ok {
			t.Errorf("condition message missing backend %q", r.Backend)
		}
	}
}

// TestApply_Idempotent verifies that a second Apply call with identical input
// does not change LastTransitionTime on the condition.
func TestApply_Idempotent(t *testing.T) {
	t.Parallel()

	c := newFakeClient(t, newNode(nil))
	results := allFourResults(true, true, false)

	if err := Apply(context.Background(), c, testNodeName, results); err != nil {
		t.Fatalf("first Apply: %v", err)
	}

	node := getNode(t, c)
	cond := findCondition(node.Status.Conditions)
	if cond == nil {
		t.Fatalf("SetecRuntimes condition missing after first Apply")
	}
	firstTransition := cond.LastTransitionTime.Time

	// Sleep briefly so that a naive implementation would produce a different
	// timestamp on the second call.
	time.Sleep(10 * time.Millisecond)

	if err := Apply(context.Background(), c, testNodeName, results); err != nil {
		t.Fatalf("second Apply: %v", err)
	}

	node = getNode(t, c)
	cond = findCondition(node.Status.Conditions)
	if cond == nil {
		t.Fatalf("SetecRuntimes condition missing after second Apply")
	}

	// LastTransitionTime must be frozen — same Status, no transition.
	if !cond.LastTransitionTime.Time.Equal(firstTransition) {
		t.Errorf("LastTransitionTime changed on no-op Apply: first=%v second=%v",
			firstTransition, cond.LastTransitionTime.Time)
	}
}

// TestApply_Transition verifies that a status flip updates the appropriate
// label and advances LastTransitionTime.
func TestApply_Transition(t *testing.T) {
	t.Parallel()

	c := newFakeClient(t, newNode(nil))

	// First apply: gvisor unavailable.
	results := allFourResults(true, true, false)
	if err := Apply(context.Background(), c, testNodeName, results); err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	node := getNode(t, c)
	firstCond := findCondition(node.Status.Conditions)
	if firstCond == nil {
		t.Fatalf("condition missing after first Apply")
	}
	firstTransition := firstCond.LastTransitionTime.Time

	if node.Labels[labelPrefix+"gvisor"] != testLabelFalse {
		t.Errorf("gvisor label should be false initially, got %q", node.Labels[labelPrefix+"gvisor"])
	}

	// Ensure time advances by at least one second (condition timestamps
	// are second-granular).
	time.Sleep(1100 * time.Millisecond)

	// Second apply: gvisor now available → status flips True→False→True path.
	results2 := allFourResults(true, true, true)
	if err := Apply(context.Background(), c, testNodeName, results2); err != nil {
		t.Fatalf("second Apply: %v", err)
	}

	node = getNode(t, c)
	secondCond := findCondition(node.Status.Conditions)
	if secondCond == nil {
		t.Fatalf("condition missing after second Apply")
	}

	if node.Labels[labelPrefix+"gvisor"] != testLabelTrue {
		t.Errorf("gvisor label should be true after transition, got %q", node.Labels[labelPrefix+"gvisor"])
	}

	// The overall condition status changed (False→True), so transition time must advance.
	if !secondCond.LastTransitionTime.After(firstTransition) {
		t.Errorf("LastTransitionTime did not advance on status flip: first=%v second=%v",
			firstTransition, secondCond.LastTransitionTime.Time)
	}
}

// TestApply_PreservesUnrelatedLabels verifies that Apply does not remove
// labels that do not share the setec.zeroroot.ai/runtime. prefix.
func TestApply_PreservesUnrelatedLabels(t *testing.T) {
	t.Parallel()

	extraLabels := map[string]string{
		"example.com/foo":        "bar",
		"kubernetes.io/hostname": "test-node-0",
	}
	c := newFakeClient(t, newNode(extraLabels))
	results := allFourResults(false, false, false)

	if err := Apply(context.Background(), c, testNodeName, results); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	node := getNode(t, c)

	for k, want := range extraLabels {
		if got, ok := node.Labels[k]; !ok || got != want {
			t.Errorf("label %q = %q, want %q (should be preserved)", k, got, want)
		}
	}
}
