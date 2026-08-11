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

// KEKSource resolves the key-encryption key an EncryptedBackend seals
// per-snapshot DEKs with. Two implementations exist and they define
// the two sealing domains of ADR-0005 invariant 5:
//
//   - FileKEKSource — the NODE-LOCAL keyfile. Pool entries and
//     local-disk snapshots never leave the node, so a node-scoped KEK
//     is the right (and smallest) trust domain.
//   - StaticKEKSource — a caller-provided key. Session checkpoints
//     must be unsealable by WHICHEVER node resumes the session, so
//     their KEK is cluster-scoped: a per-session Kubernetes Secret the
//     operator creates at session start, hands to the node-agent over
//     the mTLS control channel per call, and deletes at session end
//     (crypto-erasing every checkpoint it sealed).
type KEKSource interface {
	// KEK returns the 32-byte key-encryption key.
	KEK(ctx context.Context) ([]byte, error)
}

// FileKEKSource loads (creating on first use) the node-local KEK
// file. The key is resolved once per process.
type FileKEKSource struct {
	// Path is the keyfile location (0600, created on first use).
	Path string

	once sync.Once
	kek  []byte
	err  error
}

// KEK implements KEKSource.
func (f *FileKEKSource) KEK(context.Context) ([]byte, error) {
	f.once.Do(func() {
		f.kek, f.err = atrest.LoadOrCreateKEK(f.Path)
	})
	return f.kek, f.err
}

// StaticKEKSource serves a caller-provided KEK (the per-session key
// read from a Kubernetes Secret and forwarded over mTLS). It holds
// the key in memory only.
type StaticKEKSource []byte

// KEK implements KEKSource.
func (s StaticKEKSource) KEK(context.Context) ([]byte, error) {
	if len(s) != atrest.KeySize {
		return nil, fmt.Errorf("storage: static KEK must be %d bytes, got %d", atrest.KeySize, len(s))
	}
	return s, nil
}

// SealedDEKStore persists the sealed (KEK-wrapped) per-snapshot DEKs.
// Destroying a sealed DEK is the cryptographic erasure of its
// snapshot, so Destroy is always invoked BEFORE ciphertext
// reclamation. A missing blob surfaces as os.ErrNotExist from Destroy
// (idempotent teardown) and ErrNotFound from Get (the artifact no
// longer exists from the caller's perspective).
type SealedDEKStore interface {
	// Put stores a sealed DEK under snapshotID. An existing blob is
	// never overwritten: Put returns ErrAlreadyExists instead.
	Put(ctx context.Context, snapshotID string, sealed []byte) error

	// Get returns the sealed DEK, or ErrNotFound when it was
	// destroyed or never created.
	Get(ctx context.Context, snapshotID string) ([]byte, error)

	// Destroy irrecoverably removes the sealed DEK. A blob that does
	// not exist returns os.ErrNotExist.
	Destroy(ctx context.Context, snapshotID string) error
}

// DirDEKStore keeps one sealed-DEK sidecar file per snapshot
// (<snapshotID>.dek, 0600) in a node-local directory that must NOT
// live inside the inner backend's artifact tree — a copy of the
// artifact tree must carry zero key material. Destroy zero-overwrites
// and fsyncs before unlinking.
type DirDEKStore struct {
	// Dir is created on first Put (0700).
	Dir string
}

// path renders the sidecar path for a snapshot.
func (d *DirDEKStore) path(snapshotID string) string {
	return filepath.Join(d.Dir, snapshotID+".dek")
}

// Put implements SealedDEKStore with O_EXCL create semantics.
func (d *DirDEKStore) Put(_ context.Context, snapshotID string, sealed []byte) error {
	if err := validateSnapshotID(snapshotID); err != nil {
		return err
	}
	if err := os.MkdirAll(d.Dir, 0o700); err != nil {
		return fmt.Errorf("storage: mkdir key dir: %w", err)
	}
	kf, err := os.OpenFile(d.path(snapshotID), os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return ErrAlreadyExists
		}
		return fmt.Errorf("storage: create sealed DEK: %w", err)
	}
	_, werr := kf.Write(sealed)
	if serr := kf.Sync(); werr == nil {
		werr = serr
	}
	if cerr := kf.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		_ = os.Remove(d.path(snapshotID))
		return fmt.Errorf("storage: write sealed DEK: %w", werr)
	}
	return nil
}

// Get implements SealedDEKStore.
func (d *DirDEKStore) Get(_ context.Context, snapshotID string) ([]byte, error) {
	if err := validateSnapshotID(snapshotID); err != nil {
		return nil, err
	}
	sealed, err := os.ReadFile(d.path(snapshotID))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("storage: read sealed DEK: %w", err)
	}
	return sealed, nil
}

// Destroy implements SealedDEKStore: zero-overwrite, fsync, unlink.
func (d *DirDEKStore) Destroy(_ context.Context, snapshotID string) error {
	if err := validateSnapshotID(snapshotID); err != nil {
		return err
	}
	return atrest.Shred(d.path(snapshotID))
}

// EncryptedBackend enforces encryption at rest (ADR-0005 invariant 5)
// in front of any inner StorageBackend. Every snapshot is encrypted
// with its own DEK; the DEK is sealed with the KEK the configured
// KEKSource serves and persisted through the SealedDEKStore.
//
// Two production compositions exist (see KEKSource): the node-local
// one (FileKEKSource + DirDEKStore over LocalDiskBackend) for pool
// entries and single-node snapshots, and the portable one
// (StaticKEKSource carrying a per-session KEK + the object store's
// DEK sidecar over S3Backend) for session checkpoints that must
// restore on a different node than the one that wrote them.
//
// Delete destroys the sealed DEK first and only then reclaims the
// ciphertext. Even when the inner backend's best-effort overwrite is
// defeated (copy-on-write filesystems, remote object stores), the
// artifact is cryptographically erased the moment its sealed DEK — or
// for session checkpoints, the per-session KEK Secret — is gone.
//
// This wrapper is the ONLY write path production wires (see
// cmd/node-agent): unencrypted snapshot persistence does not exist.
type EncryptedBackend struct {
	// Inner is the backend the ciphertext is stored in.
	Inner StorageBackend

	// KEK serves the key-encryption key DEKs are sealed with.
	KEK KEKSource

	// DEKs persists the sealed per-snapshot DEKs.
	DEKs SealedDEKStore
}

// EncryptedAtRest implements the AtRestReporter capability: every
// artifact this wrapper serves was written through the per-snapshot
// sealed-DEK path — there is no plaintext write path behind it. The
// node-agent reports it per restore so the operator-side invariant
// gate (ADR-0005) never has to infer encryption.
func (b *EncryptedBackend) EncryptedAtRest() bool { return true }

// dekAAD binds a sealed DEK to the snapshot it protects, so a sealed
// blob cannot be moved between snapshots.
func dekAAD(snapshotID string) string {
	return "setec-snapshot:" + snapshotID
}

// Save generates a fresh DEK, seals it into the DEK store, and
// streams the encrypted payload into the inner backend. On any inner
// failure the sealed DEK is destroyed so no orphan key material
// remains.
func (b *EncryptedBackend) Save(ctx context.Context, snapshotID string, state io.Reader) (int64, string, error) {
	if err := ctx.Err(); err != nil {
		return 0, "", err
	}
	if err := validateSnapshotID(snapshotID); err != nil {
		return 0, "", err
	}
	kek, err := b.KEK.KEK(ctx)
	if err != nil {
		return 0, "", err
	}

	dek, err := atrest.NewDEK()
	if err != nil {
		return 0, "", err
	}
	sealed, err := atrest.SealDEK(kek, dek, dekAAD(snapshotID))
	if err != nil {
		return 0, "", err
	}
	// A sealed DEK already present means the snapshot exists (or a
	// concurrent Save is racing us) — never overwrite key material.
	if err := b.DEKs.Put(ctx, snapshotID, sealed); err != nil {
		return 0, "", err
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
		if shredErr := b.DEKs.Destroy(ctx, snapshotID); shredErr != nil && !errors.Is(shredErr, os.ErrNotExist) {
			return 0, "", fmt.Errorf("storage: save failed (%w) and sealed DEK cleanup failed: %v", saveErr, shredErr)
		}
		return 0, "", saveErr
	}
	return size, ref, nil
}

// Open unseals the snapshot's DEK and returns a verifying, decrypting
// reader over the inner payload. A destroyed (or never-created)
// sealed DEK returns ErrNotFound — the artifact is cryptographically
// erased, so from the caller's perspective it no longer exists. A DEK
// or payload that fails authentication returns ErrCorrupted.
func (b *EncryptedBackend) Open(ctx context.Context, storageRef string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateSnapshotID(storageRef); err != nil {
		return nil, err
	}
	kek, err := b.KEK.KEK(ctx)
	if err != nil {
		return nil, err
	}
	sealed, err := b.DEKs.Get(ctx, storageRef)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, fmt.Errorf("%w (sealed DEK destroyed or never created)", ErrNotFound)
		}
		return nil, err
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
	keyErr := b.DEKs.Destroy(ctx, storageRef)
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

// Compile-time interface assertions.
var (
	_ StorageBackend = (*EncryptedBackend)(nil)
	_ KEKSource      = (*FileKEKSource)(nil)
	_ KEKSource      = (StaticKEKSource)(nil)
	_ SealedDEKStore = (*DirDEKStore)(nil)
)
