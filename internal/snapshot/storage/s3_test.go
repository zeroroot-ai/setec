// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/zeroroot-ai/setec/internal/snapshot/atrest"
)

// fakeS3 is a minimal in-process S3-compatible object store: enough
// of the REST surface (path-style PUT/GET/HEAD/DELETE object) for the
// aws-sdk-go-v2 client to run the backend end-to-end without MinIO.
type fakeS3 struct {
	mu      sync.Mutex
	objects map[string][]byte
}

func newFakeS3() *fakeS3 { return &fakeS3{objects: map[string][]byte{}} }

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimPrefix(r.URL.Path, "/")
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.Method {
	case http.MethodPut:
		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		f.objects[key] = body
		w.Header().Set("ETag", `"fake"`)
		w.WriteHeader(http.StatusOK)
	case http.MethodGet:
		body, ok := f.objects[key]
		if !ok {
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`<?xml version="1.0"?><Error><Code>NoSuchKey</Code><Message>gone</Message></Error>`))
			return
		}
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	case http.MethodHead:
		body, ok := f.objects[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
		w.WriteHeader(http.StatusOK)
	case http.MethodDelete:
		delete(f.objects, key)
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// corrupt flips a byte of a stored object, addressed by key suffix.
func (f *fakeS3) corrupt(t *testing.T, suffix string) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for key, body := range f.objects {
		if strings.HasSuffix(key, suffix) && len(body) > 0 {
			mutated := append([]byte(nil), body...)
			mutated[len(mutated)/2] ^= 0xFF
			f.objects[key] = mutated
			return
		}
	}
	t.Fatalf("no object with suffix %q to corrupt", suffix)
}

// newTestS3Backend spins up the fake store and a backend against it.
func newTestS3Backend(t *testing.T) (*S3Backend, *fakeS3) {
	t.Helper()
	fake := newFakeS3()
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	backend, err := NewS3Backend(context.Background(), S3Config{
		Endpoint:        srv.URL,
		Bucket:          "checkpoints",
		Region:          "test-region",
		Prefix:          "sessions/",
		UsePathStyle:    true,
		AccessKeyID:     "test",
		SecretAccessKey: "test",
	})
	if err != nil {
		t.Fatalf("NewS3Backend: %v", err)
	}
	return backend, fake
}

func TestS3BackendRoundTrip(t *testing.T) {
	ctx := context.Background()
	b, _ := newTestS3Backend(t)
	payload := bytes.Repeat([]byte("setec astronomy "), 4096)

	size, ref, err := b.Save(ctx, "ns-sb-ckpt-1", bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if ref != "ns-sb-ckpt-1" {
		t.Fatalf("ref = %q, want snapshot id", ref)
	}
	if size != int64(len(payload)) {
		t.Fatalf("size = %d, want %d", size, len(payload))
	}

	gotSize, exists, err := b.Stat(ctx, ref)
	if err != nil || !exists || gotSize != int64(len(payload)) {
		t.Fatalf("Stat = (%d,%v,%v), want (%d,true,nil)", gotSize, exists, err, len(payload))
	}

	rc, err := b.Open(ctx, ref)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	_ = rc.Close()
	if !bytes.Equal(got, payload) {
		t.Fatal("payload mismatch after S3 round trip")
	}

	if err := b.Delete(ctx, ref); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := b.Open(ctx, ref); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Open after delete = %v, want ErrNotFound", err)
	}
	if err := b.Delete(ctx, ref); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second Delete = %v, want ErrNotFound", err)
	}
}

func TestS3BackendDoubleSave(t *testing.T) {
	ctx := context.Background()
	b, _ := newTestS3Backend(t)
	if _, _, err := b.Save(ctx, "dup", strings.NewReader("one")); err != nil {
		t.Fatalf("first Save: %v", err)
	}
	if _, _, err := b.Save(ctx, "dup", strings.NewReader("two")); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("second Save = %v, want ErrAlreadyExists", err)
	}
}

func TestS3BackendCorruptionDetected(t *testing.T) {
	ctx := context.Background()
	b, fake := newTestS3Backend(t)
	if _, _, err := b.Save(ctx, "c1", strings.NewReader(strings.Repeat("x", 8192))); err != nil {
		t.Fatalf("Save: %v", err)
	}
	fake.corrupt(t, "c1/state.bin")
	rc, err := b.Open(ctx, "c1")
	if err == nil {
		_, err = io.ReadAll(rc)
		_ = rc.Close()
	}
	if !errors.Is(err, ErrCorrupted) {
		t.Fatalf("read of corrupted payload = %v, want ErrCorrupted", err)
	}
}

func TestS3BackendRejectsBadIDs(t *testing.T) {
	ctx := context.Background()
	b, _ := newTestS3Backend(t)
	for _, id := range []string{"", "a/b", "..", "a..b"} {
		if _, _, err := b.Save(ctx, id, strings.NewReader("x")); err == nil {
			t.Fatalf("Save(%q) accepted an unsafe id", id)
		}
	}
}

func TestS3DEKStore(t *testing.T) {
	ctx := context.Background()
	b, _ := newTestS3Backend(t)
	store := b.DEKStore()

	if _, err := store.Get(ctx, "k1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get before Put = %v, want ErrNotFound", err)
	}
	if err := store.Put(ctx, "k1", []byte("sealed-blob")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := store.Put(ctx, "k1", []byte("other")); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("second Put = %v, want ErrAlreadyExists (never overwrite key material)", err)
	}
	sealed, err := store.Get(ctx, "k1")
	if err != nil || string(sealed) != "sealed-blob" {
		t.Fatalf("Get = (%q,%v)", sealed, err)
	}
	if err := store.Destroy(ctx, "k1"); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if err := store.Destroy(ctx, "k1"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("second Destroy = %v, want os.ErrNotExist", err)
	}
}

// TestS3EncryptedComposition proves the portable session-checkpoint
// sealing path end to end: a checkpoint written through the encrypted
// wrapper on "node A" opens on "node B" (a separate backend instance)
// with only the cluster-scoped session KEK; a wrong KEK fails closed;
// destroying the sealed DEK (or the whole Delete) crypto-erases the
// checkpoint; and the bucket never holds plaintext.
func TestS3EncryptedComposition(t *testing.T) {
	ctx := context.Background()
	inner, fake := newTestS3Backend(t)

	kek := bytes.Repeat([]byte{0x42}, atrest.KeySize)
	nodeA := &EncryptedBackend{Inner: inner, KEK: StaticKEKSource(kek), DEKs: inner.DEKStore()}

	secret := []byte("TOP SECRET guest memory: sk-ant-not-really")
	payload := append(bytes.Repeat([]byte("m"), 4096), secret...)
	if _, _, err := nodeA.Save(ctx, "sess-ckpt-1", bytes.NewReader(payload)); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// The bucket carries no plaintext.
	fake.mu.Lock()
	for key, body := range fake.objects {
		if bytes.Contains(body, secret) {
			fake.mu.Unlock()
			t.Fatalf("plaintext leaked into bucket object %q", key)
		}
	}
	fake.mu.Unlock()

	// "Node B": a fresh backend instance against the same store, same KEK.
	innerB, err := NewS3Backend(ctx, S3Config{
		Endpoint: inner.endpointForTest(), Bucket: inner.Bucket, Prefix: inner.Prefix,
		Region: "test-region", UsePathStyle: true, AccessKeyID: "test", SecretAccessKey: "test",
	})
	if err != nil {
		t.Fatalf("node B backend: %v", err)
	}
	nodeB := &EncryptedBackend{Inner: innerB, KEK: StaticKEKSource(kek), DEKs: innerB.DEKStore()}
	rc, err := nodeB.Open(ctx, "sess-ckpt-1")
	if err != nil {
		t.Fatalf("Open on node B: %v", err)
	}
	got, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("node B decrypt: err=%v match=%v", err, bytes.Equal(got, payload))
	}

	// Wrong KEK fails closed.
	wrong := &EncryptedBackend{Inner: innerB, KEK: StaticKEKSource(bytes.Repeat([]byte{9}, atrest.KeySize)), DEKs: innerB.DEKStore()}
	if _, err := wrong.Open(ctx, "sess-ckpt-1"); !errors.Is(err, ErrCorrupted) {
		t.Fatalf("Open with wrong KEK = %v, want ErrCorrupted", err)
	}

	// Delete crypto-erases: the sealed DEK is gone first, so the
	// artifact no longer exists from any node's perspective.
	if err := nodeB.Delete(ctx, "sess-ckpt-1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := nodeA.Open(ctx, "sess-ckpt-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Open after delete = %v, want ErrNotFound", err)
	}
}

// endpointForTest exposes the backend's endpoint for building a second
// client in tests.
func (b *S3Backend) endpointForTest() string {
	opts := b.Client.Options()
	if opts.BaseEndpoint != nil {
		return *opts.BaseEndpoint
	}
	return ""
}
