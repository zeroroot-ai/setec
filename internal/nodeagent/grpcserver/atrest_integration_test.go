// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package grpcserver

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	setecgrpcv1 "github.com/zeroroot-ai/setec/api/grpc/v1"
	"github.com/zeroroot-ai/setec/internal/firecracker"
	"github.com/zeroroot-ai/setec/internal/nodeagent/launchersandbox"
	"github.com/zeroroot-ai/setec/internal/podspec"
	"github.com/zeroroot-ai/setec/internal/snapshot/atrest"
	"github.com/zeroroot-ai/setec/internal/snapshot/storage"
)

// This file is the integration test docs/design/isolation.md invariant 5 gates on:
// driving the real node-agent RPC surface over the PRODUCTION storage
// composition (EncryptedBackend over LocalDiskBackend), it asserts a
// snapshot artifact is unreadable without its key and provably gone —
// artifact AND key — after teardown.

// guestSecret is the recognizable "sensitive guest memory" pattern the
// fake Firecracker writes into the snapshot.
var guestSecret = bytes.Repeat([]byte("INTEGRATION-GUEST-SECRET-"), 128)

// capturingFC writes guestSecret at CreateSnapshot.
type capturingFC struct {
	mu sync.Mutex
	// root is the work volume of the launcher Pod on the host. The fake
	// maps the paths that Firecracker sees in the Pod to it.
	root string
}

func (f *capturingFC) host(p string) string {
	return filepath.Join(f.root, strings.TrimPrefix(p, podspec.LauncherWorkMountPath))
}

func (f *capturingFC) Pause(context.Context) error  { return nil }
func (f *capturingFC) Resume(context.Context) error { return nil }
func (f *capturingFC) CreateSnapshot(_ context.Context, state, mem string) error {
	if err := os.WriteFile(f.host(state), []byte("STATE-HEADER"), 0o600); err != nil {
		return err
	}
	return os.WriteFile(f.host(mem), guestSecret, 0o600)
}

// LoadSnapshot is the call of the launcher, not of the node agent.
func (f *capturingFC) LoadSnapshot(context.Context, string, string) error { return nil }

// launcherReadingMemory plays the launcher of one restore: it waits for
// the staged marker, reads the staged memory and confirms the guest.
func launcherReadingMemory(t *testing.T, p launchersandbox.Paths) <-chan []byte {
	t.Helper()
	got := make(chan []byte, 1)
	go func() {
		for range 500 {
			if _, err := os.Stat(launchersandbox.HostPath(p, podspec.LauncherRestoreStaged)); err == nil {
				b, _ := os.ReadFile(launchersandbox.HostPath(p, podspec.LauncherRestoreMemory))
				got <- b
				raw, _ := json.Marshal(podspec.RestoreEvidence{EntropyReseeded: true, Uniquified: true, ClockSet: true})
				_ = os.WriteFile(launchersandbox.HostPath(p, podspec.LauncherRestoreEvidence), raw, 0o600)
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	return got
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
	fc := &capturingFC{root: filepath.Join(base, "fcroot")}
	if err := os.MkdirAll(fc.root, 0o700); err != nil {
		t.Fatal(err)
	}
	srv := &Server{
		Storage:            backend,
		FirecrackerFactory: func(_ string) firecracker.Client { return fc },
		Machines:           fakeMachine{root: fc.root},
	}
	ctx := context.Background()

	// 1. Create a snapshot whose guest memory holds a known secret.
	resp, err := srv.CreateSnapshot(ctx, &setecgrpcv1.CreateSnapshotRequest{
		SandboxId:    "ns/sb",
		SnapshotId:   "ns-snap",
		SourcePodUid: testPodUID,
	})
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}

	// 2. At rest, NOTHING durable contains the secret: not the
	// artifact tree, not the key material, not the temp dir.
	// fc.root holds the plaintext temp pair while Firecracker writes and
	// reads it (setec#19), so it must be empty of the secret afterwards.
	for _, dir := range []string{root, filepath.Join(base, "keys"), fc.root} {
		if grepDir(t, dir, guestSecret[:25]) {
			t.Fatalf("plaintext guest secret found at rest under %s", dir)
		}
	}

	// 3. The legitimate restore path still recovers the exact guest
	// memory (decryption through the sealed per-snapshot DEK).
	p, err := srv.Machines.Resolve(ctx, testPodUID)
	if err != nil {
		t.Fatal(err)
	}
	restored := launcherReadingMemory(t, p)
	rresp, err := srv.RestoreSandbox(ctx, &setecgrpcv1.RestoreSandboxRequest{
		SnapshotId:   "ns-snap",
		StorageRef:   resp.GetStorageRef(),
		TargetPodUid: testPodUID,
	})
	if err != nil || !rresp.GetSuccess() {
		t.Fatalf("RestoreSandbox: %v / %+v", err, rresp)
	}
	if !bytes.Equal(<-restored, guestSecret) {
		t.Fatal("restore did not recover the original guest memory")
	}
	_ = os.Remove(launchersandbox.HostPath(p, podspec.LauncherRestoreStaged))
	_ = os.Remove(launchersandbox.HostPath(p, podspec.LauncherRestoreEvidence))

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
		SnapshotId:   "ns-snap",
		StorageRef:   resp.GetStorageRef(),
		TargetPodUid: testPodUID,
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

// CreateDiffSnapshot takes the path of a full snapshot here.
func (f *capturingFC) CreateDiffSnapshot(ctx context.Context, state, mem string) error {
	return f.CreateSnapshot(ctx, state, mem)
}
