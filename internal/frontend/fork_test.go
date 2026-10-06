// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package frontend

import (
	"context"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	setecv1grpc "github.com/zeroroot-ai/setec/api/grpc/v1"
	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
	runtimepkg "github.com/zeroroot-ai/setec/internal/runtime"
)

// TestFork_SnapshotsTheSourceAndStartsTheForks pins the fork of setec#195:
// a forkable snapshot with the asked lifetime, then count Sandboxes from
// it with the network of the request and not the network of the source.
func TestFork_SnapshotsTheSourceAndStartsTheForks(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	scheme := runtime.NewScheme()
	utilruntime.Must(setecv1alpha1.AddToScheme(scheme))
	src := &setecv1alpha1.Sandbox{Name: "src", Namespace: "team-a"}
	src.Spec.Image = "img@sha256:" + strings.Repeat("a", 64)
	src.Spec.Command = []string{"sh"}
	src.Spec.Network = &setecv1alpha1.Network{Mode: "external-only"}
	src.Status.Phase = setecv1alpha1.SandboxPhaseRunning
	src.Status.Runtime = &setecv1alpha1.SandboxRuntimeStatus{Chosen: runtimepkg.BackendLauncher}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(src).
		WithStatusSubresource(&setecv1alpha1.Snapshot{}).Build()
	s := &Service{Client: c, AuthDisabled: true, DefaultNamespace: "team-a"}

	// The operator side: make the snapshot that the source asks for Ready.
	go func() {
		for range 100 {
			got := &setecv1alpha1.Sandbox{}
			_ = c.Get(ctx, types.NamespacedName{Namespace: "team-a", Name: "src"}, got)
			if sp := got.Spec.Snapshot; sp != nil && sp.Create && sp.Forkable {
				snap := &setecv1alpha1.Snapshot{Name: sp.Name, Namespace: "team-a"}
				snap.Spec.Forkable = true
				_ = c.Create(ctx, snap)
				snap.Status.Phase = setecv1alpha1.SnapshotPhaseReady
				_ = c.Status().Update(ctx, snap)
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()

	resp, err := s.Fork(ctx, &setecv1grpc.ForkRequest{
		SandboxId: "team-a/src/uid", Count: 3, SnapshotTtlSeconds: 600,
		Network: &setecv1grpc.Network{Mode: "none"},
	})
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	if len(resp.GetSandboxIds()) != 3 || !strings.HasPrefix(resp.GetSnapshot(), "fork-src-") {
		t.Fatalf("Fork = %+v", resp)
	}
	got := &setecv1alpha1.Sandbox{}
	_ = c.Get(ctx, types.NamespacedName{Namespace: "team-a", Name: "src"}, got)
	if got.Spec.Snapshot.TTL.Duration != 10*time.Minute {
		t.Fatalf("snapshot ttl = %v", got.Spec.Snapshot.TTL)
	}
	forks := &setecv1alpha1.SandboxList{}
	_ = c.List(ctx, forks, client.MatchingLabels{forkOfLabel: "src"})
	if len(forks.Items) != 3 {
		t.Fatalf("forks = %d", len(forks.Items))
	}
	for _, f := range forks.Items {
		if f.Spec.SnapshotRef == nil || f.Spec.SnapshotRef.Name != resp.GetSnapshot() ||
			f.Spec.Network == nil || f.Spec.Network.Mode != "none" {
			t.Fatalf("fork = %+v", f.Spec)
		}
	}

	// A session or a count out of range is refused.
	if _, err := s.Fork(ctx, &setecv1grpc.ForkRequest{SandboxId: "team-a/src/uid", Count: 0}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("count 0 = %v", err)
	}
}
