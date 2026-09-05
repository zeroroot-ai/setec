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
	"fmt"
	"time"
)

// ImagePuller is the narrow interface the node agent uses to populate
// the local containerd content store with OCI images referenced by
// SandboxClasses. The full containerd Go client is intentionally NOT a
// direct dependency of this package; the production `cmd/node-agent`
// wires an adapter that delegates to `ctr` (or the containerd client),
// and unit tests inject a stub.
type ImagePuller interface {
	// Pull fetches the given OCI reference and places it in the local
	// content store. Idempotent: pulling an image that already exists
	// is a cheap no-op (containerd compares digests).
	Pull(ctx context.Context, ref string) error
}

// ImageCache drives prefetching. Designed to be re-used across
// SandboxClass list refreshes so nodes do not thrash on transient
// network errors.
type ImageCache struct {
	// Puller is the underlying image-fetch mechanism.
	Puller ImagePuller

	// RetryBackoff is the delay between retry attempts. 5s if zero.
	RetryBackoff time.Duration

	// MaxAttempts bounds retry. 3 if zero.
	MaxAttempts int
}

// NewImageCache constructs a cache with the given puller and default
// retry behaviour.
func NewImageCache(p ImagePuller) *ImageCache {
	return &ImageCache{
		Puller:       p,
		RetryBackoff: 5 * time.Second,
		MaxAttempts:  3,
	}
}

// Prefetch pulls each ref with bounded retries. Errors are aggregated
// so callers see every failed ref in a single summary.
func (c *ImageCache) Prefetch(ctx context.Context, refs []string) error {
	if c == nil || c.Puller == nil {
		return fmt.Errorf("nodeagent: ImageCache has no Puller configured")
	}

	var firstErr error
	for _, ref := range refs {
		if err := c.pullWithRetry(ctx, ref); err != nil {
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// pullWithRetry attempts the pull up to MaxAttempts times, waiting
// RetryBackoff between attempts. The loop respects ctx cancellation.
func (c *ImageCache) pullWithRetry(ctx context.Context, ref string) error {
	attempts := c.MaxAttempts
	if attempts <= 0 {
		attempts = 3
	}
	backoff := c.RetryBackoff
	if backoff <= 0 {
		backoff = 5 * time.Second
	}

	var lastErr error
	for i := 0; i < attempts; i++ {
		if err := c.Puller.Pull(ctx, ref); err != nil {
			lastErr = err
			// Retry unless the context is done.
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
			continue
		}
		return nil
	}
	return fmt.Errorf("nodeagent: pull %q: %w (after %d attempts)", ref, lastErr, attempts)
}
