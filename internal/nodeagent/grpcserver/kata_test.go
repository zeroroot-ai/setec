// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package grpcserver

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	setecgrpcv1 "github.com/zeroroot-ai/setec/api/grpc/v1"
	"github.com/zeroroot-ai/setec/internal/firecracker"
	"github.com/zeroroot-ai/setec/internal/nodeagent/katasandbox"
)

// testPodUID is the Pod UID every test request names.
const testPodUID = "05c716c8-eae5-4540-9daf-5c0bb659810a"

// fakeKata resolves a known Pod UID to kata-shaped paths under /kata,
// and any other UID to katasandbox.ErrNotFound.
type fakeKata struct{}

func fakeKataPaths(podUID string) katasandbox.Paths {
	vm := filepath.Join("/kata", podUID, "root")
	return katasandbox.Paths{
		APISocket:   filepath.Join(vm, "run", "firecracker.socket"),
		HybridVsock: filepath.Join(vm, "kata.hvsock"),
	}
}

func (fakeKata) Resolve(_ context.Context, podUID string) (katasandbox.Paths, error) {
	if podUID != testPodUID {
		return katasandbox.Paths{}, katasandbox.ErrNotFound
	}
	return fakeKataPaths(podUID), nil
}

// TestCreateSnapshot_DialsTheResolvedKataSocket asserts that the
// node-agent dials the socket it resolved from the Pod UID, not a path
// the operator guessed (setec#19).
func TestCreateSnapshot_DialsTheResolvedKataSocket(t *testing.T) {
	fc := &fakeFirecracker{}
	srv := newServer(t, fc, nil)
	var dialed string
	srv.FirecrackerFactory = func(sock string) firecracker.Client { dialed = sock; return fc }
	if _, err := srv.CreateSnapshot(context.Background(), &setecgrpcv1.CreateSnapshotRequest{
		SnapshotId: "snap-kata", SourcePodUid: testPodUID,
	}); err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	if want := fakeKataPaths(testPodUID).APISocket; dialed != want {
		t.Fatalf("dialed %q, want the resolved socket %q", dialed, want)
	}
}

// TestKataPaths_Errors asserts the status each resolution failure maps
// to.
func TestKataPaths_Errors(t *testing.T) {
	srv := newServer(t, &fakeFirecracker{}, nil)
	ctx := context.Background()

	if _, err := srv.kataPaths(ctx, "", "target_pod_uid"); status.Code(err) != codes.InvalidArgument {
		t.Errorf("empty UID: %v, want InvalidArgument", err)
	}
	const otherPod = "11111111-2222-3333-4444-555555555555"
	if _, err := srv.kataPaths(ctx, otherPod, "target_pod_uid"); status.Code(err) != codes.NotFound {
		t.Errorf("unknown pod: %v, want NotFound", err)
	}
	srv.KataSandboxes = failingKata{}
	if _, err := srv.kataPaths(ctx, testPodUID, "target_pod_uid"); status.Code(err) != codes.Internal {
		t.Errorf("resolver failure: %v, want Internal", err)
	}
	srv.KataSandboxes = nil
	if _, err := srv.kataPaths(ctx, testPodUID, "target_pod_uid"); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("no resolver: %v, want FailedPrecondition", err)
	}
}

type failingKata struct{}

func (failingKata) Resolve(context.Context, string) (katasandbox.Paths, error) {
	return katasandbox.Paths{}, errors.New("containerd unreachable")
}
