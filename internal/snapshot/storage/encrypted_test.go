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

package storage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/zeroroot-ai/setec/internal/snapshot/atrest"
)

// fullDiskStatfs simulates a filesystem at 100% usage so Save's
// fill-threshold check refuses the write.
func fullDiskStatfs(_ string, stat *syscall.Statfs_t) error {
	stat.Blocks = 1000
	stat.Bfree = 0
	return nil
}

// newEncryptedBackend assembles the production composition: the
// encrypted wrapper over a local-disk inner backend, with the KEK and
// sealed-DEK dir OUTSIDE the artifact root.
func newEncryptedBackend(t *testing.T) (*EncryptedBackend, string, string) {
	t.Helper()
	base := t.TempDir()
	root := filepath.Join(base, "snapshots")
	keys := filepath.Join(base, "keys")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	b := &EncryptedBackend{
		Inner:   &LocalDiskBackend{Root: root},
		KEKPath: filepath.Join(keys, "node.key"),
		KeyDir:  filepath.Join(keys, "dek"),
	}
	return b, root, keys
}

// grepTree reports whether needle occurs in any regular file under
// root.
func grepTree(t *testing.T, root string, needle []byte) bool {
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

var plaintextMarker = bytes.Repeat([]byte("SENSITIVE-GUEST-MEMORY-PATTERN-"), 64)

func TestEncryptedBackend_RoundtripAndCiphertextAtRest(t *testing.T) {
	b, root, keys := newEncryptedBackend(t)
	ctx := context.Background()

	size, ref, err := b.Save(ctx, "snap-1", bytes.NewReader(plaintextMarker))
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if ref != "snap-1" || size <= 0 {
		t.Fatalf("Save returned ref=%q size=%d", ref, size)
	}

	// The invariant itself: NOTHING under the artifact root or the key
	// dir contains the plaintext.
	if grepTree(t, root, plaintextMarker[:32]) {
		t.Fatal("artifact tree contains plaintext at rest")
	}
	if grepTree(t, keys, plaintextMarker[:32]) {
		t.Fatal("key dir contains plaintext at rest")
	}

	rc, err := b.Open(ctx, ref)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	got, err := io.ReadAll(rc)
	if cerr := rc.Close(); cerr != nil {
		t.Fatalf("Close: %v", cerr)
	}
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, plaintextMarker) {
		t.Fatal("roundtrip mismatch")
	}

	exists := false
	if _, exists, err = b.Stat(ctx, ref); err != nil || !exists {
		t.Fatalf("Stat: exists=%v err=%v", exists, err)
	}
}

func TestEncryptedBackend_UnreadableWithoutKey(t *testing.T) {
	b, _, _ := newEncryptedBackend(t)
	ctx := context.Background()
	if _, _, err := b.Save(ctx, "snap-1", bytes.NewReader(plaintextMarker)); err != nil {
		t.Fatalf("Save: %v", err)
	}
	// Destroy only the sealed DEK: the ciphertext remains on disk but
	// the artifact must be unreadable — cryptographically erased.
	if err := atrest.Shred(b.dekPath("snap-1")); err != nil {
		t.Fatalf("shred DEK: %v", err)
	}
	if _, err := b.Open(ctx, "snap-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Open without key must report ErrNotFound, got %v", err)
	}
	// Delete still reclaims the orphan ciphertext.
	if err := b.Delete(ctx, "snap-1"); err != nil {
		t.Fatalf("Delete after key destruction: %v", err)
	}
}

func TestEncryptedBackend_KeyIsPerSnapshot(t *testing.T) {
	b, _, _ := newEncryptedBackend(t)
	ctx := context.Background()
	if _, _, err := b.Save(ctx, "snap-a", bytes.NewReader(plaintextMarker)); err != nil {
		t.Fatalf("Save a: %v", err)
	}
	if _, _, err := b.Save(ctx, "snap-b", bytes.NewReader(plaintextMarker)); err != nil {
		t.Fatalf("Save b: %v", err)
	}
	ka, err := os.ReadFile(b.dekPath("snap-a"))
	if err != nil {
		t.Fatal(err)
	}
	kb, err := os.ReadFile(b.dekPath("snap-b"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(ka, kb) {
		t.Fatal("two snapshots share a sealed DEK; keys must be per-artifact")
	}
	// A sealed DEK is bound to its snapshot: grafting a's key onto b
	// must fail authentication, not decrypt b.
	if err := os.WriteFile(b.dekPath("snap-b"), ka, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Open(ctx, "snap-b"); !errors.Is(err, ErrCorrupted) {
		t.Fatalf("cross-grafted sealed DEK must fail with ErrCorrupted, got %v", err)
	}
}

func TestEncryptedBackend_DeleteDestroysArtifactAndKey(t *testing.T) {
	b, root, _ := newEncryptedBackend(t)
	ctx := context.Background()
	if _, _, err := b.Save(ctx, "snap-1", bytes.NewReader(plaintextMarker)); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := b.Delete(ctx, "snap-1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := os.Stat(b.dekPath("snap-1")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("sealed DEK survives Delete: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "snap-1")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("artifact dir survives Delete: %v", err)
	}
	if _, err := b.Open(ctx, "snap-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Open after Delete must report ErrNotFound, got %v", err)
	}
	// Idempotency: a second Delete reports ErrNotFound like the inner
	// backend does.
	if err := b.Delete(ctx, "snap-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second Delete: %v", err)
	}
}

func TestEncryptedBackend_DoubleSaveRefused(t *testing.T) {
	b, _, _ := newEncryptedBackend(t)
	ctx := context.Background()
	if _, _, err := b.Save(ctx, "snap-1", bytes.NewReader(plaintextMarker)); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, _, err := b.Save(ctx, "snap-1", bytes.NewReader(plaintextMarker)); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("double Save must return ErrAlreadyExists, got %v", err)
	}
}

func TestEncryptedBackend_FailedInnerSaveDestroysKey(t *testing.T) {
	b, _, _ := newEncryptedBackend(t)
	inner := b.Inner.(*LocalDiskBackend)
	inner.FillThreshold = 0.000001
	inner.StatfsFn = fullDiskStatfs
	ctx := context.Background()
	if _, _, err := b.Save(ctx, "snap-1", bytes.NewReader(plaintextMarker)); !errors.Is(err, ErrInsufficientStorage) {
		t.Fatalf("expected ErrInsufficientStorage, got %v", err)
	}
	if _, err := os.Stat(b.dekPath("snap-1")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("orphan sealed DEK left behind after failed Save: %v", err)
	}
}

func TestEncryptedBackend_LegacyPlaintextArtifactIsDeadButDeletable(t *testing.T) {
	// A pre-upgrade plaintext artifact has no sealed DEK: it must be
	// unreadable (there is no legacy read path — pools rebuild) but
	// still deletable so upgrades converge.
	b, _, _ := newEncryptedBackend(t)
	ctx := context.Background()
	if _, _, err := b.Inner.Save(ctx, "legacy", bytes.NewReader(plaintextMarker)); err != nil {
		t.Fatalf("inner Save: %v", err)
	}
	if _, err := b.Open(ctx, "legacy"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("legacy plaintext artifact must be unreadable (ErrNotFound), got %v", err)
	}
	if err := b.Delete(ctx, "legacy"); err != nil {
		t.Fatalf("legacy artifact must remain deletable: %v", err)
	}
}
