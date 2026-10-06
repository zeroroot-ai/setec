// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package snapshot

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	setecgrpcv1 "github.com/zeroroot-ai/setec/api/grpc/v1"
	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
)

// readyParent is a Ready full Snapshot of Sandbox s from the Pod with UID
// podUID.
func readyParent(podUID string) *setecv1alpha1.Snapshot {
	p := &setecv1alpha1.Snapshot{Namespace: "t-a", Name: "base"}
	p.Annotations = map[string]string{setecv1alpha1.SnapshotSourcePodUIDAnnotation: podUID}
	p.Spec = setecv1alpha1.SnapshotSpec{
		SourceSandbox: "s", SandboxClass: "standard", StorageBackend: "local-disk", StorageRef: "t-a-base", Node: "node-a",
	}
	p.Status.Phase = setecv1alpha1.SnapshotPhaseReady
	return p
}

// TestCreateSnapshot_DiffNamesItsParent proves that a diff request carries
// the storage reference of its parent, and that the new Snapshot records
// the parent and the source Pod.
func TestCreateSnapshot_DiffNamesItsParent(t *testing.T) {
	sb := newSandboxForCoord()
	sb.Spec.Snapshot.Parent = "base"
	pod := newPodForSandbox(sb, "node-a")
	c := newFakeClient(t, sb, pod, readyParent(string(pod.UID)))
	na := &fakeNodeAgentClient{createResp: &setecgrpcv1.CreateSnapshotResponse{StorageRef: "t-a-snap-1"}}
	if err := newCoord(c, &fakeDialer{client: na}).CreateSnapshot(context.Background(), sb); err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	if na.lastCreate.GetParentStorageRef() != "t-a-base" {
		t.Fatalf("parent_storage_ref = %q, want t-a-base", na.lastCreate.GetParentStorageRef())
	}
	got := &setecv1alpha1.Snapshot{}
	_ = c.Get(context.Background(), types.NamespacedName{Namespace: "t-a", Name: "snap-1"}, got)
	if got.Spec.Parent != "base" || got.Annotations[setecv1alpha1.SnapshotSourcePodUIDAnnotation] != string(pod.UID) {
		t.Fatalf("snapshot = %+v %v", got.Spec, got.Annotations)
	}
}

// TestCreateSnapshot_DiffRefusesAParentOfAnotherMachine proves that a diff
// is refused before the machine is touched when its parent came from
// another Pod, another Sandbox, or is not Ready.
func TestCreateSnapshot_DiffRefusesAParentOfAnotherMachine(t *testing.T) {
	for name, mutate := range map[string]func(*setecv1alpha1.Snapshot){
		"another pod": func(p *setecv1alpha1.Snapshot) {
			p.Annotations[setecv1alpha1.SnapshotSourcePodUIDAnnotation] = "old-pod"
		},
		"another sandbox": func(p *setecv1alpha1.Snapshot) { p.Spec.SourceSandbox = "other" },
		"not ready":       func(p *setecv1alpha1.Snapshot) { p.Status.Phase = setecv1alpha1.SnapshotPhaseFailed },
		"another store":   func(p *setecv1alpha1.Snapshot) { p.Spec.StorageBackend = "s3" },
	} {
		t.Run(name, func(t *testing.T) {
			sb := newSandboxForCoord()
			sb.Spec.Snapshot.Parent = "base"
			pod := newPodForSandbox(sb, "node-a")
			parent := readyParent(string(pod.UID))
			mutate(parent)
			c := newFakeClient(t, sb, pod, parent)
			na := &fakeNodeAgentClient{createResp: &setecgrpcv1.CreateSnapshotResponse{StorageRef: "x"}}
			err := newCoord(c, &fakeDialer{client: na}).CreateSnapshot(context.Background(), sb)
			if !errors.Is(err, ErrInvalidParent) || na.lastCreate != nil {
				t.Fatalf("CreateSnapshot = %v, rpc = %v; want ErrInvalidParent and no call", err, na.lastCreate)
			}
		})
	}
}

// TestCreateSnapshot_RecordsTheCPUOfItsSource proves that a Snapshot names
// the CPU template of its class and the instance type of its node.
func TestCreateSnapshot_RecordsTheCPUOfItsSource(t *testing.T) {
	sb := newSandboxForCoord()
	pod := newPodForSandbox(sb, "node-a")
	cls := &setecv1alpha1.SandboxClass{Name: "standard"}
	cls.Spec.CPUTemplate = "fleet-v1"
	node := &corev1.Node{Name: "node-a"}
	node.Labels = map[string]string{corev1.LabelInstanceTypeStable: "m8i.2xlarge"}
	c := newFakeClient(t, sb, pod, cls, node)
	na := &fakeNodeAgentClient{createResp: &setecgrpcv1.CreateSnapshotResponse{StorageRef: "t-a-snap-1"}}
	if err := newCoord(c, &fakeDialer{client: na}).CreateSnapshot(context.Background(), sb); err != nil {
		t.Fatal(err)
	}
	got := &setecv1alpha1.Snapshot{}
	_ = c.Get(context.Background(), types.NamespacedName{Namespace: "t-a", Name: "snap-1"}, got)
	if got.Spec.CPUTemplate != "fleet-v1" || got.Spec.InstanceType != "m8i.2xlarge" {
		t.Fatalf("snapshot = %+v", got.Spec)
	}
}

// TestIsOwnFork pins who may load a forkable Snapshot (setec#195): another
// Sandbox of the same namespace, and nobody outside it.
func TestIsOwnFork(t *testing.T) {
	snap := &setecv1alpha1.Snapshot{Namespace: "pair-a", Name: "fork-1"}
	snap.Spec.SourceSandbox = "src"
	fork := &setecv1alpha1.Sandbox{Namespace: "pair-a", Name: "src-fork-x"}
	if isOwnFork(fork, snap) {
		t.Fatal("a Snapshot that is not forkable loads in another Sandbox")
	}
	snap.Spec.Forkable = true
	if !isOwnFork(fork, snap) {
		t.Fatal("a fork of its own namespace is refused")
	}
	other := &setecv1alpha1.Sandbox{Namespace: "pair-b", Name: "x"}
	if isOwnFork(other, snap) {
		t.Fatal("a Sandbox of another owner loads a fork")
	}
}

// TestValidate_KeptOpensOnlyInAReviewWithNoNetwork pins the review rule of
// setec#196.
func TestValidate_KeptOpensOnlyInAReviewWithNoNetwork(t *testing.T) {
	snap := &setecv1alpha1.Snapshot{Namespace: "t-a", Name: "kept-1"}
	snap.Spec = setecv1alpha1.SnapshotSpec{Kept: true, StorageRef: "r", SourceSandbox: "s"}
	sb := &setecv1alpha1.Sandbox{Namespace: "t-a", Name: "x"}
	if len(Validate(sb, snap, nil)) == 0 {
		t.Fatal("a normal launch from a kept snapshot was accepted")
	}
	sb.Spec.Review = true
	if len(Validate(sb, snap, nil)) == 0 {
		t.Fatal("a review Sandbox with a network was accepted")
	}
	sb.Spec.Network = &setecv1alpha1.Network{Mode: setecv1alpha1.NetworkModeNone}
	if v := Validate(sb, snap, nil); len(v) != 0 {
		t.Fatalf("a review Sandbox with no network was refused: %v", v)
	}
	if !isReviewOf(sb, snap) {
		t.Fatal("the gate does not trust a review of the own namespace")
	}
}

// TestCreateSnapshot_KeptIsSealedWithTheTenantKey pins the store of
// setec#196: a kept Snapshot goes to the S3 store with the key of its
// tenant, which the first kept Snapshot makes, and lives 30 days.
func TestCreateSnapshot_KeptIsSealedWithTheTenantKey(t *testing.T) {
	sb := newSandboxForCoord()
	sb.Spec.Snapshot.Kept = true
	pod := newPodForSandbox(sb, "node-a")
	c := newFakeClient(t, sb, pod)
	na := &fakeNodeAgentClient{createResp: &setecgrpcv1.CreateSnapshotResponse{StorageRef: "t-a-snap-1"}}
	if err := newCoord(c, &fakeDialer{client: na}).CreateSnapshot(context.Background(), sb); err != nil {
		t.Fatal(err)
	}
	key := &corev1.Secret{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "t-a", Name: TenantKEKSecret}, key); err != nil {
		t.Fatalf("the tenant key: %v", err)
	}
	if na.lastCreate.GetStorageBackend() != KeptBackend || string(na.lastCreate.GetSessionKek()) != string(key.Data["kek"]) {
		t.Fatalf("create = backend %q, key sent %v", na.lastCreate.GetStorageBackend(), len(na.lastCreate.GetSessionKek()))
	}
	got := &setecv1alpha1.Snapshot{}
	_ = c.Get(context.Background(), types.NamespacedName{Namespace: "t-a", Name: "snap-1"}, got)
	if !got.Spec.Kept || got.Spec.StorageBackend != KeptBackend || got.Spec.TTL == nil || got.Spec.TTL.Duration != DefaultKeptTTL {
		t.Fatalf("kept snapshot = %+v", got.Spec)
	}
}
