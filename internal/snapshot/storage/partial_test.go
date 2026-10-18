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
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
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

// backdateLocal makes the partial artifact at id look written age ago.
func backdateLocal(t *testing.T, b *EncryptedBackend, id string, age time.Duration) {
	t.Helper()
	old := time.Now().Add(-age)
	inner := b.Inner.(*LocalDiskBackend)
	for _, p := range []string{b.DEKs.(*DirDEKStore).path(id), inner.statePath(id), inner.snapshotDir(id)} {
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatalf("backdate %s: %v", p, err)
		}
	}
}

func TestEncryptedBackend_SaveOverAPartialAttempt(t *testing.T) {
	b, _, _ := newEncryptedBackend(t)
	ctx := context.Background()
	leavePartialLocal(t, b, "snap-1")
	backdateLocal(t, b, "snap-1", 2*defaultPartialStaleAfter)

	if _, _, err := b.Save(ctx, "snap-1", bytes.NewReader(plaintextMarker)); err != nil {
		t.Fatalf("Save over a partial attempt: %v", err)
	}
	assertOpens(t, b, "snap-1", plaintextMarker)
}

// TestEncryptedBackend_SaveKeepsANewPartialArtifact: a partial artifact
// newer than PartialStaleAfter may be a Save in flight in another process
// or on another node. Save refuses the id and keeps the artifact.
func TestEncryptedBackend_SaveKeepsANewPartialArtifact(t *testing.T) {
	b, _, _ := newEncryptedBackend(t)
	ctx := context.Background()
	leavePartialLocal(t, b, "snap-1")

	if _, _, err := b.Save(ctx, "snap-1", bytes.NewReader(plaintextMarker)); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("Save over a new partial artifact: want ErrAlreadyExists, got %v", err)
	}
	if _, err := os.Stat(b.Inner.(*LocalDiskBackend).statePath("snap-1")); err != nil {
		t.Fatalf("the payload of the other Save is gone: %v", err)
	}
	if _, err := b.DEKs.Get(ctx, "snap-1"); err != nil {
		t.Fatalf("the sealed DEK of the other Save is gone: %v", err)
	}
}

// assertOpens checks that Open of id returns want.
func assertOpens(t *testing.T, b *EncryptedBackend, id string, want []byte) {
	t.Helper()
	rc, err := b.Open(context.Background(), id)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	got, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("Open returned other bytes than the last Save wrote")
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
	if err := b.RemovePartial(ctx, "snap-1", 0); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("RemovePartial of a complete snapshot: want ErrAlreadyExists, got %v", err)
	}
	if _, err := os.Stat(b.statePath("snap-1")); err != nil {
		t.Fatalf("the complete snapshot is gone: %v", err)
	}
	if err := b.RemovePartial(ctx, "snap-none", 0); err != nil {
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
	inner, fake := newVersionedTestBackend(t)
	b := &EncryptedBackend{
		Inner: inner, KEK: StaticKEKSource(bytes.Repeat([]byte{0x42}, 32)), DEKs: inner.DEKStore(),
	}
	leavePartialS3(t, b, inner, "sess-ckpt-1")
	fake.backdate(2*defaultPartialStaleAfter, inner.stateKey("sess-ckpt-1"), inner.dekKey("sess-ckpt-1"))

	if _, _, err := b.Save(ctx, "sess-ckpt-1", bytes.NewReader(plaintextMarker)); err != nil {
		t.Fatalf("Save over a partial attempt: %v", err)
	}
	assertOpens(t, b, "sess-ckpt-1", plaintextMarker)
	// Every version of the partial payload is gone: only the new one is left.
	if n := fake.liveVersionCount(inner.stateKey("sess-ckpt-1")); n != 1 {
		t.Fatalf("payload versions after the Save: got %d, want 1", n)
	}
	if _, _, err := b.Save(ctx, "sess-ckpt-1", bytes.NewReader(plaintextMarker)); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("a second Save of a complete snapshot: want ErrAlreadyExists, got %v", err)
	}
}

func TestS3EncryptedBackend_SaveKeepsANewPartialArtifact(t *testing.T) {
	ctx := context.Background()
	inner, fake := newVersionedTestBackend(t)
	b := &EncryptedBackend{
		Inner: inner, KEK: StaticKEKSource(bytes.Repeat([]byte{0x42}, 32)), DEKs: inner.DEKStore(),
		PartialStaleAfter: time.Hour,
	}
	leavePartialS3(t, b, inner, "sess-ckpt-1")

	if _, _, err := b.Save(ctx, "sess-ckpt-1", bytes.NewReader(plaintextMarker)); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("Save over a new partial artifact: want ErrAlreadyExists, got %v", err)
	}
	if n := fake.liveVersionCount(inner.stateKey("sess-ckpt-1")); n != 1 {
		t.Fatalf("the payload of the other Save: got %d versions, want 1", n)
	}
	if _, err := b.DEKs.Get(ctx, "sess-ckpt-1"); err != nil {
		t.Fatalf("the sealed DEK of the other Save is gone: %v", err)
	}
}

// leavePartialS3 makes the partial artifact of a Save that sealed its DEK
// and uploaded the payload, and ended before the digest sidecar.
func leavePartialS3(t *testing.T, b *EncryptedBackend, inner *S3Backend, id string) {
	t.Helper()
	ctx := context.Background()
	if err := b.DEKs.Put(ctx, id, []byte("sealed-dek-of-a-dead-attempt")); err != nil {
		t.Fatalf("put the DEK of the attempt: %v", err)
	}
	if _, err := inner.Client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(inner.Bucket), Key: aws.String(inner.stateKey(id)),
		Body: bytes.NewReader([]byte("half a payload")),
	}); err != nil {
		t.Fatalf("put the partial payload: %v", err)
	}
}

// TestS3EncryptedBackend_SaveKeepsANewDEKWithNoPayload: a Save in flight
// on another node has put its sealed DEK, and its multipart upload is not
// an object yet. Save refuses the id and keeps that DEK.
func TestS3EncryptedBackend_SaveKeepsANewDEKWithNoPayload(t *testing.T) {
	ctx := context.Background()
	inner, _ := newVersionedTestBackend(t)
	b := &EncryptedBackend{
		Inner: inner, KEK: StaticKEKSource(bytes.Repeat([]byte{0x42}, 32)), DEKs: inner.DEKStore(),
		PartialStaleAfter: time.Hour,
	}
	if err := b.DEKs.Put(ctx, "sess-ckpt-1", []byte("sealed-dek-of-a-save-in-flight")); err != nil {
		t.Fatalf("put the DEK: %v", err)
	}
	if _, _, err := b.Save(ctx, "sess-ckpt-1", bytes.NewReader(plaintextMarker)); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("Save over a new DEK: want ErrAlreadyExists, got %v", err)
	}
	if got, err := b.DEKs.Get(ctx, "sess-ckpt-1"); err != nil || string(got) != "sealed-dek-of-a-save-in-flight" {
		t.Fatalf("the DEK of the other Save: got %q, %v", got, err)
	}
}

// TestS3EncryptedBackend_SaveKeepsADEKOfUnknownAge: a store that does not
// send Last-Modified gives no age, and an artifact of no known age is kept.
func TestS3EncryptedBackend_SaveKeepsADEKOfUnknownAge(t *testing.T) {
	ctx := context.Background()
	inner, _ := newTestS3Backend(t) // this fake sends no Last-Modified
	b := &EncryptedBackend{
		Inner: inner, KEK: StaticKEKSource(bytes.Repeat([]byte{0x42}, 32)), DEKs: inner.DEKStore(),
		PartialStaleAfter: time.Nanosecond,
	}
	if err := b.DEKs.Put(ctx, "sess-ckpt-1", []byte("sealed-dek")); err != nil {
		t.Fatalf("put the DEK: %v", err)
	}
	_, _, err := b.Save(ctx, "sess-ckpt-1", bytes.NewReader(plaintextMarker))
	if !errors.Is(err, ErrAlreadyExists) || !strings.Contains(err.Error(), "unknown time") {
		t.Fatalf("Save over a DEK of unknown age: want ErrAlreadyExists at an unknown time, got %v", err)
	}
}

// slowReader returns its bytes only after delay.
type slowReader struct {
	delay time.Duration
	r     io.Reader
	slept bool
}

func (s *slowReader) Read(p []byte) (int, error) {
	if !s.slept {
		s.slept = true
		time.Sleep(s.delay)
	}
	return s.r.Read(p)
}

// TestS3EncryptedBackend_SaveEndsAtTheBound: no Save runs longer than
// PartialStaleAfter, because a Save on another node takes artifacts older
// than that for the remains of a dead Save. The Save fails, and its DEK
// goes.
func TestS3EncryptedBackend_SaveEndsAtTheBound(t *testing.T) {
	ctx := context.Background()
	inner, _ := newVersionedTestBackend(t)
	b := &EncryptedBackend{
		Inner: inner, KEK: StaticKEKSource(bytes.Repeat([]byte{0x42}, 32)), DEKs: inner.DEKStore(),
		PartialStaleAfter: 100 * time.Millisecond,
	}
	slow := &slowReader{delay: time.Second, r: bytes.NewReader(plaintextMarker)}
	if _, _, err := b.Save(ctx, "sess-ckpt-1", slow); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a Save longer than the bound: want DeadlineExceeded, got %v", err)
	}
	if _, err := b.DEKs.Get(ctx, "sess-ckpt-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the DEK of the ended Save: want it gone, got %v", err)
	}
}
