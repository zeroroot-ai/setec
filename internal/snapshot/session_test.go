// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package snapshot

import (
	"bytes"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	setecgrpcv1 "github.com/zeroroot-ai/setec/api/grpc/v1"
	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
)

// Repeated fixture identifier.
const (
	sessCkpt3 = "t-a-sess-ckpt-3"
)

func sessionSandbox() *setecv1alpha1.Sandbox {
	return &setecv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Namespace: "t-a", Name: "sess"},
		Spec: setecv1alpha1.SandboxSpec{
			Image: "ghcr.io/org/app:v1",
			Lifecycle: &setecv1alpha1.Lifecycle{
				Mode: setecv1alpha1.LifecycleModeSession,
			},
		},
		Status: setecv1alpha1.SandboxStatus{
			PodName: "sess-vm",
			Phase:   setecv1alpha1.SandboxPhaseRunning,
		},
	}
}

func TestCheckpointSessionForwardsKEKAndID(t *testing.T) {
	sb := sessionSandbox()
	pod := newPodForSandbox(sb, "node-a")
	na := &fakeNodeAgentClient{
		createResp: &setecgrpcv1.CreateSnapshotResponse{StorageRef: sessCkpt3, SizeBytes: 42},
	}
	coord := newCoord(newFakeClient(t, sb, pod), &fakeDialer{client: na})

	kek := bytes.Repeat([]byte{5}, 32)
	ref, size, err := coord.CheckpointSession(t.Context(), sb, "s3", 3, kek)
	if err != nil {
		t.Fatalf("CheckpointSession: %v", err)
	}
	if ref != sessCkpt3 || size != 42 {
		t.Fatalf("got (%q,%d)", ref, size)
	}
	if na.lastCreate.GetSnapshotId() != sessCkpt3 {
		t.Fatalf("snapshot id = %q", na.lastCreate.GetSnapshotId())
	}
	if na.lastCreate.GetStorageBackend() != "s3" {
		t.Fatalf("backend = %q", na.lastCreate.GetStorageBackend())
	}
	if !bytes.Equal(na.lastCreate.GetSessionKek(), kek) {
		t.Fatal("session KEK not forwarded")
	}
	if na.lastCreate.GetSourceKataSocket() == "" {
		t.Fatal("kata socket not rendered")
	}
}

// TestRestoreSessionCheckpointNoNodePinning proves the session restore
// dials whatever node the Pod landed on — there is no snapshot-node
// equality check, which is exactly what lets a drained session resume
// elsewhere.
func TestRestoreSessionCheckpointNoNodePinning(t *testing.T) {
	sb := sessionSandbox()
	pod := newPodForSandbox(sb, "node-b") // NOT the node that wrote the checkpoint
	na := &fakeNodeAgentClient{
		restoreRes: verifiedRestoreRes(),
	}
	coord := newCoord(newFakeClient(t, sb, pod), &fakeDialer{client: na})

	kek := bytes.Repeat([]byte{6}, 32)
	if err := coord.RestoreSessionCheckpoint(t.Context(), sb, sessCkpt3, "s3", kek); err != nil {
		t.Fatalf("RestoreSessionCheckpoint: %v", err)
	}
	if na.lastRestore.GetStorageRef() != sessCkpt3 ||
		na.lastRestore.GetStorageBackend() != "s3" ||
		!bytes.Equal(na.lastRestore.GetSessionKek(), kek) {
		t.Fatalf("restore request = %+v", na.lastRestore)
	}
}

func TestRestoreSessionCheckpointFailurePropagates(t *testing.T) {
	sb := sessionSandbox()
	pod := newPodForSandbox(sb, "node-b")
	na := &fakeNodeAgentClient{
		restoreRes: &setecgrpcv1.RestoreSandboxResponse{Success: false, Error: "corrupted snapshot"},
	}
	coord := newCoord(newFakeClient(t, sb, pod), &fakeDialer{client: na})
	err := coord.RestoreSessionCheckpoint(t.Context(), sb, "t-a-sess-ckpt-9", "s3", bytes.Repeat([]byte{1}, 32))
	if err == nil {
		t.Fatal("want error from failed restore")
	}
	if errors.Is(err, ErrInvariantGateViolation) {
		t.Fatalf("a node-side restore failure must not be a gate violation: %v", err)
	}
}

// TestDeleteSessionCheckpointFallsBackToAnyNode: with the Pod gone,
// the delete routes through any Ready node advertising a setec
// runtime label.
func TestDeleteSessionCheckpointFallsBackToAnyNode(t *testing.T) {
	sb := sessionSandbox()
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "node-c",
			Labels: map[string]string{"setec.zeroroot.ai/runtime.kata-fc": "true"},
		},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
		},
	}
	na := &fakeNodeAgentClient{deleteRes: &setecgrpcv1.DeleteSnapshotResponse{Success: true}}
	coord := newCoord(newFakeClient(t, sb, node), &fakeDialer{client: na})

	if err := coord.DeleteSessionCheckpoint(t.Context(), sb, sessCkpt3, "s3"); err != nil {
		t.Fatalf("DeleteSessionCheckpoint: %v", err)
	}
}

func TestDeleteSessionCheckpointNoNodesFails(t *testing.T) {
	sb := sessionSandbox()
	coord := newCoord(newFakeClient(t, sb), &fakeDialer{client: &fakeNodeAgentClient{}})
	if err := coord.DeleteSessionCheckpoint(t.Context(), sb, "ref", "s3"); err == nil {
		t.Fatal("want error when no node-agent is reachable")
	}
}
