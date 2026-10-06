// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	setecgrpcv1 "github.com/zeroroot-ai/setec/api/grpc/v1"
	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
	"github.com/zeroroot-ai/setec/internal/controller/testutil"
	runtimepkg "github.com/zeroroot-ai/setec/internal/runtime"
	snapshotpkg "github.com/zeroroot-ai/setec/internal/snapshot"
)

func durableFixture(t *testing.T, na *fakeNodeAgentClient, objs ...client.Object) (*SandboxReconciler, *setecv1alpha1.Sandbox) {
	t.Helper()
	s := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(s))
	utilruntime.Must(setecv1alpha1.AddToScheme(s))
	sb := &setecv1alpha1.Sandbox{Name: "sess", Namespace: "tenant", UID: "sb-uid"}
	sb.Spec.Lifecycle = &setecv1alpha1.Lifecycle{Mode: setecv1alpha1.LifecycleModeSession}
	sb.Status.Runtime = &setecv1alpha1.SandboxRuntimeStatus{Chosen: runtimepkg.BackendLauncher}
	sb.Status.PodName = "sess-vm"
	pod := &corev1.Pod{Name: "sess-vm", Namespace: "tenant", UID: "pod-1"}
	pod.Spec.NodeName = "node-a"
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(append([]client.Object{sb, pod}, objs...)...).
		WithStatusSubresource(&setecv1alpha1.Sandbox{}).Build()
	r := &SandboxReconciler{Client: c, Scheme: s, Recorder: testutil.NewFakeEventsRecorder(16),
		Coordinator: &snapshotpkg.Coordinator{Client: c, Dialer: &fakeNodeAgentDialer{client: na}}}
	return r, sb
}

// TestTakeCheckpoint_DurableTakesADiffOnTheSameMachine pins the diffs of
// setec#194: a durable launcher session diffs on the last checkpoint of
// the same Pod and keeps the chain; another Pod starts a full checkpoint.
func TestTakeCheckpoint_DurableTakesADiffOnTheSameMachine(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	na := &fakeNodeAgentClient{CreateResp: &setecgrpcv1.CreateSnapshotResponse{StorageRef: "ref-2"}}
	r, sb := durableFixture(t, na)
	sb.Status.Checkpoint = &setecv1alpha1.SandboxCheckpointStatus{Ref: "ref-1", Backend: "s3", Sequence: 1, PodUID: "pod-1"}
	if err := r.Status().Update(ctx, sb); err != nil {
		t.Fatal(err)
	}
	policy := &setecv1alpha1.SessionCheckpointSpec{Durable: true}
	if err := r.takeCheckpoint(ctx, logr.Discard(), sb, policy, false); err != nil {
		t.Fatal(err)
	}
	if na.LastCreate.GetParentStorageRef() != "ref-1" {
		t.Fatalf("parent = %q, want a diff on ref-1", na.LastCreate.GetParentStorageRef())
	}
	ck := sb.Status.Checkpoint
	if ck.Ref != "ref-2" || len(ck.Parents) != 1 || ck.Parents[0] != "ref-1" || ck.PodUID != "pod-1" {
		t.Fatalf("checkpoint = %+v", ck)
	}

	// A checkpoint of another Pod cannot carry a diff.
	sb.Status.Checkpoint.PodUID = "old-pod"
	if err := r.Status().Update(ctx, sb); err != nil {
		t.Fatal(err)
	}
	if err := r.takeCheckpoint(ctx, logr.Discard(), sb, policy, false); err != nil {
		t.Fatal(err)
	}
	if na.LastCreate.GetParentStorageRef() != "" || len(sb.Status.Checkpoint.Parents) != 0 {
		t.Fatalf("a checkpoint after a new Pod is a diff: %+v", sb.Status.Checkpoint)
	}
	if policy.EffectiveInterval() != 15*time.Minute {
		t.Fatalf("durable interval = %v, want 15m", policy.EffectiveInterval())
	}
}

// TestNodeLoss_MovesADurableSession pins the node loss of setec#194: a
// node not Ready past the grace removes the Pod with no grace and marks the
// last checkpoint for the restore.
func TestNodeLoss_MovesADurableSession(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	node := &corev1.Node{Name: "node-a"}
	node.Status.Conditions = []corev1.NodeCondition{{
		Type: corev1.NodeReady, Status: corev1.ConditionUnknown,
		LastTransitionTime: metav1.NewTime(time.Now().Add(-5 * time.Minute)),
	}}
	r, sb := durableFixture(t, &fakeNodeAgentClient{}, node)
	if lost, ok := r.nodeLostFor(ctx, "node-a"); !ok || lost < 4*time.Minute {
		t.Fatalf("nodeLostFor = %v, %v", lost, ok)
	}
	sb.Status.Checkpoint = &setecv1alpha1.SandboxCheckpointStatus{Ref: "ref-1", Backend: "s3"}
	if err := r.Status().Update(ctx, sb); err != nil {
		t.Fatal(err)
	}
	pod := &corev1.Pod{}
	_ = r.Get(ctx, client.ObjectKey{Namespace: "tenant", Name: "sess-vm"}, pod)
	if _, err := r.resumeAfterNodeLoss(ctx, logr.Discard(), sb, pod); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(ctx, client.ObjectKey{Namespace: "tenant", Name: "sess-vm"}, &corev1.Pod{}); err == nil {
		t.Fatal("the Pod of the lost node stays")
	}
	if !sb.Status.Checkpoint.PendingRestore {
		t.Fatal("the last checkpoint is not marked for the restore")
	}
	if _, ok := r.nodeLostFor(ctx, "node-gone"); !ok {
		t.Fatal("a deleted node is not lost")
	}
}

// TestRestorePendingCheckpoint_RunsOnceOnAStaleCache pins the fix of a
// drain run of setec#197: the cache still showed the pending checkpoint
// after the restore consumed it, and a second restore ended the healthy
// session. The live object decides, so a stale cache restores nothing.
func TestRestorePendingCheckpoint_RunsOnceOnAStaleCache(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	na := &fakeNodeAgentClient{RestoreErr: errors.New("a second restore must not run")}
	r, sb := durableFixture(t, na)
	sb.Status.Checkpoint = &setecv1alpha1.SandboxCheckpointStatus{Ref: "ref-1", Backend: "s3", Sequence: 1, PendingRestore: true}
	if err := r.Status().Update(ctx, sb); err != nil {
		t.Fatal(err)
	}
	// The API server already holds the consumed checkpoint.
	live := sb.DeepCopy()
	live.Status.Checkpoint = &setecv1alpha1.SandboxCheckpointStatus{Backend: "s3", Sequence: 1,
		LastRecovery: setecv1alpha1.SessionRecoveryResumedFromCheckpoint}
	r.APIReader = fake.NewClientBuilder().WithScheme(r.Scheme).WithObjects(live).Build()

	_, handled, err := r.restorePendingCheckpoint(ctx, logr.Discard(), sb, &setecv1alpha1.SessionCheckpointSpec{})
	if err != nil || !handled {
		t.Fatalf("restorePendingCheckpoint = %t, %v", handled, err)
	}
	if err := r.Get(ctx, client.ObjectKey{Namespace: "tenant", Name: "sess-vm"}, &corev1.Pod{}); err != nil {
		t.Fatalf("the healthy session Pod is gone: %v", err)
	}
	if got := sb.Status.Checkpoint.LastRecovery; got == setecv1alpha1.SessionRecoveryRestartedFromWorkspace {
		t.Fatal("a stale cache degraded the session to RestartedFromWorkspace")
	}
}

// TestReconcileSessionCheckpoint_NeverRestoresIntoTheWritingPod pins
// setec#220: after a suspend, a stale cache can show the Pod that wrote
// the checkpoint as Running while the Sandbox shows the pending restore.
// The checkpoint must not load into that Pod.
func TestReconcileSessionCheckpoint_NeverRestoresIntoTheWritingPod(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	na := &fakeNodeAgentClient{RestoreErr: errors.New("no restore into the writing Pod")}
	r, sb := durableFixture(t, na)
	// A session that is neither idle nor due for a checkpoint, so only
	// the restore step can act.
	now := metav1.Now()
	sb.Annotations = map[string]string{setecv1alpha1.AnnotationLastActivity: now.UTC().Format(time.RFC3339)}
	if err := r.Update(ctx, sb); err != nil {
		t.Fatal(err)
	}
	sb.Status.StartedAt = &now
	sb.Status.Checkpoint = &setecv1alpha1.SandboxCheckpointStatus{
		Ref: "ref-1", Backend: "s3", Sequence: 1, PendingRestore: true, PodUID: "pod-1"}
	if err := r.Status().Update(ctx, sb); err != nil {
		t.Fatal(err)
	}
	pod := &corev1.Pod{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: "tenant", Name: "sess-vm"}, pod); err != nil {
		t.Fatal(err)
	}
	pod.Status.Phase = corev1.PodRunning
	cls := &setecv1alpha1.SandboxClass{}
	cls.Spec.SessionCheckpoint = &setecv1alpha1.SessionCheckpointSpec{Backend: "s3"}
	desired := setecv1alpha1.SandboxStatus{Phase: setecv1alpha1.SandboxPhaseRunning}

	if _, _, err := r.reconcileSessionCheckpoint(ctx, logr.Discard(), sb, cls, pod, desired); err != nil {
		t.Fatalf("reconcileSessionCheckpoint: %v", err)
	}
	if err := r.Get(ctx, client.ObjectKey{Namespace: "tenant", Name: "sess-vm"}, &corev1.Pod{}); err != nil {
		t.Fatalf("a restore into the writing Pod deleted it: %v", err)
	}
	if !sb.Status.Checkpoint.PendingRestore {
		t.Fatal("the checkpoint was consumed by a restore into the Pod that wrote it")
	}
}
