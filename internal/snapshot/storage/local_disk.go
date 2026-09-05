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
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// LocalDiskBackend persists snapshot state files under a per-node
// directory tree. Each snapshot occupies ROOT/<snapshotID>/ and
// consists of a state.bin (the concatenated Firecracker state+memory
// payload framed by the node-agent gRPC layer) plus a state.bin.sha256
// hex digest written alongside for integrity verification.
//
// The backend never invents a wrapper format for state.bin: bytes are
// written and read verbatim. The framing that splits state from memory
// is the node-agent layer's responsibility (see
// internal/nodeagent/grpcserver). The backend is purely "an integrity-
// checked opaque blob store with secure erase".
type LocalDiskBackend struct {
	// Root is the directory all snapshots live under. Must exist and
	// be writable by the node-agent runtime user (0700 recommended at
	// the directory level; state files are 0600 regardless).
	Root string

	// FillThreshold is the fraction of free space below which Save
	// refuses new writes with ErrInsufficientStorage. 0 disables the
	// check. A typical value is 0.85 (refuse when used > 85%).
	FillThreshold float64

	// StatfsFn, if non-nil, overrides the default syscall.Statfs call
	// used by Save's fill-threshold check. Exposed for unit tests so
	// they can drive the "disk full" branch without artificially
	// filling the tempdir.
	StatfsFn func(path string, stat *syscall.Statfs_t) error
}

// statePath returns the canonical state.bin path for a given
// snapshotID under Root.
func (b *LocalDiskBackend) statePath(snapshotID string) string {
	return filepath.Join(b.Root, snapshotID, "state.bin")
}

// sha256Path returns the canonical sha256 sidecar path.
func (b *LocalDiskBackend) sha256Path(snapshotID string) string {
	return filepath.Join(b.Root, snapshotID, "state.bin.sha256")
}

// snapshotDir returns the per-snapshot directory.
func (b *LocalDiskBackend) snapshotDir(snapshotID string) string {
	return filepath.Join(b.Root, snapshotID)
}

// statfs wraps the optional test hook.
func (b *LocalDiskBackend) statfs(path string, stat *syscall.Statfs_t) error {
	if b.StatfsFn != nil {
		return b.StatfsFn(path, stat)
	}
	return syscall.Statfs(path, stat)
}

// validateSnapshotID rejects obviously-unsafe identifiers to keep the
// local-disk layout predictable and prevent path traversal attacks.
func validateSnapshotID(id string) error {
	if id == "" {
		return errors.New("storage: snapshotID must be non-empty")
	}
	if strings.ContainsAny(id, "/\\") {
		return fmt.Errorf("storage: snapshotID %q must not contain path separators", id)
	}
	if strings.Contains(id, "..") {
		return fmt.Errorf("storage: snapshotID %q must not contain '..'", id)
	}
	return nil
}

// checkFillThreshold consults Statfs on Root and returns
// ErrInsufficientStorage when usage is above FillThreshold.
// FillThreshold == 0 disables the check.
func (b *LocalDiskBackend) checkFillThreshold() error {
	if b.FillThreshold <= 0 {
		return nil
	}
	var stat syscall.Statfs_t
	if err := b.statfs(b.Root, &stat); err != nil {
		return fmt.Errorf("storage: statfs %s: %w", b.Root, err)
	}
	// Statfs returns counts in fundamental block units. Guard against
	// a bogus zero-block filesystem before dividing.
	if stat.Blocks == 0 {
		return nil
	}
	used := stat.Blocks - stat.Bfree
	usedFrac := float64(used) / float64(stat.Blocks)
	if usedFrac >= b.FillThreshold {
		return ErrInsufficientStorage
	}
	return nil
}

// Save consumes state, streams it to ROOT/<snapshotID>/state.bin with
// mode 0600, and writes a hex SHA256 digest alongside. Double-save
// against an existing snapshotID returns ErrAlreadyExists.
func (b *LocalDiskBackend) Save(ctx context.Context, snapshotID string, state io.Reader) (int64, string, error) {
	if err := ctx.Err(); err != nil {
		return 0, "", err
	}
	if err := validateSnapshotID(snapshotID); err != nil {
		return 0, "", err
	}
	if err := b.checkFillThreshold(); err != nil {
		return 0, "", err
	}

	dir := b.snapshotDir(snapshotID)
	if _, err := os.Stat(dir); err == nil {
		return 0, "", ErrAlreadyExists
	} else if !errors.Is(err, os.ErrNotExist) {
		return 0, "", fmt.Errorf("storage: stat %s: %w", dir, err)
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return 0, "", fmt.Errorf("storage: mkdir %s: %w", dir, err)
	}

	statePath := b.statePath(snapshotID)
	// O_EXCL guards against concurrent Save of the same ID racing
	// past the os.Stat check above.
	f, err := os.OpenFile(statePath, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return 0, "", ErrAlreadyExists
		}
		return 0, "", fmt.Errorf("storage: open %s: %w", statePath, err)
	}

	hasher := sha256.New()
	// TeeReader is simpler than a custom Writer fan-out and keeps the
	// hashing in-line with the disk write — no double read.
	size, copyErr := io.Copy(f, io.TeeReader(state, hasher))
	if closeErr := f.Close(); closeErr != nil && copyErr == nil {
		copyErr = closeErr
	}
	if copyErr != nil {
		_ = os.RemoveAll(dir)
		return 0, "", fmt.Errorf("storage: write %s: %w", statePath, copyErr)
	}

	digest := hex.EncodeToString(hasher.Sum(nil))
	shaPath := b.sha256Path(snapshotID)
	if err := os.WriteFile(shaPath, []byte(digest), 0o600); err != nil {
		_ = os.RemoveAll(dir)
		return 0, "", fmt.Errorf("storage: write sha256 sidecar: %w", err)
	}

	return size, snapshotID, nil
}

// Open returns a reader over the persisted state after verifying the
// SHA256 sidecar matches the on-disk contents. A mismatch returns
// ErrCorrupted; a missing snapshot returns ErrNotFound.
func (b *LocalDiskBackend) Open(ctx context.Context, storageRef string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateSnapshotID(storageRef); err != nil {
		return nil, err
	}
	statePath := b.statePath(storageRef)
	shaPath := b.sha256Path(storageRef)

	expected, err := os.ReadFile(shaPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("storage: read sha256 sidecar: %w", err)
	}

	f, err := os.Open(statePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("storage: open %s: %w", statePath, err)
	}

	// Verify first (simpler than streaming verification at the
	// cost of one extra pass; Phase 3 snapshots are sized for this).
	hasher := sha256.New()
	if _, err := io.Copy(hasher, f); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("storage: hash %s: %w", statePath, err)
	}
	got := hex.EncodeToString(hasher.Sum(nil))
	if got != strings.TrimSpace(string(expected)) {
		_ = f.Close()
		return nil, ErrCorrupted
	}

	// Seek back to the start so the caller reads the payload, not
	// an EOF.
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("storage: seek %s: %w", statePath, err)
	}
	return f, nil
}

// Delete overwrites state.bin with zeros (one pass), fsyncs, then
// unlinks the file, the sha256 sidecar, and the enclosing directory.
// A missing snapshot returns ErrNotFound rather than succeeding
// silently so callers can distinguish "already gone" from "just
// deleted".
func (b *LocalDiskBackend) Delete(ctx context.Context, storageRef string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateSnapshotID(storageRef); err != nil {
		return err
	}

	dir := b.snapshotDir(storageRef)
	statePath := b.statePath(storageRef)
	shaPath := b.sha256Path(storageRef)

	info, err := os.Stat(statePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ErrNotFound
		}
		return fmt.Errorf("storage: stat %s: %w", statePath, err)
	}

	if err := overwriteWithZeros(statePath, info.Size()); err != nil {
		return fmt.Errorf("storage: secure overwrite: %w", err)
	}

	if err := os.Remove(statePath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("storage: unlink %s: %w", statePath, err)
	}
	if err := os.Remove(shaPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("storage: unlink %s: %w", shaPath, err)
	}
	if err := os.Remove(dir); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("storage: rmdir %s: %w", dir, err)
	}
	return nil
}

// overwriteWithZeros is the pragmatic secure-erase step. It is NOT
// cryptographic erasure; it is a single-pass zero-write followed by
// an fsync, which defeats naive post-unlink readback on commodity
// block devices. Underlying filesystem COW (e.g. btrfs, zfs) may
// short-circuit this; operators who care deploy encrypted root.
func overwriteWithZeros(path string, size int64) error {
	if size <= 0 {
		return nil
	}
	f, err := os.OpenFile(path, os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	buf := make([]byte, 64*1024)
	var written int64
	for written < size {
		chunk := min(size-written, int64(len(buf)))
		n, werr := f.Write(buf[:chunk])
		if werr != nil {
			return werr
		}
		written += int64(n)
	}
	return f.Sync()
}

// Stat returns the size and existence of the persisted state.
// A non-existent snapshot returns (0, false, nil).
func (b *LocalDiskBackend) Stat(ctx context.Context, storageRef string) (int64, bool, error) {
	if err := ctx.Err(); err != nil {
		return 0, false, err
	}
	if err := validateSnapshotID(storageRef); err != nil {
		return 0, false, err
	}
	info, err := os.Stat(b.statePath(storageRef))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("storage: stat: %w", err)
	}
	return info.Size(), true, nil
}

// Compile-time assertion that LocalDiskBackend satisfies the
// StorageBackend interface.
var _ StorageBackend = (*LocalDiskBackend)(nil)
