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

package netpol

import (
	"context"
	"errors"
	"net/netip"
	"slices"
	"sync"
	"testing"
	"time"
)

// fakeClock is a manually advanced clock. The resolver's whole contract is
// about time — TTL expiry and the grace window — and a test that slept for
// it would be both slow and flaky.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// scriptedLookup is a LookupFunc whose answer and error the test controls,
// and which counts how many times it was called.
type scriptedLookup struct {
	mu    sync.Mutex
	addrs []netip.Addr
	err   error
	calls int
}

func (s *scriptedLookup) fn(_ context.Context, _ string) ([]netip.Addr, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	return slices.Clone(s.addrs), nil
}

func (s *scriptedLookup) set(addrs []netip.Addr, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.addrs, s.err = addrs, err
}

func (s *scriptedLookup) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func addrs(ss ...string) []netip.Addr {
	out := make([]netip.Addr, 0, len(ss))
	for _, s := range ss {
		out = append(out, netip.MustParseAddr(s))
	}
	return out
}

// newTestResolver wires a CachingResolver to a fake clock and a scripted
// lookup.
func newTestResolver(t *testing.T, ttl, grace time.Duration) (*CachingResolver, *scriptedLookup, *fakeClock) {
	t.Helper()
	lk := &scriptedLookup{}
	clk := newFakeClock()
	r := NewCachingResolver(ResolverOptions{
		TTL:    ttl,
		Grace:  grace,
		Lookup: lk.fn,
		Now:    clk.Now,
	})
	return r, lk, clk
}

// TestResolve_ReturnsSortedHostPrefixes covers the normalisation that
// makes a policy stable. A rotated DNS answer must not produce a different
// policy, or the reconciler would patch the NetworkPolicy on every pass
// forever.
func TestResolve_ReturnsSortedHostPrefixes(t *testing.T) {
	t.Parallel()

	r, lk, _ := newTestResolver(t, time.Minute, time.Hour)
	lk.set(addrs("203.0.113.20", "203.0.113.10", "203.0.113.20"), nil)

	got, err := r.Resolve(context.Background(), "api.example.com")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	want := []string{"203.0.113.10/32", "203.0.113.20/32"}
	if !slices.Equal(got, want) {
		t.Errorf("Resolve() = %v, want %v (sorted and de-duplicated)", got, want)
	}
}

// TestResolve_CachesWithinTTL proves the cache exists. Without it every
// reconcile of every Sandbox would issue a lookup.
func TestResolve_CachesWithinTTL(t *testing.T) {
	t.Parallel()

	r, lk, clk := newTestResolver(t, time.Minute, time.Hour)
	lk.set(addrs("203.0.113.10"), nil)

	for range 5 {
		if _, err := r.Resolve(context.Background(), "api.example.com"); err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		clk.advance(10 * time.Second)
	}
	if lk.callCount() != 1 {
		t.Errorf("lookup called %d times inside one TTL, want 1", lk.callCount())
	}
}

// TestResolve_RefreshesAfterTTL is the other half: an answer must not be
// cached forever, or the policy would freeze at whatever the destination's
// address was when the Sandbox started.
func TestResolve_RefreshesAfterTTL(t *testing.T) {
	t.Parallel()

	r, lk, clk := newTestResolver(t, time.Minute, time.Hour)
	lk.set(addrs("203.0.113.10"), nil)

	if _, err := r.Resolve(context.Background(), "api.example.com"); err != nil {
		t.Fatalf("first Resolve: %v", err)
	}

	clk.advance(time.Minute + time.Second)
	lk.set(addrs("198.51.100.7"), nil)

	got, err := r.Resolve(context.Background(), "api.example.com")
	if err != nil {
		t.Fatalf("second Resolve: %v", err)
	}
	if want := []string{"198.51.100.7/32"}; !slices.Equal(got, want) {
		t.Errorf("Resolve() after TTL = %v, want %v", got, want)
	}
	if lk.callCount() != 2 {
		t.Errorf("lookup called %d times across a TTL boundary, want 2", lk.callCount())
	}
}

// TestResolve_ServesStaleInsideGrace covers the failure mode that matters
// operationally: a resolver blip must cost staleness, not the egress of
// every running Sandbox.
func TestResolve_ServesStaleInsideGrace(t *testing.T) {
	t.Parallel()

	r, lk, clk := newTestResolver(t, time.Minute, 10*time.Minute)
	lk.set(addrs("203.0.113.10"), nil)

	if _, err := r.Resolve(context.Background(), "api.example.com"); err != nil {
		t.Fatalf("first Resolve: %v", err)
	}

	clk.advance(2 * time.Minute) // past the TTL, inside the grace window
	lk.set(nil, errors.New("dial udp 1.1.1.1:53: i/o timeout"))

	got, err := r.Resolve(context.Background(), "api.example.com")
	if err != nil {
		t.Fatalf("Resolve inside grace window returned an error: %v", err)
	}
	if want := []string{"203.0.113.10/32"}; !slices.Equal(got, want) {
		t.Errorf("Resolve() inside grace = %v, want the last good answer %v", got, want)
	}
}

// TestResolve_FailsPastGrace is the terminal state. A destination the
// operator has been unable to locate for the whole grace window is one it
// cannot write a rule for, and the caller must fail closed rather than
// keep an indefinitely stale address allowed.
func TestResolve_FailsPastGrace(t *testing.T) {
	t.Parallel()

	r, lk, clk := newTestResolver(t, time.Minute, 10*time.Minute)
	lk.set(addrs("203.0.113.10"), nil)

	if _, err := r.Resolve(context.Background(), "api.example.com"); err != nil {
		t.Fatalf("first Resolve: %v", err)
	}

	clk.advance(11 * time.Minute)
	lk.set(nil, errors.New("dial udp 1.1.1.1:53: i/o timeout"))

	if _, err := r.Resolve(context.Background(), "api.example.com"); !errors.Is(err, ErrResolveFailed) {
		t.Errorf("Resolve() past grace err = %v, want one wrapping ErrResolveFailed", err)
	}
}

// TestResolve_FirstLookupFailureHasNoFallback: a name that never resolved
// has no last-good answer to serve, so it fails immediately rather than
// waiting out a grace window it was never inside.
func TestResolve_FirstLookupFailureHasNoFallback(t *testing.T) {
	t.Parallel()

	r, lk, _ := newTestResolver(t, time.Minute, 10*time.Minute)
	lk.set(nil, errors.New("no such host"))

	if _, err := r.Resolve(context.Background(), "nope.example.com"); !errors.Is(err, ErrResolveFailed) {
		t.Errorf("Resolve() err = %v, want one wrapping ErrResolveFailed", err)
	}
}

// TestResolve_EmptyAnswerIsNotAnError distinguishes "the name answered
// with nothing" from "the lookup failed". The generator treats both as a
// dropped entry, but only one of them is a resolver problem.
func TestResolve_EmptyAnswerIsNotAnError(t *testing.T) {
	t.Parallel()

	r, lk, _ := newTestResolver(t, time.Minute, time.Hour)
	lk.set(nil, nil)

	got, err := r.Resolve(context.Background(), "empty.example.com")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("Resolve() = %v, want empty", got)
	}
}

// TestResolve_DefaultsAreApplied pins that the zero-value options do not
// produce a resolver with a zero TTL, which would re-look-up on every
// single call.
func TestResolve_DefaultsAreApplied(t *testing.T) {
	t.Parallel()

	r := NewCachingResolver(ResolverOptions{})
	if r.ttl != DefaultResolveTTL {
		t.Errorf("ttl = %v, want %v", r.ttl, DefaultResolveTTL)
	}
	if r.grace != DefaultResolveGrace {
		t.Errorf("grace = %v, want %v", r.grace, DefaultResolveGrace)
	}
	if r.timeout != DefaultResolveTimeout {
		t.Errorf("timeout = %v, want %v", r.timeout, DefaultResolveTimeout)
	}
	if r.lookup == nil || r.now == nil {
		t.Error("lookup and now must default to non-nil")
	}
}

// TestConfig_EffectiveRefreshInterval covers the small accessor the
// controller uses to decide its requeue cadence.
func TestConfig_EffectiveRefreshInterval(t *testing.T) {
	t.Parallel()

	if got := (Config{}).EffectiveRefreshInterval(); got != DefaultRefreshInterval {
		t.Errorf("unset EffectiveRefreshInterval() = %v, want %v", got, DefaultRefreshInterval)
	}
	if got := (Config{RefreshInterval: 5 * time.Minute}).EffectiveRefreshInterval(); got != 5*time.Minute {
		t.Errorf("EffectiveRefreshInterval() = %v, want 5m", got)
	}
}
