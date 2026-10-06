// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	setecgrpcv1 "github.com/zeroroot-ai/setec/api/grpc/v1"
	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
	runtimepkg "github.com/zeroroot-ai/setec/internal/runtime"
	snapshotpkg "github.com/zeroroot-ai/setec/internal/snapshot"
	"github.com/zeroroot-ai/setec/internal/status"
)

// restoreFixture is a launcher Sandbox named work whose snapshotRef names a
// Ready local Snapshot of a Sandbox also named work, and its running Pod.
func restoreFixture(t *testing.T, na *fakeNodeAgentClient) (*SandboxReconciler, *setecv1alpha1.Sandbox) {
	t.Helper()
	s := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(s))
	utilruntime.Must(setecv1alpha1.AddToScheme(s))
	sb := &setecv1alpha1.Sandbox{Name: "work", Namespace: "tenant"}
	sb.Spec.SnapshotRef = &setecv1alpha1.SandboxSnapshotRef{Name: "snap"}
	sb.Status.Runtime = &setecv1alpha1.SandboxRuntimeStatus{Chosen: runtimepkg.BackendLauncher}
	snap := &setecv1alpha1.Snapshot{Name: "snap", Namespace: "tenant"}
	snap.Spec = setecv1alpha1.SnapshotSpec{SourceSandbox: "work", Node: "node-a", StorageBackend: "local-disk", StorageRef: "tenant-snap"}
	pod := &corev1.Pod{Name: "work-vm", Namespace: "tenant"}
	pod.Spec.NodeName = "node-a"
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(sb, snap, pod).Build()
	r := &SandboxReconciler{Client: c, Coordinator: &snapshotpkg.Coordinator{
		Client: c, Dialer: &fakeNodeAgentDialer{client: na},
	}}
	return r, sb
}

func TestMaybeRestoreLauncher_RunningOnlyAfterTheGatePasses(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r, sb := restoreFixture(t, &fakeNodeAgentClient{})
	pending := holdUntilRestored(sb, r.maybeRestoreLauncher(ctx, sb, setecv1alpha1.SandboxStatus{Phase: setecv1alpha1.SandboxPhasePending}))
	if pending.Phase != setecv1alpha1.SandboxPhasePending || pending.Reason != ReasonRestoring {
		t.Fatalf("before the Pod runs = %+v; want Pending Restoring", pending)
	}
	got := r.maybeRestoreLauncher(ctx, sb, setecv1alpha1.SandboxStatus{Phase: setecv1alpha1.SandboxPhaseRunning})
	if got.Phase != setecv1alpha1.SandboxPhaseRunning {
		t.Fatalf("after a confirmed restore = %+v; want Running", got)
	}
	stored := &setecv1alpha1.Sandbox{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(sb), stored); err != nil || stored.Annotations[RestoredAnnotation] != "snap" {
		t.Fatalf("the restored annotation = %v, %v", stored.Annotations, err)
	}
	if needsLauncherRestore(stored) {
		t.Fatal("a restored Sandbox still needs a restore")
	}
}

func TestMaybeRestoreLauncher_UnconfirmedGuestFailsTheSandbox(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r, sb := restoreFixture(t, &fakeNodeAgentClient{RestoreRes: &setecgrpcv1.RestoreSandboxResponse{
		Success: true, EntropyReseeded: true, Uniquified: false, EncryptedAtRest: true,
	}})
	got := r.maybeRestoreLauncher(ctx, sb, setecv1alpha1.SandboxStatus{Phase: setecv1alpha1.SandboxPhaseRunning})
	if got.Phase != setecv1alpha1.SandboxPhaseFailed || got.Reason != status.ReasonInvariantGateViolation {
		t.Fatalf("a guest with no new identity = %+v; want Failed %s", got, status.ReasonInvariantGateViolation)
	}

	// A Snapshot of another Sandbox is refused before any state loads.
	r2, sb2 := restoreFixture(t, &fakeNodeAgentClient{})
	snap := &setecv1alpha1.Snapshot{}
	_ = r2.Get(ctx, types.NamespacedName{Namespace: "tenant", Name: "snap"}, snap)
	snap.Spec.SourceSandbox = "other"
	if err := r2.Update(ctx, snap); err != nil {
		t.Fatal(err)
	}
	if got := r2.maybeRestoreLauncher(ctx, sb2, setecv1alpha1.SandboxStatus{Phase: setecv1alpha1.SandboxPhaseRunning}); got.Phase != setecv1alpha1.SandboxPhaseFailed {
		t.Fatalf("a restore from another Sandbox = %+v; want Failed", got)
	}
}
