// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package storage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A Save that ends inside the write, for example when the node agent is
// stopped, leaves a partial artifact at the snapshot id: a payload with no
// digest sidecar, and a sealed DEK. A new Save at that id must remove the
// partial artifact and write the snapshot. It used to fail with
// ErrAlreadyExists at each try, and the partial artifact stayed (setec#317).

// leavePartialLocal makes the partial artifact of a Save that ended after
// the DEK was sealed and part of the payload was written.
func leavePartialLocal(t *testing.T, b *EncryptedBackend, id string) {
	t.Helper()
	if err := b.DEKs.Put(context.Background(), id, []byte("sealed-dek-of-a-dead-attempt")); err != nil {
		t.Fatalf("put the DEK of the attempt: %v", err)
	}
	inner := b.Inner.(*LocalDiskBackend)
	if err := os.MkdirAll(inner.snapshotDir(id), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(inner.statePath(id), []byte("half a payload"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestEncryptedBackend_SaveOverAPartialAttempt(t *testing.T) {
	b, _, _ := newEncryptedBackend(t)
	ctx := context.Background()
	leavePartialLocal(t, b, "snap-1")

	if _, _, err := b.Save(ctx, "snap-1", bytes.NewReader(plaintextMarker)); err != nil {
		t.Fatalf("Save over a partial attempt: %v", err)
	}
	rc, err := b.Open(ctx, "snap-1")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	got, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, plaintextMarker) {
		t.Fatal("Open returned other bytes than the second Save wrote")
	}
}

// TestLocalDiskBackend_RemovePartialKeepsACompleteSnapshot: a payload with
// its sidecar is a complete snapshot. It is never removed.
func TestLocalDiskBackend_RemovePartialKeepsACompleteSnapshot(t *testing.T) {
	b := &LocalDiskBackend{Root: t.TempDir()}
	ctx := context.Background()
	if _, _, err := b.Save(ctx, "snap-1", bytes.NewReader([]byte("complete"))); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := b.RemovePartial(ctx, "snap-1"); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("RemovePartial of a complete snapshot: want ErrAlreadyExists, got %v", err)
	}
	if _, err := os.Stat(b.statePath("snap-1")); err != nil {
		t.Fatalf("the complete snapshot is gone: %v", err)
	}
	if err := b.RemovePartial(ctx, "snap-none"); err != nil {
		t.Fatalf("RemovePartial with nothing there: %v", err)
	}
	if _, err := os.Stat(filepath.Join(b.Root, "snap-none")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("RemovePartial made a directory: %v", err)
	}
}

// blockingReader returns its bytes only after release is closed.
type blockingReader struct {
	started chan struct{}
	release chan struct{}
	r       io.Reader
	once    bool
}

func (b *blockingReader) Read(p []byte) (int, error) {
	if !b.once {
		b.once = true
		close(b.started)
		<-b.release
	}
	return b.r.Read(p)
}

// TestEncryptedBackend_SaveDoesNotRemoveASaveInFlight: in one process, a
// Save of an id that another Save writes now is refused. Its files are not
// partial, they are in flight.
func TestEncryptedBackend_SaveDoesNotRemoveASaveInFlight(t *testing.T) {
	b, _, _ := newEncryptedBackend(t)
	ctx := context.Background()
	first := &blockingReader{started: make(chan struct{}), release: make(chan struct{}), r: bytes.NewReader(plaintextMarker)}
	done := make(chan error, 1)
	go func() {
		_, _, err := b.Save(ctx, "snap-1", first)
		done <- err
	}()
	select {
	case <-first.started:
	case <-time.After(10 * time.Second):
		t.Fatal("the first Save did not start")
	}

	if _, _, err := b.Save(ctx, "snap-1", bytes.NewReader([]byte("other"))); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("Save during a Save of the same id: want ErrAlreadyExists, got %v", err)
	}
	close(first.release)
	if err := <-done; err != nil {
		t.Fatalf("the first Save: %v", err)
	}
	rc, err := b.Open(ctx, "snap-1")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	if !bytes.Equal(got, plaintextMarker) {
		t.Fatal("the first Save did not keep its bytes")
	}
}

func TestS3EncryptedBackend_SaveOverAPartialAttempt(t *testing.T) {
	ctx := context.Background()
	inner, fake := newTestS3Backend(t)
	b := &EncryptedBackend{Inner: inner, KEK: StaticKEKSource(bytes.Repeat([]byte{0x42}, 32)), DEKs: inner.DEKStore()}

	// The attempt sealed its DEK and uploaded the payload, and ended before
	// the digest sidecar.
	if err := b.DEKs.Put(ctx, "sess-ckpt-1", []byte("sealed-dek-of-a-dead-attempt")); err != nil {
		t.Fatalf("put the DEK of the attempt: %v", err)
	}
	fake.mu.Lock()
	fake.objects["checkpoints/"+inner.stateKey("sess-ckpt-1")] = []byte("half a payload")
	fake.mu.Unlock()

	if _, _, err := b.Save(ctx, "sess-ckpt-1", bytes.NewReader(plaintextMarker)); err != nil {
		t.Fatalf("Save over a partial attempt: %v", err)
	}
	rc, err := b.Open(ctx, "sess-ckpt-1")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	if !bytes.Equal(got, plaintextMarker) {
		t.Fatal("Open returned other bytes than the second Save wrote")
	}
	if _, _, err := b.Save(ctx, "sess-ckpt-1", bytes.NewReader(plaintextMarker)); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("a second Save of a complete snapshot: want ErrAlreadyExists, got %v", err)
	}
}
