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

package grpcserver

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	setecgrpcv1 "github.com/zeroroot-ai/setec/api/grpc/v1"
	"github.com/zeroroot-ai/setec/internal/firecracker"
	"github.com/zeroroot-ai/setec/internal/snapshot/atrest"
	"github.com/zeroroot-ai/setec/internal/snapshot/storage"
)

// sessionTestServer wires a Server whose "s3" session backend is the
// encrypted wrapper over a local-disk inner (the composition is
// identical to production apart from the inner transport), keyed by
// whatever per-call KEK the RPC forwards.
func sessionTestServer(t *testing.T, fc *fakeFirecracker) *Server {
	t.Helper()
	base := t.TempDir()
	inner := &storage.LocalDiskBackend{Root: filepath.Join(base, "portable")}
	return &Server{
		Storage: &storage.EncryptedBackend{
			Inner: &storage.LocalDiskBackend{Root: filepath.Join(base, "local")},
			KEK:   &storage.FileKEKSource{Path: filepath.Join(base, "keys", "node.key")},
			DEKs:  &storage.DirDEKStore{Dir: filepath.Join(base, "keys", "dek")},
		},
		SessionStorage: func(kek []byte) storage.StorageBackend {
			return &storage.EncryptedBackend{
				Inner: inner,
				KEK:   storage.StaticKEKSource(kek),
				DEKs:  &storage.DirDEKStore{Dir: filepath.Join(base, "portable-dek")},
			}
		},
		FirecrackerFactory: func(string) firecracker.Client { return fc },
		TempDir:            filepath.Join(base, "tmp"),
	}
}

func testKEK(fill byte) []byte { return bytes.Repeat([]byte{fill}, atrest.KeySize) }

func TestSessionKEKRouting(t *testing.T) {
	ctx := context.Background()
	fc := &fakeFirecracker{}
	srv := sessionTestServer(t, fc)

	// session_kek with the local-disk backend is rejected.
	_, err := srv.CreateSnapshot(ctx, &setecgrpcv1.CreateSnapshotRequest{
		SnapshotId:       "bad",
		StorageBackend:   "local-disk",
		SourceKataSocket: "/tmp/sock",
		SessionKek:       testKEK(1),
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("session_kek+local-disk = %v, want InvalidArgument", err)
	}

	// Unknown backend is rejected.
	_, err = srv.CreateSnapshot(ctx, &setecgrpcv1.CreateSnapshotRequest{
		SnapshotId: "bad2", StorageBackend: "nfs", SourceKataSocket: "/tmp/sock",
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("unknown backend = %v, want InvalidArgument", err)
	}

	// s3 without a configured session backend fails FailedPrecondition.
	srvNoS3 := sessionTestServer(t, fc)
	srvNoS3.SessionStorage = nil
	_, err = srvNoS3.CreateSnapshot(ctx, &setecgrpcv1.CreateSnapshotRequest{
		SnapshotId: "x", StorageBackend: "s3", SourceKataSocket: "/tmp/sock", SessionKek: testKEK(1),
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("s3 unconfigured = %v, want FailedPrecondition", err)
	}
}

func TestSessionCheckpointRoundTripThroughServer(t *testing.T) {
	ctx := context.Background()
	fc := &fakeFirecracker{}
	srv := sessionTestServer(t, fc)
	kek := testKEK(7)

	resp, err := srv.CreateSnapshot(ctx, &setecgrpcv1.CreateSnapshotRequest{
		SandboxId:        "ns/sb",
		SnapshotId:       "ns-sb-ckpt-1",
		StorageBackend:   "s3",
		SourceKataSocket: "/tmp/sock",
		SessionKek:       kek,
	})
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	if resp.GetStorageRef() == "" {
		t.Fatal("empty storage ref")
	}

	// Restore with the right KEK succeeds and reaches LoadSnapshot.
	rresp, err := srv.RestoreSandbox(ctx, &setecgrpcv1.RestoreSandboxRequest{
		SnapshotId:       "ns-sb-ckpt-1",
		StorageRef:       resp.GetStorageRef(),
		StorageBackend:   "s3",
		KataSocketTarget: "/tmp/target-sock",
		SessionKek:       kek,
	})
	if err != nil || !rresp.GetSuccess() {
		t.Fatalf("RestoreSandbox = (%v,%v), want success", rresp, err)
	}
	if len(fc.loadCalls) != 1 {
		t.Fatalf("LoadSnapshot calls = %d, want 1", len(fc.loadCalls))
	}

	// Restore with the wrong KEK fails closed (corrupted, DataLoss).
	_, err = srv.RestoreSandbox(ctx, &setecgrpcv1.RestoreSandboxRequest{
		SnapshotId:       "ns-sb-ckpt-1",
		StorageRef:       resp.GetStorageRef(),
		StorageBackend:   "s3",
		KataSocketTarget: "/tmp/target-sock",
		SessionKek:       testKEK(9),
	})
	if status.Code(err) != codes.DataLoss {
		t.Fatalf("wrong-KEK restore = %v, want DataLoss", err)
	}

	// Delete routes to the s3 backend without needing the KEK.
	dresp, err := srv.DeleteSnapshot(ctx, &setecgrpcv1.DeleteSnapshotRequest{
		SnapshotId:     "ns-sb-ckpt-1",
		StorageRef:     resp.GetStorageRef(),
		StorageBackend: "s3",
	})
	if err != nil || !dresp.GetSuccess() {
		t.Fatalf("DeleteSnapshot = (%v,%v), want success", dresp, err)
	}
	_, err = srv.RestoreSandbox(ctx, &setecgrpcv1.RestoreSandboxRequest{
		SnapshotId:       "ns-sb-ckpt-1",
		StorageRef:       resp.GetStorageRef(),
		StorageBackend:   "s3",
		KataSocketTarget: "/tmp/target-sock",
		SessionKek:       kek,
	})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("restore after delete = %v, want NotFound", err)
	}
}
