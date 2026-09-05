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

package nodeagent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/remotes"
	"github.com/containerd/errdefs"
)

// fakeContainerdClient is a narrow test double that satisfies the
// containerdClient interface without dialing a real daemon.
type fakeContainerdClient struct {
	mu           sync.Mutex
	getErr       error
	getImg       images.Image
	pullErr      error
	getCalls     int
	pullCalls    int
	closeCalls   int
	lastPullRef  string
	lastResolver remotes.Resolver
}

func (f *fakeContainerdClient) GetImage(_ context.Context, _ string) (images.Image, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getCalls++
	return f.getImg, f.getErr
}

func (f *fakeContainerdClient) Pull(_ context.Context, ref string, resolver remotes.Resolver) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pullCalls++
	f.lastPullRef = ref
	f.lastResolver = resolver
	return f.pullErr
}

func (f *fakeContainerdClient) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closeCalls++
	return nil
}

// notFoundErr wraps errdefs.ErrNotFound so errdefs.IsNotFound returns
// true for the fake's response.
func notFoundErr() error {
	return errdefs.ErrNotFound
}

// TestPull_CacheHit verifies that when the image already exists in the
// content store, Pull returns nil without invoking the remote Pull API.
func TestPull_CacheHit(t *testing.T) {
	fake := &fakeContainerdClient{getErr: nil, getImg: images.Image{Name: "alpine:latest"}}
	p := newContainerdPullerWithClient(fake, "k8s.io", "")

	if err := p.Pull(context.Background(), "alpine:latest"); err != nil {
		t.Fatalf("Pull returned %v, want nil", err)
	}
	if fake.pullCalls != 0 {
		t.Fatalf("expected zero pull calls on cache hit, got %d", fake.pullCalls)
	}
	if fake.getCalls != 1 {
		t.Fatalf("expected one GetImage call, got %d", fake.getCalls)
	}
}

// TestPull_FreshPull verifies the happy-path pull when GetImage reports
// not-found and Pull succeeds.
func TestPull_FreshPull(t *testing.T) {
	fake := &fakeContainerdClient{getErr: notFoundErr(), pullErr: nil}
	p := newContainerdPullerWithClient(fake, "k8s.io", "")

	if err := p.Pull(context.Background(), "alpine:latest"); err != nil {
		t.Fatalf("Pull returned %v, want nil", err)
	}
	if fake.pullCalls != 1 {
		t.Fatalf("expected one pull call, got %d", fake.pullCalls)
	}
	if fake.lastPullRef != "alpine:latest" {
		t.Fatalf("lastPullRef = %q, want %q", fake.lastPullRef, "alpine:latest")
	}
}

// TestPull_ContainerdUnreachable verifies that a non-NotFound error
// from GetImage is classified as ErrContainerdUnreachable so the pool
// Manager triggers retry.
func TestPull_ContainerdUnreachable(t *testing.T) {
	fake := &fakeContainerdClient{getErr: errors.New("rpc error: code = Unavailable desc = connection refused")}
	p := newContainerdPullerWithClient(fake, "k8s.io", "")

	err := p.Pull(context.Background(), "alpine:latest")
	if err == nil {
		t.Fatal("Pull returned nil, want error")
	}
	if !errors.Is(err, ErrContainerdUnreachable) {
		t.Fatalf("err = %v, want wrap of ErrContainerdUnreachable", err)
	}
	if fake.pullCalls != 0 {
		t.Fatalf("expected zero pull calls when GetImage failed fatally, got %d", fake.pullCalls)
	}
}

// TestPull_RegistryNotFound verifies that a NotFound from the remote
// pull surfaces as ErrImageNotFound.
func TestPull_RegistryNotFound(t *testing.T) {
	fake := &fakeContainerdClient{
		getErr:  notFoundErr(),
		pullErr: errors.New("manifest unknown: 404 not found"),
	}
	p := newContainerdPullerWithClient(fake, "k8s.io", "")

	err := p.Pull(context.Background(), "alpine:doesnotexist")
	if err == nil {
		t.Fatal("Pull returned nil, want error")
	}
	if !errors.Is(err, ErrImageNotFound) {
		t.Fatalf("err = %v, want wrap of ErrImageNotFound", err)
	}
}

// TestPull_AuthRequired verifies a registry 401 is classified as
// ErrAuthRequired.
func TestPull_AuthRequired(t *testing.T) {
	fake := &fakeContainerdClient{
		getErr:  notFoundErr(),
		pullErr: errors.New("pull access denied: 401 Unauthorized"),
	}
	p := newContainerdPullerWithClient(fake, "k8s.io", "")

	err := p.Pull(context.Background(), "private/image:1")
	if err == nil {
		t.Fatal("Pull returned nil, want error")
	}
	if !errors.Is(err, ErrAuthRequired) {
		t.Fatalf("err = %v, want wrap of ErrAuthRequired", err)
	}
}

// TestPull_GenericPullFailed verifies a pull error that doesn't match
// any known classification falls through to ErrPullFailed.
func TestPull_GenericPullFailed(t *testing.T) {
	fake := &fakeContainerdClient{
		getErr:  notFoundErr(),
		pullErr: errors.New("disk quota exceeded"),
	}
	p := newContainerdPullerWithClient(fake, "k8s.io", "")

	err := p.Pull(context.Background(), "alpine:latest")
	if err == nil {
		t.Fatal("Pull returned nil, want error")
	}
	if !errors.Is(err, ErrPullFailed) {
		t.Fatalf("err = %v, want wrap of ErrPullFailed", err)
	}
}

// TestPull_EmptyRef rejects an empty reference up-front without
// touching the client.
func TestPull_EmptyRef(t *testing.T) {
	fake := &fakeContainerdClient{}
	p := newContainerdPullerWithClient(fake, "k8s.io", "")

	err := p.Pull(context.Background(), "")
	if !errors.Is(err, ErrPullFailed) {
		t.Fatalf("err = %v, want ErrPullFailed", err)
	}
	if fake.getCalls != 0 || fake.pullCalls != 0 {
		t.Fatalf("empty ref should not touch the client; got get=%d pull=%d", fake.getCalls, fake.pullCalls)
	}
}

// TestPull_NilReceiver guards against nil-puller callers.
func TestPull_NilReceiver(t *testing.T) {
	var p *ContainerdPuller
	err := p.Pull(context.Background(), "alpine:latest")
	if !errors.Is(err, ErrPullFailed) {
		t.Fatalf("err = %v, want ErrPullFailed", err)
	}
}

// TestClose_CallsUnderlying exercises the Close pass-through and
// verifies multiple Close calls are safe.
func TestClose_CallsUnderlying(t *testing.T) {
	fake := &fakeContainerdClient{}
	p := newContainerdPullerWithClient(fake, "k8s.io", "")
	if err := p.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if fake.closeCalls != 2 {
		t.Fatalf("closeCalls = %d, want 2", fake.closeCalls)
	}
}

// TestLoadDockerAuth_EncodedAuth parses a Docker config.json whose
// auth entries carry the base64-encoded form.
func TestLoadDockerAuth_EncodedAuth(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	// base64("alice:hunter2") = YWxpY2U6aHVudGVyMg==
	body := `{"auths":{"example.com":{"auth":"YWxpY2U6aHVudGVyMg=="}}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	entries, err := loadDockerAuth(path)
	if err != nil {
		t.Fatalf("loadDockerAuth: %v", err)
	}
	e, ok := entries["example.com"]
	if !ok {
		t.Fatalf("missing example.com entry: %+v", entries)
	}
	if e.username != "alice" || e.password != "hunter2" {
		t.Fatalf("got %q/%q, want alice/hunter2", e.username, e.password)
	}
}

// TestLoadDockerAuth_SeparateFields parses a config.json whose entries
// use the explicit username/password split instead of auth.
func TestLoadDockerAuth_SeparateFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	body := `{"auths":{"example.com":{"username":"bob","password":"s3cret"}}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	entries, err := loadDockerAuth(path)
	if err != nil {
		t.Fatalf("loadDockerAuth: %v", err)
	}
	if entries["example.com"].username != "bob" || entries["example.com"].password != "s3cret" {
		t.Fatalf("unexpected creds: %+v", entries["example.com"])
	}
}

// TestLoadDockerAuth_MissingFile surfaces a wrapped read error.
func TestLoadDockerAuth_MissingFile(t *testing.T) {
	_, err := loadDockerAuth("/tmp/definitely-not-here.json")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

// TestBuildResolver_Anonymous verifies the empty-authFile path
// produces a resolver (non-nil).
func TestBuildResolver_Anonymous(t *testing.T) {
	p := newContainerdPullerWithClient(&fakeContainerdClient{}, "k8s.io", "")
	r, err := p.buildResolver()
	if err != nil {
		t.Fatalf("buildResolver: %v", err)
	}
	if r == nil {
		t.Fatal("resolver is nil")
	}
}

// TestBuildResolver_WithAuthFile verifies the auth-file path loads
// successfully when the file is valid JSON.
func TestBuildResolver_WithAuthFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	body := `{"auths":{"example.com":{"username":"u","password":"p"}}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	p := newContainerdPullerWithClient(&fakeContainerdClient{}, "k8s.io", path)
	r, err := p.buildResolver()
	if err != nil {
		t.Fatalf("buildResolver: %v", err)
	}
	if r == nil {
		t.Fatal("resolver is nil")
	}
}

// TestBuildResolver_InvalidAuthFile surfaces parse errors.
func TestBuildResolver_InvalidAuthFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	p := newContainerdPullerWithClient(&fakeContainerdClient{}, "k8s.io", path)
	if _, err := p.buildResolver(); err == nil {
		t.Fatal("expected parse error, got nil")
	}
}

// TestPull_BuildResolverFailure verifies that a broken authFile at
// pull time surfaces as ErrPullFailed (not ErrImageNotFound or similar)
// because the resolver cannot be constructed.
func TestPull_BuildResolverFailure(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "invalid.json")
	if err := os.WriteFile(bad, []byte("}not json{"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	fake := &fakeContainerdClient{getErr: notFoundErr()}
	p := newContainerdPullerWithClient(fake, "k8s.io", bad)

	err := p.Pull(context.Background(), "alpine:latest")
	if !errors.Is(err, ErrPullFailed) {
		t.Fatalf("err = %v, want ErrPullFailed", err)
	}
	if fake.pullCalls != 0 {
		t.Fatalf("pullCalls = %d, expected zero because resolver failed first", fake.pullCalls)
	}
}

// TestClose_NilReceiver is a guard against nil-puller Close calls.
func TestClose_NilReceiver(t *testing.T) {
	var p *ContainerdPuller
	if err := p.Close(); err != nil {
		t.Fatalf("Close returned %v, want nil", err)
	}
}

// TestClassifyPullError_Nil verifies classifyPullError passes through
// nil unchanged.
func TestClassifyPullError_Nil(t *testing.T) {
	if err := classifyPullError("alpine", nil); err != nil {
		t.Fatalf("got %v, want nil", err)
	}
}

// TestClassifyPullError_ErrdefsClassification verifies the errdefs
// classifications route to the matching sentinel even when the error
// message itself contains no hints.
func TestClassifyPullError_ErrdefsClassification(t *testing.T) {
	cases := map[string]struct {
		in   error
		want error
	}{
		"errdefs notfound":    {errdefs.ErrNotFound, ErrImageNotFound},
		"errdefs permission":  {errdefs.ErrPermissionDenied, ErrAuthRequired},
		"errdefs unavailable": {errdefs.ErrUnavailable, ErrContainerdUnreachable},
		"generic":             {errors.New("who knows"), ErrPullFailed},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			out := classifyPullError("alpine", tc.in)
			if !errors.Is(out, tc.want) {
				t.Fatalf("classifyPullError(%v) = %v, want wrap of %v", tc.in, out, tc.want)
			}
		})
	}
}

// TestLoadDockerAuth_InvalidBase64 surfaces a decode error when the
// auth field is not valid base64.
func TestLoadDockerAuth_InvalidBase64(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	body := `{"auths":{"example.com":{"auth":"!!!not-base64!!!"}}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := loadDockerAuth(path); err == nil {
		t.Fatal("expected decode error, got nil")
	}
}

// TestHelpers_NilErrors covers the defensive nil-err guards on the
// isXxxLike helpers so none of them panic if handed a nil.
func TestHelpers_NilErrors(t *testing.T) {
	if isNotFoundLike(nil) {
		t.Error("isNotFoundLike(nil) returned true")
	}
	if isAuthErrorLike(nil) {
		t.Error("isAuthErrorLike(nil) returned true")
	}
	if isUnreachableLike(nil) {
		t.Error("isUnreachableLike(nil) returned true")
	}
}

// TestLookupDockerCreds verifies the host → credential mapping,
// including the docker.io index alias fallback.
func TestLookupDockerCreds(t *testing.T) {
	creds := map[string]dockerAuthEntry{
		"ghcr.io":                     {username: "ghu", password: "ghp"},
		"https://index.docker.io/v1/": {username: "dock", password: "dpw"},
	}
	cases := []struct {
		name     string
		host     string
		wantUser string
		wantPass string
	}{
		{"direct hit", "ghcr.io", "ghu", "ghp"},
		{"docker.io alias", "registry-1.docker.io", "dock", "dpw"},
		{"miss", "private.example.com", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u, p := lookupDockerCreds(creds, tc.host)
			if u != tc.wantUser || p != tc.wantPass {
				t.Fatalf("got (%q, %q), want (%q, %q)", u, p, tc.wantUser, tc.wantPass)
			}
		})
	}
}

// TestPull_Dedupes verifies two concurrent Pull calls for the same
// reference result in exactly one underlying Pull invocation via the
// sync.Once machinery.
func TestPull_Dedupes(t *testing.T) {
	fake := &fakeContainerdClient{getErr: notFoundErr()}
	p := newContainerdPullerWithClient(fake, "k8s.io", "")

	var wg sync.WaitGroup
	for range 5 {
		wg.Go(func() {
			_ = p.Pull(context.Background(), "alpine:latest")
		})
	}
	wg.Wait()
	if fake.pullCalls != 1 {
		t.Fatalf("pullCalls = %d, want 1 (dedup failed)", fake.pullCalls)
	}
}
