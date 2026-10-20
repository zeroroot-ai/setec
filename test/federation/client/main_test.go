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

// sequence returns an attemptFunc that gives the answers in order, then keeps
// giving the last one.
func sequence(answers ...[2]any) attemptFunc {
	i := 0
	return func(context.Context) (codes.Code, string, error) {
		a := answers[min(i, len(answers)-1)]
		i++
		return a[0].(codes.Code), a[1].(string), nil
	}
}

const (
	// The exact texts of charts federation run 39455771427, outside.test caller.
	tlsAlert = `rpc error: code = Unavailable desc = connection error: desc = ` +
		`"error reading server preface: remote error: tls: unknown certificate authority"`
	brokenPipe = `rpc error: code = Unavailable desc = write tcp 10.244.0.178:36560->10.96.148.111:50051: ` +
		`write: broken pipe`
)

// TestRunRefused pins the refusal check. A broken pipe between the alerts of
// the fleet is inconclusive, an answer of the handler fails, and too few
// alerts fail.
func TestRunRefused(t *testing.T) {
	t.Parallel()
	alert := [2]any{codes.Unavailable, tlsAlert}
	pipe := [2]any{codes.Unavailable, brokenPipe}
	deadline := [2]any{codes.DeadlineExceeded, "context deadline exceeded"}
	reset := [2]any{codes.Unavailable, "connection error: desc = \"transport: connection reset by peer\""}
	for _, tc := range []struct {
		name string
		try  attemptFunc
		want int
	}{
		{"alerts with a broken pipe between", sequence(alert, alert, pipe, alert), 0},
		{"alerts with a reset between", sequence(alert, reset, alert, alert), 0},
		{"handler answer", sequence(alert, [2]any{codes.NotFound, "get Sandbox: not found"}), 1},
		{"handler answer after a broken pipe", sequence(pipe, [2]any{codes.PermissionDenied, "x"}), 1},
		{"backend down between alerts", sequence(alert, [2]any{codes.Unavailable, "backend down"}, alert, alert), 1},
		{"deadline between alerts", sequence(alert, deadline, alert, alert), 1},
		{"handler status that holds a tls text", sequence(alert, alert,
			[2]any{codes.Unavailable, "dial backend: remote error: tls: bad certificate"}, alert), 1},
		{"handler status that holds a tls text only", sequence(
			[2]any{codes.Unavailable, "remote error: tls: bad certificate"}), 1},
		{"handler status that holds a broken pipe text", sequence(alert,
			[2]any{codes.Unavailable, "backend: write: broken pipe"}, alert, alert), 1},
		{"too few alerts", sequence(alert, alert, pipe), 1},
		{"client rejects the fleet", sequence([2]any{codes.Unavailable, "x509: certificate signed by unknown authority"}), 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := runRefused(tc.try, 60*time.Millisecond, 5*time.Millisecond, refusalFloor); got != tc.want {
				t.Fatalf("runRefused = %d, want %d", got, tc.want)
			}
		})
	}
}
