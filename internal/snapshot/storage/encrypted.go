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
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/zeroroot-ai/setec/internal/snapshot/atrest"
)

// EncryptedBackend enforces encryption at rest (ADR-0005 invariant 5)
// in front of any inner StorageBackend. Every snapshot is encrypted
// with its own DEK; the DEK is sealed with the node-local KEK and
// stored as a sidecar in KeyDir — deliberately OUTSIDE the inner
// backend, so a copy of the artifact tree (a backup, an object-store
// replica, a reclaimed disk) carries zero key material.
//
// Delete destroys the sealed DEK first — zero-overwrite, fsync,
// unlink — and only then reclaims the ciphertext. Even when the inner
// backend's best-effort overwrite is defeated (copy-on-write
// filesystems, remote object stores), the artifact is cryptographically
// erased the moment its sealed DEK is gone.
//
// This wrapper is the ONLY write path production wires (see
// cmd/node-agent): unencrypted snapshot persistence does not exist.
type EncryptedBackend struct {
	// Inner is the backend the ciphertext is stored in.
	Inner StorageBackend

	// KEKPath is the node-local key-encryption-key file, created on
	// first use (0600).
	KEKPath string

	// KeyDir holds one sealed-DEK sidecar per snapshot
	// (<snapshotID>.dek, 0600). Created on first use (0700). Must NOT
	// live inside the inner backend's artifact tree.
	KeyDir string

	kekOnce sync.Once
	kek     []byte
	kekErr  error
}

// loadKEK resolves the node KEK exactly once per process.
func (b *EncryptedBackend) loadKEK() ([]byte, error) {
	b.kekOnce.Do(func() {
		b.kek, b.kekErr = atrest.LoadOrCreateKEK(b.KEKPath)
	})
	return b.kek, b.kekErr
}

// dekPath renders the sealed-DEK sidecar path for a snapshot.
func (b *EncryptedBackend) dekPath(snapshotID string) string {
	return filepath.Join(b.KeyDir, snapshotID+".dek")
}

// dekAAD binds a sealed DEK to the snapshot it protects, so a sidecar
// cannot be moved between snapshots.
func dekAAD(snapshotID string) string {
	return "setec-snapshot:" + snapshotID
}

// Save generates a fresh DEK, seals it into KeyDir, and streams the
// encrypted payload into the inner backend. On any inner failure the
// sealed DEK is destroyed so no orphan key material remains.
func (b *EncryptedBackend) Save(ctx context.Context, snapshotID string, state io.Reader) (int64, string, error) {
	if err := ctx.Err(); err != nil {
		return 0, "", err
	}
	if err := validateSnapshotID(snapshotID); err != nil {
		return 0, "", err
	}
	kek, err := b.loadKEK()
	if err != nil {
		return 0, "", err
	}
	if err := os.MkdirAll(b.KeyDir, 0o700); err != nil {
		return 0, "", fmt.Errorf("storage: mkdir key dir: %w", err)
	}

	dek, err := atrest.NewDEK()
	if err != nil {
		return 0, "", err
	}
	sealed, err := atrest.SealDEK(kek, dek, dekAAD(snapshotID))
	if err != nil {
		return 0, "", err
	}
	// O_EXCL mirrors the inner backend's double-save guard: a sealed
	// DEK already present means the snapshot exists (or a concurrent
	// Save is racing us) — never overwrite key material.
	kf, err := os.OpenFile(b.dekPath(snapshotID), os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return 0, "", ErrAlreadyExists
		}
		return 0, "", fmt.Errorf("storage: create sealed DEK: %w", err)
	}
	_, werr := kf.Write(sealed)
	if serr := kf.Sync(); werr == nil {
		werr = serr
	}
	if cerr := kf.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		_ = os.Remove(b.dekPath(snapshotID))
		return 0, "", fmt.Errorf("storage: write sealed DEK: %w", werr)
	}

	pr, pw := io.Pipe()
	go func() {
		_, encErr := atrest.Encrypt(pw, state, dek)
		_ = pw.CloseWithError(encErr)
	}()

	size, ref, saveErr := b.Inner.Save(ctx, snapshotID, pr)
	if saveErr != nil {
		_ = pr.CloseWithError(saveErr)
		// The ciphertext never landed; destroy the orphan key.
		if shredErr := atrest.Shred(b.dekPath(snapshotID)); shredErr != nil && !errors.Is(shredErr, os.ErrNotExist) {
			return 0, "", fmt.Errorf("storage: save failed (%w) and sealed DEK cleanup failed: %v", saveErr, shredErr)
		}
		return 0, "", saveErr
	}
	return size, ref, nil
}

// Open unseals the snapshot's DEK and returns a verifying, decrypting
// reader over the inner payload. A destroyed (or never-created) sealed
// DEK returns ErrNotFound — the artifact is cryptographically erased,
// so from the caller's perspective it no longer exists. A DEK or
// payload that fails authentication returns ErrCorrupted.
func (b *EncryptedBackend) Open(ctx context.Context, storageRef string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateSnapshotID(storageRef); err != nil {
		return nil, err
	}
	kek, err := b.loadKEK()
	if err != nil {
		return nil, err
	}
	sealed, err := os.ReadFile(b.dekPath(storageRef))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w (sealed DEK destroyed or never created)", ErrNotFound)
		}
		return nil, fmt.Errorf("storage: read sealed DEK: %w", err)
	}
	dek, err := atrest.OpenDEK(kek, sealed, dekAAD(storageRef))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCorrupted, err)
	}

	rc, err := b.Inner.Open(ctx, storageRef)
	if err != nil {
		return nil, err
	}
	dr, err := atrest.NewDecryptingReader(rc, dek)
	if err != nil {
		_ = rc.Close()
		if errors.Is(err, atrest.ErrDecrypt) {
			return nil, fmt.Errorf("%w: %v", ErrCorrupted, err)
		}
		return nil, err
	}
	return &decryptReadCloser{Reader: dr, closer: rc}, nil
}

// Delete destroys the sealed DEK FIRST (the cryptographic erasure),
// then delegates ciphertext reclamation to the inner backend. A
// snapshot with neither key nor ciphertext returns ErrNotFound so the
// caller's idempotency contract is preserved.
func (b *EncryptedBackend) Delete(ctx context.Context, storageRef string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateSnapshotID(storageRef); err != nil {
		return err
	}
	keyErr := atrest.Shred(b.dekPath(storageRef))
	if keyErr != nil && !errors.Is(keyErr, os.ErrNotExist) {
		return fmt.Errorf("storage: destroy sealed DEK: %w", keyErr)
	}
	innerErr := b.Inner.Delete(ctx, storageRef)
	if errors.Is(innerErr, ErrNotFound) && !errors.Is(keyErr, os.ErrNotExist) {
		// The key existed and is now destroyed; the artifact was
		// already gone. That is a successful teardown.
		return nil
	}
	return innerErr
}

// Stat delegates to the inner backend; the reported size is the
// stored (ciphertext) size, consistent with what Save returned.
func (b *EncryptedBackend) Stat(ctx context.Context, storageRef string) (int64, bool, error) {
	return b.Inner.Stat(ctx, storageRef)
}

// decryptReadCloser pairs the decrypting reader with the inner
// payload's closer.
type decryptReadCloser struct {
	io.Reader
	closer io.Closer
}

func (d *decryptReadCloser) Close() error { return d.closer.Close() }

// Compile-time interface assertion.
var _ StorageBackend = (*EncryptedBackend)(nil)
