// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package netpol

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sort"
	"sync"
	"time"
)

// HostResolver turns a declared egress host name into the address blocks a
// NetworkPolicy may name.
//
// Kubernetes NetworkPolicy has no hostname matcher. Enforcing an
// allow-list entry like "api.example.com:443" therefore means resolving
// the name and writing the answer into the rule as ipBlock peers. The
// interface exists so the translator stays testable without DNS and so a
// deployment can substitute a different source of truth.
type HostResolver interface {
	// Resolve returns the CIDR blocks host currently maps to, as
	// single-address prefixes. An empty result with a nil error means the
	// name resolved to nothing.
	Resolve(ctx context.Context, host string) ([]string, error)
}

// ErrResolveFailed wraps every lookup failure surfaced by CachingResolver
// so callers can distinguish "DNS did not answer" from a malformed
// declaration.
var ErrResolveFailed = errors.New("netpol: egress host did not resolve")

// Default timings for CachingResolver. They are exported so the operator's
// flags can document the same numbers the code defaults to.
const (
	// DefaultResolveTTL is how long a successful answer is reused before
	// the resolver looks the name up again. A policy is only as current
	// as its last lookup, so this is also the worst-case lag between a
	// destination changing address and the policy following it.
	DefaultResolveTTL = 60 * time.Second

	// DefaultResolveGrace is how long a previously-good answer keeps
	// being served after lookups start failing.
	//
	// Without a grace window a transient resolver outage would translate
	// straight into "this Sandbox's egress rule grants nothing", cutting
	// off running workloads for a DNS blip. With it, a blip costs
	// staleness rather than an outage, and a sustained failure still ends
	// in the rule being withdrawn — which is the correct terminal state,
	// because a destination the operator can no longer locate is one it
	// cannot write a rule for.
	DefaultResolveGrace = 10 * time.Minute

	// DefaultResolveTimeout bounds a single lookup. A reconcile must not
	// block on an unresponsive resolver.
	DefaultResolveTimeout = 5 * time.Second
)

// LookupFunc is the DNS primitive CachingResolver is built on. It matches
// the shape of net.Resolver.LookupNetIP with the network argument bound.
type LookupFunc func(ctx context.Context, host string) ([]netip.Addr, error)

// CachingResolver resolves egress host names and remembers the answers.
//
// It exists because the policy is a long-lived object rebuilt on every
// reconcile: without a cache, every reconcile of every Sandbox would issue
// a fresh lookup, and a slow resolver would show up as reconcile latency.
// With one, a reconcile normally reads memory and the lookup happens at
// most once per TTL per name.
//
// The cache is also what makes failure survivable. A cached answer stays
// usable for Grace after lookups start failing, so a resolver blip does
// not immediately withdraw the egress rules of running Sandboxes.
//
// The zero value is not usable; construct with NewCachingResolver.
type CachingResolver struct {
	lookup  LookupFunc
	ttl     time.Duration
	grace   time.Duration
	timeout time.Duration
	now     func() time.Time

	mu      sync.Mutex
	entries map[string]resolvedEntry
}

// resolvedEntry is one cached answer.
type resolvedEntry struct {
	// prefixes is the last answer that succeeded, as single-address CIDRs.
	prefixes []string
	// at is when that answer was obtained.
	at time.Time
}

// ResolverOptions configures a CachingResolver. The zero value selects the
// package defaults for every field.
type ResolverOptions struct {
	// TTL is how long a successful answer is reused. Zero selects
	// DefaultResolveTTL.
	TTL time.Duration

	// Grace is how long a stale answer survives lookup failure. Zero
	// selects DefaultResolveGrace.
	Grace time.Duration

	// Timeout bounds a single lookup. Zero selects
	// DefaultResolveTimeout.
	Timeout time.Duration

	// Lookup overrides the DNS primitive. Nil selects the process
	// resolver. Tests set this; production does not.
	Lookup LookupFunc

	// Now overrides the clock. Nil selects time.Now. Tests set this;
	// production does not.
	Now func() time.Time
}

// NewCachingResolver builds a CachingResolver from opts, filling every
// unset field with its documented default.
func NewCachingResolver(opts ResolverOptions) *CachingResolver {
	r := &CachingResolver{
		lookup:  opts.Lookup,
		ttl:     opts.TTL,
		grace:   opts.Grace,
		timeout: opts.Timeout,
		now:     opts.Now,
		entries: make(map[string]resolvedEntry),
	}
	if r.lookup == nil {
		r.lookup = func(ctx context.Context, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip4", host)
		}
	}
	if r.ttl <= 0 {
		r.ttl = DefaultResolveTTL
	}
	if r.grace <= 0 {
		r.grace = DefaultResolveGrace
	}
	if r.timeout <= 0 {
		r.timeout = DefaultResolveTimeout
	}
	if r.now == nil {
		r.now = time.Now
	}
	return r
}

// Resolve implements HostResolver.
//
// The result is sorted and de-duplicated so that an unchanged DNS answer
// produces a byte-identical policy on every reconcile. Multi-address
// records commonly come back in rotated order, and without normalisation
// that rotation alone would make the reconciler patch the NetworkPolicy on
// every pass.
func (r *CachingResolver) Resolve(ctx context.Context, host string) ([]string, error) {
	now := r.now()

	if cached, ok := r.fresh(host, now); ok {
		return cached, nil
	}

	lookupCtx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	addrs, err := r.lookup(lookupCtx, host)
	if err != nil {
		// Serve the last good answer while it is inside the grace window.
		// Beyond it, report the failure and let the caller fail closed.
		if stale, ok := r.stale(host, now); ok {
			return stale, nil
		}
		return nil, fmt.Errorf("%w: %q: %w", ErrResolveFailed, host, err)
	}

	prefixes := normalisePrefixes(addrs)
	r.store(host, prefixes, now)
	return prefixes, nil
}

// fresh returns a cached answer that is still inside the TTL.
func (r *CachingResolver) fresh(host string, now time.Time) ([]string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.entries[host]
	if !ok || now.Sub(e.at) >= r.ttl {
		return nil, false
	}
	return append([]string(nil), e.prefixes...), true
}

// stale returns a cached answer that is past its TTL but inside the grace
// window.
func (r *CachingResolver) stale(host string, now time.Time) ([]string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.entries[host]
	if !ok || now.Sub(e.at) >= r.grace {
		return nil, false
	}
	return append([]string(nil), e.prefixes...), true
}

// store records a successful answer.
func (r *CachingResolver) store(host string, prefixes []string, now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries[host] = resolvedEntry{prefixes: prefixes, at: now}
}

// normalisePrefixes renders resolved addresses as single-host prefixes,
// de-duplicated and sorted. See Resolve for why the ordering matters.
func normalisePrefixes(addrs []netip.Addr) []string {
	seen := make(map[string]struct{}, len(addrs))
	out := make([]string, 0, len(addrs))
	for _, a := range addrs {
		a = a.Unmap()
		p := netip.PrefixFrom(a, a.BitLen()).String()
		if _, dup := seen[p]; dup {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}
