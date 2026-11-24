// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package credentials

import (
	"errors"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestWatchReporter(t *testing.T) {
	t.Parallel()
	var out strings.Builder
	clock := time.Unix(1000, 0)
	report := newWatchReporter(&out, func() time.Time { return clock })

	denied := status.Error(codes.PermissionDenied, "no identity issued")
	report(denied)
	if got := out.String(); !strings.Contains(got, "waiting for SPIRE to issue an identity") ||
		!strings.Contains(got, "no identity issued") {
		t.Fatalf("a refusal of an unregistered workload is not named as a wait: %q", got)
	}

	out.Reset()
	report(denied)
	if out.Len() != 0 {
		t.Errorf("the same line within the interval is written again: %q", out.String())
	}

	// A fault of another kind is not hidden by the wait line.
	report(errors.New("connection refused"))
	if !strings.Contains(out.String(), "SPIFFE Workload API failure: connection refused") {
		t.Errorf("a fault is not reported: %q", out.String())
	}

	out.Reset()
	clock = clock.Add(reportInterval)
	report(denied)
	if !strings.Contains(out.String(), "waiting for SPIRE") {
		t.Errorf("the wait line does not come back after the interval: %q", out.String())
	}
}

func TestNoIdentityIssued(t *testing.T) {
	t.Parallel()
	if noIdentityIssued(errors.New("plain")) {
		t.Error("a plain error is a wait")
	}
	if !noIdentityIssued(status.Error(codes.PermissionDenied, "x")) {
		t.Error("PermissionDenied is not a wait")
	}
	if noIdentityIssued(status.Error(codes.Unavailable, "x")) {
		t.Error("Unavailable is a wait")
	}
}

// TestStartupWaitBoundsTheFirstFetch: with no deadline on the context, the
// wait for the first SVID ends after StartupWait and the error names the socket.
func TestStartupWaitBoundsTheFirstFetch(t *testing.T) {
	t.Parallel()
	socket := t.TempDir() + "/absent.sock"
	p, err := New(SPIFFESource{
		SocketPath:      socket,
		AuthorizedIDs:   []string{"spiffe://example.org/ns/setec/sa/caller"},
		StartupWait:     300 * time.Millisecond,
		OnRotationError: func(error) {},
	})
	if err != nil {
		t.Fatal(err)
	}
	begin := time.Now()
	_, err = p.ServerCredentials(t.Context())
	if err == nil || !strings.Contains(err.Error(), socket) {
		t.Fatalf("want an error that names the socket, got %v", err)
	}
	if d := time.Since(begin); d > 5*time.Second {
		t.Errorf("the wait took %s, want about StartupWait", d)
	}
}
