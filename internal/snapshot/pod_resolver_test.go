// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package snapshot

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// PodResolver is what replaced the unresolvable DNS name at the heart
// of setec#92. Instead of hoping <node>.<service>.<namespace>.svc
// resolves (it never does, see dialer.go), the operator asks the API
// which node-agent Pod is on the node. These tests pin the three ways
// that lookup can go wrong plus the case where it must succeed.

const (
	// testNamespace is the namespace every test's PodResolver watches,
	// unless a test is specifically proving the namespace scope.
	testNamespace = "setec-system"
	// testWorkerNode is the node under test in every case that is not
	// specifically about a different node.
	testWorkerNode = "worker-1"
	// testPodName and testPodIP are the acceptance-case Pod's name and
	// address. Tests that need a different Pod name or IP say so
	// explicitly.
	testPodName = "node-agent-abc"
	testPodIP   = "10.0.0.5"
)

func newPodResolverScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	utilruntime.Must(corev1.AddToScheme(s))
	return s
}

func newFakePodClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(newPodResolverScheme(t)).WithObjects(objs...).Build()
}

func nodeAgentPod(name, namespace, node string, phase corev1.PodPhase, ready corev1.ConditionStatus, ip string) *corev1.Pod {
	return &corev1.Pod{
		Name:      name,
		Namespace: namespace,
		UID:       types.UID(name + "-uid"),
		Labels:    map[string]string{NodeAgentComponentLabel: nodeAgentComponentValue},
		Spec:      corev1.PodSpec{NodeName: node},
		Status: corev1.PodStatus{
			Phase: phase,
			PodIP: ip,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: ready},
			},
		},
	}
}

// TestPodResolver_FindsTheRunningReadyPod is the acceptance case every
// refusal below is measured against.
func TestPodResolver_FindsTheRunningReadyPod(t *testing.T) {
	t.Parallel()
	pod := nodeAgentPod(testPodName, testNamespace, testWorkerNode, corev1.PodRunning, corev1.ConditionTrue, testPodIP)
	r := &PodResolver{
		Client:    newFakePodClient(t, pod),
		Namespace: testNamespace,
	}

	got, err := r.ResolveNodeAgentPod(t.Context(), testWorkerNode)
	if err != nil {
		t.Fatalf("ResolveNodeAgentPod: %v", err)
	}
	if got.Name != testPodName {
		t.Fatalf("resolved pod = %q, want %q", got.Name, testPodName)
	}
	if got.Status.PodIP != testPodIP {
		t.Fatalf("resolved PodIP = %q, want %q", got.Status.PodIP, testPodIP)
	}
}

// TestPodResolver_NoPodOnNode is the direct symptom setec#92 describes:
// no node-agent Pod is scheduled onto the node the operator wants to
// reach (or, as originally happened, one is running but the operator
// never found it because it was looking for a DNS record instead).
func TestPodResolver_NoPodOnNode(t *testing.T) {
	t.Parallel()
	// A node-agent Pod exists, but on a different node.
	pod := nodeAgentPod(testPodName, testNamespace, "worker-2", corev1.PodRunning, corev1.ConditionTrue, testPodIP)
	r := &PodResolver{
		Client:    newFakePodClient(t, pod),
		Namespace: testNamespace,
	}

	_, err := r.ResolveNodeAgentPod(t.Context(), testWorkerNode)
	if err == nil {
		t.Fatal("ResolveNodeAgentPod with no pod on the node: want error, got nil")
	}
	if !strings.Contains(err.Error(), `"`+testWorkerNode+`"`) {
		t.Fatalf("error = %q, want it to name the node", err)
	}
}

// TestPodResolver_RefusesANotReadyPod is what makes this a Ready check
// and not just an existence check: a node-agent Pod that is Pending or
// still starting cannot yet serve a snapshot RPC, and dialing it
// anyway is a slower, more confusing way to fail than refusing here.
func TestPodResolver_RefusesANotReadyPod(t *testing.T) {
	t.Parallel()
	pod := nodeAgentPod(testPodName, testNamespace, testWorkerNode, corev1.PodPending, corev1.ConditionFalse, "")
	r := &PodResolver{
		Client:    newFakePodClient(t, pod),
		Namespace: testNamespace,
	}

	_, err := r.ResolveNodeAgentPod(t.Context(), testWorkerNode)
	if err == nil {
		t.Fatal("ResolveNodeAgentPod against a not-Ready pod: want error, got nil")
	}
}

// TestPodResolver_RefusesARunningButNotReadyPod covers the gap between
// Phase and the Ready condition: a Pod can be Running (its containers
// started) before its readiness probe has passed once.
func TestPodResolver_RefusesARunningButNotReadyPod(t *testing.T) {
	t.Parallel()
	pod := nodeAgentPod(testPodName, testNamespace, testWorkerNode, corev1.PodRunning, corev1.ConditionFalse, testPodIP)
	r := &PodResolver{
		Client:    newFakePodClient(t, pod),
		Namespace: testNamespace,
	}

	_, err := r.ResolveNodeAgentPod(t.Context(), testWorkerNode)
	if err == nil {
		t.Fatal("ResolveNodeAgentPod against a Running-but-not-Ready pod: want error, got nil")
	}
}

// TestPodResolver_FindsTheNewPodAfterARestart is the resolver side of
// the restarted-node-agent case. (The dialer side, which proves the
// connection cache actually drops the old connection, is
// TestGRPCDialer_RedialsAfterNodeAgentRestart in dialer_test.go.) Once
// the old Pod is gone and a new one has replaced it, on the same node
// but with a new UID, the resolver must report the new one. It must
// not report the one it used to know about, and it must not error.
func TestPodResolver_FindsTheNewPodAfterARestart(t *testing.T) {
	t.Parallel()
	oldPod := nodeAgentPod("node-agent-old", testNamespace, testWorkerNode, corev1.PodRunning, corev1.ConditionTrue, testPodIP)
	c := newFakePodClient(t, oldPod)
	r := &PodResolver{Client: c, Namespace: testNamespace}

	before, err := r.ResolveNodeAgentPod(t.Context(), testWorkerNode)
	if err != nil {
		t.Fatalf("ResolveNodeAgentPod before restart: %v", err)
	}
	if before.UID != "node-agent-old-uid" {
		t.Fatalf("resolved UID = %q, want %q", before.UID, "node-agent-old-uid")
	}

	// The DaemonSet controller deletes the old Pod and creates a
	// replacement with a new name, UID and IP. This is exactly what a
	// node-agent restart (crash, eviction, node drain) produces.
	if err := c.Delete(t.Context(), oldPod); err != nil {
		t.Fatalf("delete old pod: %v", err)
	}
	newPod := nodeAgentPod("node-agent-new", testNamespace, testWorkerNode, corev1.PodRunning, corev1.ConditionTrue, "10.0.0.9")
	if err := c.Create(t.Context(), newPod); err != nil {
		t.Fatalf("create new pod: %v", err)
	}

	after, err := r.ResolveNodeAgentPod(t.Context(), testWorkerNode)
	if err != nil {
		t.Fatalf("ResolveNodeAgentPod after restart: %v", err)
	}
	if after.UID != "node-agent-new-uid" {
		t.Fatalf("resolved UID = %q, want the new pod's %q", after.UID, "node-agent-new-uid")
	}
	if after.Status.PodIP != "10.0.0.9" {
		t.Fatalf("resolved PodIP = %q, want the new pod's %q", after.Status.PodIP, "10.0.0.9")
	}
}

// TestPodResolver_IgnoresPodsInOtherNamespaces guards the namespace
// scope: a node-agent-labeled Pod in an unrelated namespace (a
// different setec install sharing the cluster, or a coincidental
// label collision) must never be dialed.
func TestPodResolver_IgnoresPodsInOtherNamespaces(t *testing.T) {
	t.Parallel()
	pod := nodeAgentPod(testPodName, "other-namespace", testWorkerNode, corev1.PodRunning, corev1.ConditionTrue, testPodIP)
	r := &PodResolver{
		Client:    newFakePodClient(t, pod),
		Namespace: testNamespace,
	}

	_, err := r.ResolveNodeAgentPod(t.Context(), testWorkerNode)
	if err == nil {
		t.Fatal("ResolveNodeAgentPod against a pod in another namespace: want error, got nil")
	}
}

// TestPodResolver_RequiresANodeName mirrors the dialer's own guard so
// the same mistake cannot slip in below it.
func TestPodResolver_RequiresANodeName(t *testing.T) {
	t.Parallel()
	r := &PodResolver{Client: newFakePodClient(t), Namespace: testNamespace}
	if _, err := r.ResolveNodeAgentPod(t.Context(), ""); err == nil {
		t.Fatal("ResolveNodeAgentPod with an empty node name: want error, got nil")
	}
}
