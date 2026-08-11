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
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"

	setecgrpcv1 "github.com/zeroroot-ai/setec/api/grpc/v1"
	"github.com/zeroroot-ai/setec/internal/firecracker"
	"github.com/zeroroot-ai/setec/internal/snapshot/atrest"
	"github.com/zeroroot-ai/setec/internal/snapshot/storage"
)

// This file is the integration test ADR-0005 invariant 5 gates on:
// driving the real node-agent RPC surface over the PRODUCTION storage
// composition (EncryptedBackend over LocalDiskBackend), it asserts a
// snapshot artifact is unreadable without its key and provably gone —
// artifact AND key — after teardown.

// guestSecret is the recognisable "sensitive guest memory" pattern the
// fake Firecracker writes into the snapshot.
var guestSecret = bytes.Repeat([]byte("INTEGRATION-GUEST-SECRET-"), 128)

// capturingFC writes guestSecret at CreateSnapshot and captures the
// plaintext it is handed back at LoadSnapshot.
type capturingFC struct {
	mu       sync.Mutex
	restored []byte
}

func (f *capturingFC) Pause(context.Context) error  { return nil }
func (f *capturingFC) Resume(context.Context) error { return nil }
func (f *capturingFC) CreateSnapshot(_ context.Context, state, mem string) error {
	if err := os.WriteFile(state, []byte("STATE-HEADER"), 0o600); err != nil {
		return err
	}
	return os.WriteFile(mem, guestSecret, 0o600)
}
func (f *capturingFC) LoadSnapshot(_ context.Context, _, mem string) error {
	b, err := os.ReadFile(mem)
	if err != nil {
		return err
	}
	f.mu.Lock()
	f.restored = b
	f.mu.Unlock()
	return nil
}

// grepDir reports whether needle occurs in any regular file under
// root.
func grepDir(t *testing.T, root string, needle []byte) bool {
	t.Helper()
	found := false
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		if bytes.Contains(b, needle) {
			found = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return found
}

func TestSnapshotAtRest_UnreadableWithoutKeyAndGoneAfterTeardown(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "snapshots")
	keyDir := filepath.Join(base, "keys", "dek")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	backend := &storage.EncryptedBackend{
		Inner: &storage.LocalDiskBackend{Root: root},
		KEK:   &storage.FileKEKSource{Path: filepath.Join(base, "keys", "node.key")},
		DEKs:  &storage.DirDEKStore{Dir: keyDir},
	}
	fc := &capturingFC{}
	srv := &Server{
		Storage:            backend,
		FirecrackerFactory: func(_ string) firecracker.Client { return fc },
		TempDir:            filepath.Join(base, "tmp"),
	}
	ctx := context.Background()

	// 1. Create a snapshot whose guest memory holds a known secret.
	resp, err := srv.CreateSnapshot(ctx, &setecgrpcv1.CreateSnapshotRequest{
		SandboxId:        "ns/sb",
		SnapshotId:       "ns-snap",
		SourceKataSocket: "/run/fake.socket",
	})
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}

	// 2. At rest, NOTHING durable contains the secret: not the
	// artifact tree, not the key material, not the temp dir.
	for _, dir := range []string{root, filepath.Join(base, "keys"), filepath.Join(base, "tmp")} {
		if grepDir(t, dir, guestSecret[:25]) {
			t.Fatalf("plaintext guest secret found at rest under %s", dir)
		}
	}

	// 3. The legitimate restore path still recovers the exact guest
	// memory (decryption through the sealed per-snapshot DEK).
	rresp, err := srv.RestoreSandbox(ctx, &setecgrpcv1.RestoreSandboxRequest{
		SnapshotId:       "ns-snap",
		StorageRef:       resp.GetStorageRef(),
		KataSocketTarget: "/run/fake-target.socket",
	})
	if err != nil || !rresp.GetSuccess() {
		t.Fatalf("RestoreSandbox: %v / %+v", err, rresp)
	}
	if !bytes.Equal(fc.restored, guestSecret) {
		t.Fatal("restore did not recover the original guest memory")
	}

	// 4. Destroy ONLY the key: the ciphertext is still on disk, but
	// the artifact must be unreadable — cryptographically erased.
	dekFiles, err := filepath.Glob(filepath.Join(keyDir, "*.dek"))
	if err != nil || len(dekFiles) != 1 {
		t.Fatalf("expected exactly one sealed DEK, got %v (%v)", dekFiles, err)
	}
	if err := atrest.Shred(dekFiles[0]); err != nil {
		t.Fatalf("shred sealed DEK: %v", err)
	}
	if _, err := srv.RestoreSandbox(ctx, &setecgrpcv1.RestoreSandboxRequest{
		SnapshotId:       "ns-snap",
		StorageRef:       resp.GetStorageRef(),
		KataSocketTarget: "/run/fake-target.socket",
	}); err == nil {
		t.Fatal("restore must fail once the snapshot's key is destroyed")
	}

	// 5. Teardown: DeleteSnapshot removes artifact AND key. (The key
	// is already gone; delete must still reclaim the ciphertext.)
	dresp, err := srv.DeleteSnapshot(ctx, &setecgrpcv1.DeleteSnapshotRequest{
		SnapshotId: "ns-snap",
		StorageRef: resp.GetStorageRef(),
	})
	if err != nil || !dresp.GetSuccess() {
		t.Fatalf("DeleteSnapshot: %v / %+v", err, dresp)
	}
	if entries, _ := os.ReadDir(root); len(entries) != 0 {
		t.Fatalf("artifact tree not empty after teardown: %v", entries)
	}
	if entries, _ := os.ReadDir(keyDir); len(entries) != 0 {
		t.Fatalf("key dir not empty after teardown: %v", entries)
	}

	// 6. Idempotent teardown keeps reporting success.
	dresp, err = srv.DeleteSnapshot(ctx, &setecgrpcv1.DeleteSnapshotRequest{
		SnapshotId: "ns-snap",
		StorageRef: resp.GetStorageRef(),
	})
	if err != nil || !dresp.GetSuccess() {
		t.Fatalf("repeat DeleteSnapshot: %v / %+v", err, dresp)
	}
}
