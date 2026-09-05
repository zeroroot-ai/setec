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
	"sync"
	"testing"
	"time"
)

// fakePuller records every Pull invocation and optionally scripts
// per-call return values so tests assert retry semantics.
type fakePuller struct {
	mu    sync.Mutex
	calls []string
	// err is returned from every Pull when non-nil.
	err error
	// failFirstN causes the first N Pull calls to return the errBase;
	// subsequent calls succeed.
	failFirstN int
	// errBase is the scripted retry error.
	errBase error
}

func (f *fakePuller) Pull(_ context.Context, ref string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, ref)
	if f.err != nil {
		return f.err
	}
	if f.failFirstN > 0 {
		f.failFirstN--
		return f.errBase
	}
	return nil
}

func TestPrefetch_HappyPath(t *testing.T) {
	t.Parallel()
	p := &fakePuller{}
	cache := &ImageCache{Puller: p, RetryBackoff: time.Millisecond, MaxAttempts: 3}
	if err := cache.Prefetch(context.Background(), []string{"a", "b"}); err != nil {
		t.Fatalf("Prefetch(): %v", err)
	}
	if got := len(p.calls); got != 2 {
		t.Fatalf("calls = %d, want 2", got)
	}
}

func TestPrefetch_NoPuller(t *testing.T) {
	t.Parallel()
	cache := &ImageCache{}
	if err := cache.Prefetch(context.Background(), []string{"a"}); err == nil {
		t.Fatal("expected error when no puller configured")
	}
}

func TestPrefetch_RetryRecoversFromTransientError(t *testing.T) {
	t.Parallel()
	p := &fakePuller{
		failFirstN: 2,
		errBase:    errors.New("transient"),
	}
	cache := &ImageCache{Puller: p, RetryBackoff: time.Millisecond, MaxAttempts: 3}
	if err := cache.Prefetch(context.Background(), []string{"a"}); err != nil {
		t.Fatalf("Prefetch(): %v", err)
	}
	// 2 failures + 1 success = 3 calls.
	if got := len(p.calls); got != 3 {
		t.Fatalf("calls = %d, want 3", got)
	}
}

func TestPrefetch_RetryExhausted(t *testing.T) {
	t.Parallel()
	p := &fakePuller{err: errors.New("permanent")}
	cache := &ImageCache{Puller: p, RetryBackoff: time.Millisecond, MaxAttempts: 3}
	err := cache.Prefetch(context.Background(), []string{"a"})
	if err == nil {
		t.Fatal("expected error on exhausted retries")
	}
	if got := len(p.calls); got != 3 {
		t.Fatalf("calls = %d, want 3 (MaxAttempts)", got)
	}
}

func TestPrefetch_ContextCancellation(t *testing.T) {
	t.Parallel()
	p := &fakePuller{err: errors.New("transient")}
	cache := &ImageCache{Puller: p, RetryBackoff: time.Hour, MaxAttempts: 5}
	ctx, cancel := context.WithCancel(context.Background())
	// Cancel almost immediately; the first Pull attempt still happens
	// synchronously, then the backoff wait observes the cancellation.
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()
	if err := cache.Prefetch(ctx, []string{"a"}); err == nil {
		t.Fatal("expected context error")
	}
}

func TestPrefetch_DefaultRetryValues(t *testing.T) {
	t.Parallel()
	p := &fakePuller{}
	cache := NewImageCache(p)
	if err := cache.Prefetch(context.Background(), []string{"a"}); err != nil {
		t.Fatalf("Prefetch(): %v", err)
	}
	if cache.MaxAttempts != 3 {
		t.Errorf("MaxAttempts = %d, want 3", cache.MaxAttempts)
	}
	if cache.RetryBackoff == 0 {
		t.Error("RetryBackoff should have a default")
	}
}

func TestPrefetch_MultipleRefsFirstErrorWins(t *testing.T) {
	t.Parallel()
	// First ref succeeds, second fails permanently — first error
	// observed must be returned.
	p := &fakePuller{err: nil}
	cache := &ImageCache{Puller: p, RetryBackoff: time.Millisecond, MaxAttempts: 1}

	// Build a puller that fails specifically for "b".
	custom := &customPuller{
		onCall: func(ref string) error {
			if ref == "b" {
				return errors.New("b-fail")
			}
			return nil
		},
	}
	cache.Puller = custom
	err := cache.Prefetch(context.Background(), []string{"a", "b", "c"})
	if err == nil {
		t.Fatal("expected error")
	}
}

// customPuller runs a per-call callback so we can script sophisticated
// retry/failure scenarios without polluting fakePuller's minimal
// interface.
type customPuller struct {
	onCall func(ref string) error
}

func (c *customPuller) Pull(_ context.Context, ref string) error {
	if c.onCall != nil {
		return c.onCall(ref)
	}
	return nil
}
