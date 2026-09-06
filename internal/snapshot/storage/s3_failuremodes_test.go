// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// Tests for the three S3 failure modes in #297. Each of them is a case the
// happy-path fake in s3_test.go cannot express, so this file carries a fake
// that models the parts of S3 that actually differ: object versioning,
// HeadObject's 403-instead-of-404, and in-progress multipart uploads.

// versionedFakeS3 is an S3-compatible fake with object versioning, a
// switchable "no s3:ListBucket" mode, and a multipart-upload registry.
type versionedFakeS3 struct {
	mu sync.Mutex

	// versions holds every version ever written for a key, oldest first.
	// A deleteMarker entry models what DeleteObject does on a versioned
	// bucket: it hides the key without removing anything.
	versions map[string][]fakeVersion

	// headForbidden models the least-privilege policy trap: without
	// s3:ListBucket, S3 answers HeadObject for a missing key with 403.
	headForbidden bool

	// uploads models in-progress multipart uploads: id -> (key, initiated).
	uploads map[string]fakeUpload

	// aborted records the upload ids AbortMultipartUpload was called for.
	aborted []string
}

type fakeVersion struct {
	id           string
	body         []byte
	deleteMarker bool
}

type fakeUpload struct {
	key       string
	initiated time.Time
}

func newVersionedFakeS3() *versionedFakeS3 {
	return &versionedFakeS3{
		versions: map[string][]fakeVersion{},
		uploads:  map[string]fakeUpload{},
	}
}

// current returns the newest non-delete-marker version of a key.
func (f *versionedFakeS3) current(key string) ([]byte, bool) {
	vs := f.versions[key]
	if len(vs) == 0 {
		return nil, false
	}
	last := vs[len(vs)-1]
	if last.deleteMarker {
		return nil, false
	}
	return last.body, true
}

// liveVersionCount counts stored object versions, ignoring delete markers —
// this is what "did the erase actually erase" comes down to.
func (f *versionedFakeS3) liveVersionCount(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	var n int
	for _, v := range f.versions[key] {
		if !v.deleteMarker {
			n++
		}
	}
	return n
}

func (f *versionedFakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	query := r.URL.Query()
	// Path style: /<bucket>/<key...>; bucket-level operations have no key.
	trimmed := strings.TrimPrefix(r.URL.Path, "/")
	parts := strings.SplitN(trimmed, "/", 2)
	var key string
	if len(parts) == 2 {
		key = parts[1]
	}

	switch {
	case r.Method == http.MethodGet && query.Has("versions"):
		f.listObjectVersions(w, query.Get("prefix"))
		return
	case r.Method == http.MethodGet && query.Has("uploads"):
		f.listMultipartUploads(w)
		return
	case r.Method == http.MethodDelete && query.Get("uploadId") != "":
		id := query.Get("uploadId")
		delete(f.uploads, id)
		f.aborted = append(f.aborted, id)
		w.WriteHeader(http.StatusNoContent)
		return
	}

	switch r.Method {
	case http.MethodPut:
		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		f.versions[key] = append(f.versions[key], fakeVersion{
			id:   fmt.Sprintf("v%d", len(f.versions[key])+1),
			body: body,
		})
		w.Header().Set("ETag", `"fake"`)
		w.WriteHeader(http.StatusOK)

	case http.MethodGet:
		body, ok := f.current(key)
		if !ok {
			writeS3Error(w, http.StatusNotFound, "NoSuchKey")
			return
		}
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)

	case http.MethodHead:
		body, ok := f.current(key)
		if !ok {
			if f.headForbidden {
				// No body: HEAD responses never carry one, which is exactly
				// why this is easy to misread as a 404 in the first place.
				w.WriteHeader(http.StatusForbidden)
				return
			}
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
		w.WriteHeader(http.StatusOK)

	case http.MethodDelete:
		if id := query.Get("versionId"); id != "" {
			kept := f.versions[key][:0]
			for _, v := range f.versions[key] {
				if v.id != id {
					kept = append(kept, v)
				}
			}
			f.versions[key] = kept
			w.WriteHeader(http.StatusNoContent)
			return
		}
		// Versioned bucket: a plain delete writes a marker and removes
		// nothing. This is the whole bug.
		if len(f.versions[key]) > 0 {
			f.versions[key] = append(f.versions[key], fakeVersion{
				id:           fmt.Sprintf("v%d", len(f.versions[key])+1),
				deleteMarker: true,
			})
		}
		w.WriteHeader(http.StatusNoContent)

	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func writeS3Error(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w,
		`<?xml version="1.0"?><Error><Code>%s</Code><Message>fake</Message></Error>`, code)
}

func (f *versionedFakeS3) listObjectVersions(w http.ResponseWriter, prefix string) {
	var sb strings.Builder
	sb.WriteString(`<?xml version="1.0"?><ListVersionsResult><IsTruncated>false</IsTruncated>`)
	keys := make([]string, 0, len(f.versions))
	for k := range f.versions {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		for _, v := range f.versions[k] {
			tag := "Version"
			if v.deleteMarker {
				tag = "DeleteMarker"
			}
			fmt.Fprintf(&sb, `<%s><Key>%s</Key><VersionId>%s</VersionId><IsLatest>false</IsLatest></%s>`,
				tag, k, v.id, tag)
		}
	}
	sb.WriteString(`</ListVersionsResult>`)
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, sb.String())
}

func (f *versionedFakeS3) listMultipartUploads(w http.ResponseWriter) {
	var sb strings.Builder
	sb.WriteString(`<?xml version="1.0"?><ListMultipartUploadsResult><IsTruncated>false</IsTruncated>`)
	ids := make([]string, 0, len(f.uploads))
	for id := range f.uploads {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		u := f.uploads[id]
		fmt.Fprintf(&sb, `<Upload><Key>%s</Key><UploadId>%s</UploadId><Initiated>%s</Initiated></Upload>`,
			u.key, id, u.initiated.UTC().Format(time.RFC3339))
	}
	sb.WriteString(`</ListMultipartUploadsResult>`)
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, sb.String())
}

func newVersionedTestBackend(t *testing.T) (*S3Backend, *versionedFakeS3) {
	t.Helper()
	fake := newVersionedFakeS3()
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

// --- 1. HeadObject 403 -------------------------------------------------

func TestSaveOnForbiddenHeadNamesTheMissingPermission(t *testing.T) {
	backend, fake := newVersionedTestBackend(t)
	fake.headForbidden = true

	_, _, err := backend.Save(context.Background(), "sess-1", strings.NewReader("payload"))
	if err == nil {
		t.Fatal("Save succeeded against a store that 403s the pre-write HEAD")
	}
	// The point of the fix is that the operator does not have to know the
	// HeadObject-403 quirk to read this error.
	if !strings.Contains(err.Error(), "s3:ListBucket") {
		t.Errorf("error does not name the missing permission: %v", err)
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("error does not say the status it saw: %v", err)
	}
}

func TestForbiddenHeadIsNotReportedAsNotFound(t *testing.T) {
	backend, fake := newVersionedTestBackend(t)
	fake.headForbidden = true

	// Stat must not answer "no such checkpoint" for a broken IAM policy: a
	// 403 is not a 404, and conflating them turns a misconfiguration into a
	// silent, permanent absence.
	_, ok, err := backend.Stat(context.Background(), "sess-1")
	if err == nil {
		t.Fatal("Stat reported success against a 403")
	}
	if ok {
		t.Error("Stat claimed the object exists")
	}
	if errors.Is(err, ErrNotFound) {
		t.Error("Stat mapped a 403 onto ErrNotFound")
	}
	if !strings.Contains(err.Error(), "s3:ListBucket") {
		t.Errorf("error does not name the missing permission: %v", err)
	}
}

func TestNotFoundHeadStillReadsAsAbsent(t *testing.T) {
	backend, _ := newVersionedTestBackend(t)

	// The 403 handling must not have broken the ordinary case.
	if _, ok, err := backend.Stat(context.Background(), "sess-absent"); err != nil || ok {
		t.Fatalf("Stat on an absent key = (%v, %v), want (false, nil)", ok, err)
	}
}

// --- 2. Crypto-erase on a versioned bucket -----------------------------

func TestDestroySealedDEKRemovesEveryVersion(t *testing.T) {
	backend, fake := newVersionedTestBackend(t)
	ctx := context.Background()
	store := backend.DEKStore()

	if err := store.Put(ctx, "sess-1", []byte("sealed-dek-v1")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	// A second write is refused (never replace key material), so simulate the
	// noncurrent version an earlier lifecycle would have left behind.
	dekKey := backend.dekKey("sess-1")
	fake.mu.Lock()
	fake.versions[dekKey] = append(fake.versions[dekKey], fakeVersion{id: "v2", body: []byte("sealed-dek-v2")})
	fake.mu.Unlock()

	if got := fake.liveVersionCount(dekKey); got != 2 {
		t.Fatalf("setup: %d live versions, want 2", got)
	}

	if err := store.Destroy(ctx, "sess-1"); err != nil {
		t.Fatalf("Destroy: %v", err)
	}

	// This is the assertion that matters: ADR-0005 invariant 5 treats this
	// destroy as the erasure, so key material surviving as a noncurrent
	// version makes the guarantee nominal.
	if got := fake.liveVersionCount(dekKey); got != 0 {
		t.Errorf("%d sealed-DEK versions survived Destroy, want 0", got)
	}
}

func TestDeleteRemovesEveryVersionOfEveryObject(t *testing.T) {
	backend, fake := newVersionedTestBackend(t)
	ctx := context.Background()

	if _, _, err := backend.Save(ctx, "sess-1", strings.NewReader("payload")); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := backend.DEKStore().Put(ctx, "sess-1", []byte("sealed")); err != nil {
		t.Fatalf("Put dek: %v", err)
	}
	if err := backend.Delete(ctx, "sess-1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	for _, key := range []string{backend.stateKey("sess-1"), backend.shaKey("sess-1"), backend.dekKey("sess-1")} {
		if got := fake.liveVersionCount(key); got != 0 {
			t.Errorf("%d versions of %q survived Delete, want 0", got, key)
		}
	}
}

func TestDeletingThePayloadSpareTheSidecarsOfOtherSnapshots(t *testing.T) {
	backend, fake := newVersionedTestBackend(t)
	ctx := context.Background()

	// ListObjectVersions has no exact-key filter, only a prefix — and
	// state.bin is a prefix of state.bin.sha256 and state.bin.dek. A
	// prefix-wide delete would take a snapshot's sidecars out from under it.
	if _, _, err := backend.Save(ctx, "sess-1", strings.NewReader("one")); err != nil {
		t.Fatalf("Save sess-1: %v", err)
	}
	if _, _, err := backend.Save(ctx, "sess-2", strings.NewReader("two")); err != nil {
		t.Fatalf("Save sess-2: %v", err)
	}
	if err := backend.deleteObjectAllVersions(ctx, backend.stateKey("sess-1")); err != nil {
		t.Fatalf("deleteObjectAllVersions: %v", err)
	}
	if got := fake.liveVersionCount(backend.stateKey("sess-1")); got != 0 {
		t.Errorf("payload survived: %d versions", got)
	}
	if got := fake.liveVersionCount(backend.shaKey("sess-1")); got != 1 {
		t.Errorf("sha sidecar of the same snapshot was collateral: %d versions, want 1", got)
	}
	if got := fake.liveVersionCount(backend.stateKey("sess-2")); got != 1 {
		t.Errorf("another snapshot was collateral: %d versions, want 1", got)
	}
}

// --- 3. Orphaned multipart uploads -------------------------------------

func TestAbortStaleMultipartUploadsAbortsOnlyTheStaleOnes(t *testing.T) {
	backend, fake := newVersionedTestBackend(t)

	fake.mu.Lock()
	fake.uploads["upload-old"] = fakeUpload{
		key: "sessions/sess-1/state.bin", initiated: time.Now().Add(-6 * time.Hour),
	}
	fake.uploads["upload-live"] = fakeUpload{
		key: "sessions/sess-2/state.bin", initiated: time.Now().Add(-2 * time.Minute),
	}
	fake.mu.Unlock()

	n, err := backend.AbortStaleMultipartUploads(context.Background(), time.Hour)
	if err != nil {
		t.Fatalf("AbortStaleMultipartUploads: %v", err)
	}
	if n != 1 {
		t.Errorf("aborted %d uploads, want 1", n)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.aborted) != 1 || fake.aborted[0] != "upload-old" {
		// Aborting a live upload would kill another node-agent's suspend
		// mid-flight, which is worse than the leak being fixed.
		t.Errorf("aborted %v, want only [upload-old]", fake.aborted)
	}
}

func TestAbortStaleMultipartUploadsRejectsANonPositiveWindow(t *testing.T) {
	backend, _ := newVersionedTestBackend(t)
	// A zero window would abort every upload including live ones.
	if _, err := backend.AbortStaleMultipartUploads(context.Background(), 0); err == nil {
		t.Fatal("a zero window was accepted")
	}
}

func TestAbortStaleMultipartUploadsIsANoOpWithNothingToDo(t *testing.T) {
	backend, fake := newVersionedTestBackend(t)
	n, err := backend.AbortStaleMultipartUploads(context.Background(), time.Hour)
	if err != nil || n != 0 {
		t.Fatalf("AbortStaleMultipartUploads = (%d, %v), want (0, nil)", n, err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.aborted) != 0 {
		t.Errorf("aborted %v with no uploads present", fake.aborted)
	}
}
