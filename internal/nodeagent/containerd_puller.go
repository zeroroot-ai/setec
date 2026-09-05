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
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/remotes"
	"github.com/containerd/containerd/v2/core/remotes/docker"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/errdefs"
)

// Typed sentinel errors returned by ContainerdPuller. Callers use
// errors.Is to distinguish transient from permanent failures so retry
// policy in the pool Manager can be correct.
var (
	// ErrContainerdUnreachable indicates the containerd daemon could not
	// be reached over its Unix socket. Transient; the pool Manager
	// retries on the next reconcile tick.
	ErrContainerdUnreachable = errors.New("nodeagent: containerd unreachable")

	// ErrImageNotFound indicates the registry returned 404 for the
	// requested OCI reference (tag or digest does not exist). Non-
	// transient without operator action.
	ErrImageNotFound = errors.New("nodeagent: image not found in registry")

	// ErrAuthRequired indicates the registry returned 401/403. The
	// operator must configure an auth file (--containerd-auth-file)
	// or make the image public.
	ErrAuthRequired = errors.New("nodeagent: registry authentication required")

	// ErrPullFailed is the catch-all for any other pull failure. The
	// wrapped error carries the underlying cause.
	ErrPullFailed = errors.New("nodeagent: image pull failed")
)

// containerdClient is the narrow interface ContainerdPuller depends on.
// A real *client.Client satisfies it via the containerdClientAdapter
// wrapper; tests inject a fake so the unit tests do not require a
// running containerd daemon.
type containerdClient interface {
	// GetImage looks up a single image by reference. Returns an
	// errdefs.IsNotFound-wrapped error when the image is not in the
	// local content store.
	GetImage(ctx context.Context, ref string) (images.Image, error)

	// Pull downloads the given reference into the local content store.
	Pull(ctx context.Context, ref string, resolver remotes.Resolver) error

	// Close releases the underlying gRPC connection.
	Close() error
}

// ContainerdPuller is the production ImagePuller implementation. It
// dials the node's containerd daemon over a Unix socket, uses the
// standard Docker-registry resolver for remote auth, and satisfies the
// narrow ImagePuller interface consumed by ImageCache.
//
// The puller is safe for concurrent use: a mutex serialises pulls of
// the same reference so duplicate prefetch calls collapse into one
// network round-trip.
type ContainerdPuller struct {
	client    containerdClient
	namespace string
	authFile  string

	// mu guards inflight. Each ref gets a dedicated sync.Once under
	// lock so the second caller for a given ref waits on the first
	// instead of racing another Pull.
	mu       sync.Mutex
	inflight map[string]*sync.Once
	results  map[string]error
}

// NewContainerdPuller dials the containerd daemon at socketPath and
// returns a ready ContainerdPuller. A non-empty authFile is read as a
// Docker config.json and used as the source of registry credentials;
// an empty authFile means anonymous access only.
//
// Returns ErrContainerdUnreachable wrapping the dial error when the
// socket cannot be reached at startup; callers typically treat this as
// fatal.
func NewContainerdPuller(socketPath, namespace, authFile string) (*ContainerdPuller, error) {
	if namespace == "" {
		namespace = "k8s.io"
	}
	c, err := client.New(socketPath, client.WithDefaultNamespace(namespace))
	if err != nil {
		return nil, fmt.Errorf("%w: dial %q: %v", ErrContainerdUnreachable, socketPath, err)
	}
	return newContainerdPullerWithClient(&containerdClientAdapter{c: c}, namespace, authFile), nil
}

// newContainerdPullerWithClient is the constructor tests use to inject
// a fake containerdClient. Not exported.
func newContainerdPullerWithClient(c containerdClient, namespace, authFile string) *ContainerdPuller {
	return &ContainerdPuller{
		client:    c,
		namespace: namespace,
		authFile:  authFile,
		inflight:  map[string]*sync.Once{},
		results:   map[string]error{},
	}
}

// Close releases the underlying containerd connection. Safe to call
// multiple times.
func (p *ContainerdPuller) Close() error {
	if p == nil || p.client == nil {
		return nil
	}
	return p.client.Close()
}

// Pull satisfies the ImagePuller interface. It short-circuits when the
// reference is already in the local content store (cache hit) and
// otherwise delegates to containerd's Pull API using a Docker-registry
// resolver configured from authFile.
func (p *ContainerdPuller) Pull(ctx context.Context, ref string) error {
	if p == nil {
		return fmt.Errorf("%w: nil puller", ErrPullFailed)
	}
	if ref == "" {
		return fmt.Errorf("%w: empty reference", ErrPullFailed)
	}

	// Ensure the containerd namespace is bound to the context before
	// any API call — the client.WithDefaultNamespace option only
	// applies when the caller has not already scoped the context.
	ctx = namespaces.WithNamespace(ctx, p.namespace)

	once, result := p.onceFor(ref)
	once.Do(func() {
		result.setErr(p.doPull(ctx, ref))
	})
	return result.err()
}

// onceFor returns a sync.Once unique to the reference plus a results
// cell callers read after Do returns. Serialising pulls of the same
// ref means a burst of Prefetch calls for the same image makes exactly
// one network round-trip.
func (p *ContainerdPuller) onceFor(ref string) (*sync.Once, *pullResult) {
	p.mu.Lock()
	defer p.mu.Unlock()
	once, ok := p.inflight[ref]
	if !ok {
		once = &sync.Once{}
		p.inflight[ref] = once
	}
	return once, &pullResult{p: p, ref: ref}
}

// pullResult is a tiny helper that stores the outcome of a Once'd pull
// in the parent Puller's results map. It exists so callers of Pull()
// return the right error even after Once.Do has already fired.
type pullResult struct {
	p   *ContainerdPuller
	ref string
}

func (r *pullResult) setErr(err error) {
	r.p.mu.Lock()
	defer r.p.mu.Unlock()
	r.p.results[r.ref] = err
}

func (r *pullResult) err() error {
	r.p.mu.Lock()
	defer r.p.mu.Unlock()
	return r.p.results[r.ref]
}

// doPull checks the local content store for the reference, returns
// immediately on cache hit, and otherwise dispatches to the containerd
// Pull API. All containerd errors are classified into typed sentinels
// before they reach the caller.
func (p *ContainerdPuller) doPull(ctx context.Context, ref string) error {
	if _, err := p.client.GetImage(ctx, ref); err == nil {
		// Cache hit — image already present in the content store.
		return nil
	} else if !errdefs.IsNotFound(err) && !isNotFoundLike(err) {
		// Anything other than NotFound from GetImage almost always
		// means the connection to containerd is dead; surface as
		// unreachable so the pool Manager retries.
		return fmt.Errorf("%w: GetImage %q: %v", ErrContainerdUnreachable, ref, err)
	}

	resolver, err := p.buildResolver()
	if err != nil {
		return fmt.Errorf("%w: build resolver: %v", ErrPullFailed, err)
	}

	if err := p.client.Pull(ctx, ref, resolver); err != nil {
		return classifyPullError(ref, err)
	}
	return nil
}

// buildResolver constructs a Docker-registry resolver using credentials
// from authFile (a Docker config.json) when non-empty, else returns a
// resolver configured for anonymous access.
func (p *ContainerdPuller) buildResolver() (remotes.Resolver, error) {
	if p.authFile == "" {
		return docker.NewResolver(docker.ResolverOptions{}), nil
	}
	creds, err := loadDockerAuth(p.authFile)
	if err != nil {
		return nil, err
	}
	authorizer := docker.NewDockerAuthorizer(docker.WithAuthCreds(func(host string) (string, string, error) {
		u, p := lookupDockerCreds(creds, host)
		return u, p, nil
	}))
	return docker.NewResolver(docker.ResolverOptions{
		Hosts: docker.ConfigureDefaultRegistries(
			docker.WithAuthorizer(authorizer),
		),
	}), nil
}

// dockerAuthEntry holds a host-scoped credential pair from a Docker
// config.json.
type dockerAuthEntry struct {
	username string
	password string
}

// lookupDockerCreds returns (username, password) for the given
// registry host, falling back to the docker.io index alias when the
// caller asks for registry-1.docker.io. Returning empty strings means
// anonymous access.
func lookupDockerCreds(creds map[string]dockerAuthEntry, host string) (string, string) {
	if c, ok := creds[host]; ok {
		return c.username, c.password
	}
	if host == "registry-1.docker.io" {
		if c, ok := creds["https://index.docker.io/v1/"]; ok {
			return c.username, c.password
		}
	}
	return "", ""
}

// loadDockerAuth parses a Docker config.json and returns its auths
// section as a host→credential map. The file's `auths` entries may
// carry either a base64-encoded "username:password" string or
// separate username / password fields; both shapes are supported.
func loadDockerAuth(path string) (map[string]dockerAuthEntry, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read auth file %q: %w", path, err)
	}
	var parsed struct {
		Auths map[string]struct {
			Auth     string `json:"auth"`
			Username string `json:"username"`
			Password string `json:"password"`
		} `json:"auths"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("parse auth file %q: %w", path, err)
	}
	out := make(map[string]dockerAuthEntry, len(parsed.Auths))
	for host, entry := range parsed.Auths {
		user, pass := entry.Username, entry.Password
		if entry.Auth != "" && (user == "" || pass == "") {
			decoded, err := base64.StdEncoding.DecodeString(entry.Auth)
			if err != nil {
				return nil, fmt.Errorf("decode auth for %q: %w", host, err)
			}
			if idx := strings.IndexByte(string(decoded), ':'); idx > 0 {
				user = string(decoded[:idx])
				pass = string(decoded[idx+1:])
			}
		}
		out[host] = dockerAuthEntry{username: user, password: pass}
	}
	return out, nil
}

// classifyPullError inspects a containerd Pull error and returns the
// best-matching typed sentinel. Callers use errors.Is to distinguish
// the cases; the underlying error is always wrapped so detailed
// diagnostics survive in log output.
func classifyPullError(ref string, err error) error {
	switch {
	case err == nil:
		return nil
	case errdefs.IsNotFound(err) || isNotFoundLike(err):
		return fmt.Errorf("%w: %q: %v", ErrImageNotFound, ref, err)
	case errdefs.IsUnauthorized(err) || errdefs.IsPermissionDenied(err) || isAuthErrorLike(err):
		return fmt.Errorf("%w: %q: %v", ErrAuthRequired, ref, err)
	case errdefs.IsUnavailable(err) || isUnreachableLike(err):
		return fmt.Errorf("%w: %q: %v", ErrContainerdUnreachable, ref, err)
	default:
		return fmt.Errorf("%w: %q: %v", ErrPullFailed, ref, err)
	}
}

// isNotFoundLike matches error messages that indicate 404 from a
// registry even when the containerd error classification is coarser
// than we need. Kept narrow — only substrings that cannot plausibly
// appear in other error paths.
func isNotFoundLike(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "not found") || strings.Contains(msg, "404")
}

// isAuthErrorLike matches the textual forms docker registries use for
// auth failures that have not been classified via errdefs yet.
func isAuthErrorLike(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unauthorized") ||
		strings.Contains(msg, "401") ||
		strings.Contains(msg, "403") ||
		strings.Contains(msg, "authentication required")
}

// isUnreachableLike matches error strings that indicate the containerd
// daemon is not reachable (socket missing, RPC drained). Helps keep
// classification stable across containerd client versions.
func isUnreachableLike(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "no such file") ||
		strings.Contains(msg, "socket")
}

// containerdClientAdapter wraps *client.Client so it satisfies the
// narrow containerdClient interface. Kept private so test fakes don't
// need to import the containerd client package.
type containerdClientAdapter struct {
	c *client.Client
}

func (a *containerdClientAdapter) GetImage(ctx context.Context, ref string) (images.Image, error) {
	img, err := a.c.ImageService().Get(ctx, ref)
	if err != nil {
		return images.Image{}, err
	}
	return img, nil
}

func (a *containerdClientAdapter) Pull(ctx context.Context, ref string, resolver remotes.Resolver) error {
	_, err := a.c.Pull(ctx, ref, client.WithPullUnpack, client.WithResolver(resolver))
	return err
}

func (a *containerdClientAdapter) Close() error {
	return a.c.Close()
}

// Ensure ContainerdPuller satisfies the ImagePuller interface at
// compile time. A broken refactor of the interface becomes a build
// failure, not a runtime surprise at pool-fill time.
var _ ImagePuller = (*ContainerdPuller)(nil)
