// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package main

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

// TestReadinessFollowsTheIdentity: the agent is alive while it waits for SPIRE
// and is ready only after it holds its identity.
func TestReadinessFollowsTheIdentity(t *testing.T) {
	t.Parallel()
	var ready atomic.Bool
	mux := metricsMux(prometheus.NewRegistry(), &ready)
	code := func(path string) int {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, http.NoBody))
		return rec.Code
	}
	if got := code("/healthz"); got != http.StatusOK {
		t.Errorf("/healthz while it waits = %d, want 200", got)
	}
	if got := code("/readyz"); got != http.StatusServiceUnavailable {
		t.Errorf("/readyz while it waits = %d, want 503", got)
	}
	ready.Store(true)
	if got := code("/readyz"); got != http.StatusOK {
		t.Errorf("/readyz with the identity = %d, want 200", got)
	}
}
