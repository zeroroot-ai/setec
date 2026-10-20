// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package main

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc/codes"

	"github.com/zeroroot-ai/setec/internal/frontend"
)

func answer(code codes.Code, msg string) attemptFunc {
	return func(context.Context) (codes.Code, string, error) { return code, msg, nil }
}

// TestRunAccepted pins what the client counts as accepted. A caller that the
// frontend does not enroll fails, even though TLS passed.
func TestRunAccepted(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		try  attemptFunc
		want int
	}{
		{"unenrolled caller", answer(codes.PermissionDenied, "caller spiffe://a.test/x "+frontend.NotEnrolledText), 1},
		{"no accessible namespace", answer(codes.PermissionDenied, "pair a/t "+frontend.NoNamespaceText+": boom"), 0},
		{"another namespace", answer(codes.PermissionDenied, "the caller "+frontend.NotOwnerText+" \"x\""), 0},
		{"past auth", answer(codes.NotFound, "get Sandbox: not found"), 0},
		{"ok answer", answer(codes.OK, ""), 0},
		{"answer that proves nothing", answer(codes.InvalidArgument, "bad id"), 1},
		{"other permission denied", answer(codes.PermissionDenied, "the client certificate carries no SPIFFE ID"), 1},
		{"never answers", answer(codes.Unavailable, "remote error: tls: bad certificate"), 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := runAccepted(tc.try, 30*time.Millisecond, 10*time.Millisecond); got != tc.want {
				t.Fatalf("runAccepted = %d, want %d", got, tc.want)
			}
		})
	}
}
