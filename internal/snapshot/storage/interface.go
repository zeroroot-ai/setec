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

// Package storage declares the StorageBackend interface Phase 3
// snapshot/restore is built on, plus a set of typed sentinel errors
// callers can wrap checks around. The package intentionally depends
// only on the standard library so every consumer (operator,
// node-agent, tests) can import it cheaply. No Kubernetes types are
// referenced here.
package storage

import (
	"context"
	"errors"
	"io"
)

// Common sentinel errors. Callers should prefer errors.Is checks
// against these values over string-matching.
var (
	// ErrNotFound is returned by Open/Stat/Delete when the requested
	// storageRef does not exist.
	ErrNotFound = errors.New("storage: not found")

	// ErrCorrupted is returned by Open when the persisted SHA256 does
	// not match the state file on disk. The caller should treat this
	// as a restore-blocking condition and surface a RestoreFailed
	// reason on the parent Sandbox.
	ErrCorrupted = errors.New("storage: corrupted state (sha256 mismatch)")

	// ErrInsufficientStorage is returned by Save when the backend's
	// fill-threshold check refuses the write. Callers MUST NOT pause
	// the source VM in this case — the snapshot operation is aborted
	// before any mutation of the source.
	ErrInsufficientStorage = errors.New("storage: insufficient free space")

	// ErrAlreadyExists is returned by Save when a storageRef with the
	// given snapshotID already exists. Snapshot creation is expected
	// to be idempotent at the Coordinator level, so the node-agent
	// returns this so the Coordinator can decide whether to reuse the
	// existing ref or fail fast.
	ErrAlreadyExists = errors.New("storage: snapshot already exists")
)

// StorageBackend abstracts the on-node persistence of microVM state
// files. Implementations operate on opaque byte streams keyed by a
// backend-scoped reference; the caller never interprets the ref's
// internal structure.
//
// Phase 3 ships a single implementation (LocalDiskBackend). The
// interface is shaped so future phases can add object-store or
// content-addressable backends without touching the controller or
// node-agent glue code.
type StorageBackend interface {
	// Save persists the incoming state stream under the given
	// snapshotID and returns the resulting size (bytes read from the
	// stream) and the opaque storageRef the caller stores in the
	// Snapshot CR. snapshotID MUST be stable across retries (the
	// Coordinator uses the Snapshot CR's namespace/name).
	Save(ctx context.Context, snapshotID string, state io.Reader) (size int64, storageRef string, err error)

	// Open returns a reader over the previously-persisted state. The
	// implementation is responsible for integrity verification (e.g.
	// SHA256) before the first byte is returned; a corrupt payload
	// surfaces as ErrCorrupted.
	Open(ctx context.Context, storageRef string) (io.ReadCloser, error)

	// Delete reclaims the state files associated with storageRef.
	// Implementations SHOULD overwrite the state contents before
	// unlinking so residual bytes are not trivially recoverable from
	// the underlying block device.
	Delete(ctx context.Context, storageRef string) error

	// Stat returns the size of the persisted state and a boolean
	// indicating existence. A non-existent ref returns (0, false,
	// nil) rather than an error.
	Stat(ctx context.Context, storageRef string) (size int64, exists bool, err error)
}

// AtRestReporter is the optional capability a StorageBackend
// implements to attest that every artifact it serves is encrypted at
// rest (ADR-0005 invariant 5). The node-agent consults it per restore
// to populate the encrypted_at_rest response signal the operator-side
// invariant gate verifies. A backend that does not implement the
// interface is treated as unencrypted — the signal is never inferred.
type AtRestReporter interface {
	// EncryptedAtRest reports whether the backend persists and serves
	// state exclusively through an encrypted path.
	EncryptedAtRest() bool
}

// IsEncryptedAtRest reports whether the given backend attests
// encryption at rest via the AtRestReporter capability.
func IsEncryptedAtRest(b StorageBackend) bool {
	r, ok := b.(AtRestReporter)
	return ok && r.EncryptedAtRest()
}
