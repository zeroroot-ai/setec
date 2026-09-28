// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package snapshot

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	setecgrpcv1 "github.com/zeroroot-ai/setec/api/grpc/v1"
	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
)

func snapshotOn(node, backend string) *setecv1alpha1.Snapshot {
	return &setecv1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Namespace: "t-a", Name: "s"},
		Spec: setecv1alpha1.SnapshotSpec{
			Node: node, StorageBackend: backend, StorageRef: "ref",
		},
	}
}

// TestDeleteSnapshot_LocalDiskOnAVanishedNodeIsDeleted asserts that a
// local-disk snapshot whose Node no longer exists deletes without a
// node-agent. Its state went with the node, and retrying the dial
// forever kept the Snapshot undeletable (setec#19).
func TestDeleteSnapshot_LocalDiskOnAVanishedNodeIsDeleted(t *testing.T) {
	c := newFakeClient(t)
	coord := newCoord(c, &fakeDialer{dialErr: errors.New("dial must not happen")})
	if err := coord.DeleteSnapshot(context.Background(), snapshotOn("gone-node", "local-disk")); err != nil {
		t.Fatalf("DeleteSnapshot on a vanished node = %v, want nil", err)
	}
}

// TestDeleteSnapshot_LocalDiskOnALiveNodeStillDials asserts that an
// existing node keeps the secure-erase path through its node-agent.
func TestDeleteSnapshot_LocalDiskOnALiveNodeStillDials(t *testing.T) {
	c := newFakeClient(t, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}})
	coord := newCoord(c, &fakeDialer{dialErr: errors.New("node-agent down")})
	if err := coord.DeleteSnapshot(context.Background(), snapshotOn("node-1", "local-disk")); err == nil {
		t.Fatal("DeleteSnapshot on a live node with its node-agent down = nil, want the dial error")
	}
}

// TestDeleteSnapshot_S3IsNeverNodeGone asserts that a backend whose
// state is not on the node always goes through a node-agent.
func TestDeleteSnapshot_S3IsNeverNodeGone(t *testing.T) {
	c := newFakeClient(t)
	na := &fakeNodeAgentClient{deleteRes: &setecgrpcv1.DeleteSnapshotResponse{Success: true}}
	dialed := false
	coord := newCoord(c, dialerFunc(func() (NodeAgentClient, error) { dialed = true; return na, nil }))
	if err := coord.DeleteSnapshot(context.Background(), snapshotOn("gone-node", "s3")); err != nil {
		t.Fatalf("DeleteSnapshot s3: %v", err)
	}
	if !dialed {
		t.Fatal("an s3 snapshot on a missing node skipped the node-agent; its state is not node-local")
	}
}

type dialerFunc func() (NodeAgentClient, error)

func (f dialerFunc) Dial(context.Context, string) (NodeAgentClient, error) { return f() }
