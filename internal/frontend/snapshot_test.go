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

var snapshotTestImage = "img@sha256:" + strings.Repeat("b", 64)

// snapshotTestService is a Service on a fake client with a running launcher
// sandbox "src" in team-a, and an operator that makes each snapshot that the
// source asks for Ready.
func snapshotTestService(t *testing.T, objs ...client.Object) (*Service, client.Client) {
	t.Helper()
	ctx := context.Background()
	scheme := runtime.NewScheme()
	utilruntime.Must(setecv1alpha1.AddToScheme(scheme))
	src := &setecv1alpha1.Sandbox{Name: "src", Namespace: "team-a"}
	src.Spec.Image = snapshotTestImage
	src.Spec.Command = []string{"python", "agent.py"}
	src.Spec.Network = &setecv1alpha1.Network{Mode: "external-only"}
	src.Status.Phase = setecv1alpha1.SandboxPhaseRunning
	src.Status.Runtime = &setecv1alpha1.SandboxRuntimeStatus{Chosen: runtimepkg.BackendLauncher}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(append(objs, src)...).
		WithStatusSubresource(&setecv1alpha1.Snapshot{}).Build()
	go func() {
		for range 200 {
			got := &setecv1alpha1.Sandbox{}
			_ = c.Get(ctx, types.NamespacedName{Namespace: "team-a", Name: "src"}, got)
			if sp := got.Spec.Snapshot; sp != nil && sp.Create {
				snap := &setecv1alpha1.Snapshot{Name: sp.Name, Namespace: "team-a"}
				snap.Spec.Forkable = sp.Forkable
				snap.Spec.TTL = sp.TTL
				_ = c.Create(ctx, snap)
				snap.Status.Phase = setecv1alpha1.SnapshotPhaseReady
				_ = c.Status().Update(ctx, snap)
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	return &Service{Client: c, AuthDisabled: true, DefaultNamespace: "team-a"}, c
}

// readySnapshot is a Ready snapshot of the Snapshot call in team-a.
func readySnapshot(name string) *setecv1alpha1.Snapshot {
	snap := &setecv1alpha1.Snapshot{Name: name, Namespace: "team-a", Annotations: map[string]string{
		setecv1alpha1.SnapshotVCPUAnnotation:   "2",
		setecv1alpha1.SnapshotMemoryAnnotation: "1Gi",
		snapshotCommandAnnotation:              `["python","agent.py"]`,
	}}
	snap.Spec.Forkable = true
	snap.Spec.SandboxClass = "standard"
	snap.Spec.ImageRef = snapshotTestImage
	snap.Status.Phase = setecv1alpha1.SnapshotPhaseReady
	return snap
}

// TestSnapshot_LivesSevenDaysAndRecordsTheCommand pins setec#242: the
// Snapshot call asks for a forkable snapshot that lives 7 days, and records
// the command of the source on it.
func TestSnapshot_LivesSevenDaysAndRecordsTheCommand(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, c := snapshotTestService(t)
	resp, err := s.Snapshot(ctx, &setecv1grpc.SnapshotRequest{SandboxId: "team-a/src/uid"})
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if !strings.HasPrefix(resp.GetSnapshot(), "snap-src-") {
		t.Fatalf("snapshot = %q", resp.GetSnapshot())
	}
	snap := &setecv1alpha1.Snapshot{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: "team-a", Name: resp.GetSnapshot()}, snap); err != nil {
		t.Fatal(err)
	}
	if !snap.Spec.Forkable || snap.Spec.TTL == nil || snap.Spec.TTL.Duration != 7*24*time.Hour {
		t.Fatalf("snapshot spec = forkable %t ttl %v, want forkable for 7 days", snap.Spec.Forkable, snap.Spec.TTL)
	}
	if got := snap.Annotations[snapshotCommandAnnotation]; got != `["python","agent.py"]` {
		t.Fatalf("command record = %q", got)
	}
}

// TestSnapshot_TTLAndOwner: a TTL of the request wins, a negative TTL is
// refused, and a sandbox of another pair is refused.
func TestSnapshot_TTLAndOwner(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, c := snapshotTestService(t)
	if _, err := s.Snapshot(ctx, &setecv1grpc.SnapshotRequest{SandboxId: "team-a/src/uid", TtlSeconds: -1}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("negative ttl = %v", err)
	}
	if _, err := s.Snapshot(ctx, &setecv1grpc.SnapshotRequest{SandboxId: "team-b/src/uid"}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("another pair = %v", err)
	}
	resp, err := s.Snapshot(ctx, &setecv1grpc.SnapshotRequest{SandboxId: "team-a/src/uid", TtlSeconds: 3600})
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	snap := &setecv1alpha1.Snapshot{}
	_ = c.Get(ctx, types.NamespacedName{Namespace: "team-a", Name: resp.GetSnapshot()}, snap)
	if snap.Spec.TTL == nil || snap.Spec.TTL.Duration != time.Hour {
		t.Fatalf("ttl = %v, want 1h", snap.Spec.TTL)
	}
}

// TestSnapshot_RefusesASessionAndAStoppedSandbox: the source must be a
// running launcher sandbox that is not a session.
func TestSnapshot_RefusesASessionAndAStoppedSandbox(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	stopped := &setecv1alpha1.Sandbox{Name: "done", Namespace: "team-a"}
	stopped.Status.Phase = setecv1alpha1.SandboxPhaseCompleted
	stopped.Status.Runtime = &setecv1alpha1.SandboxRuntimeStatus{Chosen: runtimepkg.BackendLauncher}
	sess := &setecv1alpha1.Sandbox{Name: "sess", Namespace: "team-a"}
	sess.Spec.Lifecycle = &setecv1alpha1.Lifecycle{Mode: setecv1alpha1.LifecycleModeSession}
	sess.Status.Phase = setecv1alpha1.SandboxPhaseRunning
	sess.Status.Runtime = &setecv1alpha1.SandboxRuntimeStatus{Chosen: runtimepkg.BackendLauncher}
	s, _ := snapshotTestService(t, stopped, sess)
	for _, id := range []string{"team-a/done/uid", "team-a/sess/uid"} {
		if _, err := s.Snapshot(ctx, &setecv1grpc.SnapshotRequest{SandboxId: id}); status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("%s = %v, want FailedPrecondition", id, err)
		}
	}
}

// TestLaunch_FromSnapshotIsANormalSandboxWithTheRequestNetwork pins the
// launch of setec#242: the sandbox loads the snapshot, takes its class,
// image, size and command, and gets the network of the request. It is not
// a review sandbox.
func TestLaunch_FromSnapshotIsANormalSandboxWithTheRequestNetwork(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, c := snapshotTestService(t, readySnapshot("snap-src-1"))
	resp, err := s.Launch(ctx, &setecv1grpc.LaunchRequest{
		FromSnapshot: "snap-src-1",
		Network:      &setecv1grpc.Network{Mode: "egress-allow-list", Allow: []*setecv1grpc.NetworkAllow{{Host: "example.org", Port: 443}}},
		Env:          map[string]string{"RUN": "2"},
	})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	sb := &setecv1alpha1.Sandbox{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: "team-a", Name: resp.GetName()}, sb); err != nil {
		t.Fatal(err)
	}
	switch {
	case sb.Spec.SnapshotRef == nil || sb.Spec.SnapshotRef.Name != "snap-src-1":
		t.Fatalf("snapshotRef = %+v", sb.Spec.SnapshotRef)
	case sb.Spec.Review:
		t.Fatal("a sandbox from a snapshot is a review sandbox")
	case sb.Spec.SandboxClassName != "standard" || sb.Spec.Image != snapshotTestImage:
		t.Fatalf("class %q image %q", sb.Spec.SandboxClassName, sb.Spec.Image)
	case sb.Spec.Resources.VCPU != 2 || sb.Spec.Resources.Memory.String() != "1Gi":
		t.Fatalf("resources = %+v", sb.Spec.Resources)
	case strings.Join(sb.Spec.Command, " ") != "python agent.py":
		t.Fatalf("command = %v", sb.Spec.Command)
	case sb.Spec.Network == nil || sb.Spec.Network.Mode != "egress-allow-list" ||
		len(sb.Spec.Network.Allow) != 1 || sb.Spec.Network.Allow[0].Host != "example.org":
		t.Fatalf("network = %+v, want the network of the request", sb.Spec.Network)
	case sb.Labels[fromSnapshotLabel] != "snap-src-1":
		t.Fatalf("labels = %v", sb.Labels)
	}
}

// TestLaunch_FromSnapshotRefusals pins the refusals: a snapshot of another
// pair is not found, a kept or not forkable or not Ready snapshot is
// refused, a request that names another class, image or size is refused,
// and a session or review_snapshot with it is refused.
func TestLaunch_FromSnapshotRefusals(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	other := readySnapshot("snap-other")
	other.Namespace = "team-b"
	kept := readySnapshot("kept-1")
	kept.Spec.Kept = true
	plain := readySnapshot("plain-1")
	plain.Spec.Forkable = false
	creating := readySnapshot("creating-1")
	creating.Status.Phase = setecv1alpha1.SnapshotPhaseCreating
	s, _ := snapshotTestService(t, readySnapshot("snap-src-1"), other, kept, plain, creating)

	cases := []struct {
		name string
		req  *setecv1grpc.LaunchRequest
		want codes.Code
	}{
		{"another pair", &setecv1grpc.LaunchRequest{FromSnapshot: "snap-other"}, codes.NotFound},
		{"kept", &setecv1grpc.LaunchRequest{FromSnapshot: "kept-1"}, codes.FailedPrecondition},
		{"not forkable", &setecv1grpc.LaunchRequest{FromSnapshot: "plain-1"}, codes.FailedPrecondition},
		{"not ready", &setecv1grpc.LaunchRequest{FromSnapshot: "creating-1"}, codes.FailedPrecondition},
		{"other class", &setecv1grpc.LaunchRequest{FromSnapshot: "snap-src-1", SandboxClass: "big"}, codes.InvalidArgument},
		{"other image", &setecv1grpc.LaunchRequest{FromSnapshot: "snap-src-1", Image: "other@sha256:" + strings.Repeat("c", 64)}, codes.InvalidArgument},
		{"other size", &setecv1grpc.LaunchRequest{FromSnapshot: "snap-src-1", Resources: &setecv1grpc.Resources{Vcpu: 4}}, codes.InvalidArgument},
		{"session", &setecv1grpc.LaunchRequest{FromSnapshot: "snap-src-1", Lifecycle: &setecv1grpc.Lifecycle{Mode: "session"}}, codes.InvalidArgument},
		{"with review", &setecv1grpc.LaunchRequest{FromSnapshot: "snap-src-1", ReviewSnapshot: "kept-1"}, codes.InvalidArgument},
	}
	for _, tc := range cases {
		if _, err := s.Launch(ctx, tc.req); status.Code(err) != tc.want {
			t.Errorf("%s: %v, want %s", tc.name, err, tc.want)
		}
	}
}

// TestLaunch_FromSnapshotMatchingFieldsAndCommand: fields equal to the
// snapshot pass, and a command of the request replaces the recorded one.
func TestLaunch_FromSnapshotMatchingFieldsAndCommand(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, c := snapshotTestService(t, readySnapshot("snap-src-1"))
	resp, err := s.Launch(ctx, &setecv1grpc.LaunchRequest{
		FromSnapshot: "snap-src-1", SandboxClass: "standard", Image: snapshotTestImage,
		Resources: &setecv1grpc.Resources{Vcpu: 2, Memory: "1024Mi"}, Command: []string{"sh"},
	})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	sb := &setecv1alpha1.Sandbox{}
	_ = c.Get(ctx, types.NamespacedName{Namespace: "team-a", Name: resp.GetName()}, sb)
	if strings.Join(sb.Spec.Command, " ") != "sh" {
		t.Fatalf("command = %v", sb.Spec.Command)
	}
}
