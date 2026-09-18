// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

//go:build e2e

// Session exec against a live cluster (setec#8, the residual acceptance
// criterion of setec#239). The ABI landed with unit tests behind a
// containerExecutor seam; these scenarios drive the real pods/exec path.
//
// They run on any backend, kata-fc on metal or runc on kind: Exec rides
// the CRI exec path (ADR-0008), which is backend-agnostic, and the
// properties asserted here belong to the session lifecycle and the exit
// contract, not to the isolation backend.
package e2e

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/controller-runtime/pkg/client"

	setecv1grpc "github.com/zeroroot-ai/setec/api/grpc/v1"
	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
	"github.com/zeroroot-ai/setec/internal/frontend"
)

// execStream is an in-memory SandboxService_ExecServer. It feeds a scripted
// client half and records everything the server sends back, so the
// frontend can be driven in-process exactly as session_reattach_test.go
// drives Attach: no gRPC listener, no mTLS, no deployed frontend.
type execStream struct {
	setecv1grpc.SandboxService_ExecServer

	ctx  context.Context
	in   []*setecv1grpc.SandboxServiceExecRequest
	next int
	out  []*setecv1grpc.SandboxServiceExecResponse
}

func (s *execStream) Context() context.Context { return s.ctx }

func (s *execStream) Send(m *setecv1grpc.SandboxServiceExecResponse) error {
	s.out = append(s.out, m)
	return nil
}

func (s *execStream) Recv() (*setecv1grpc.SandboxServiceExecRequest, error) {
	if s.next >= len(s.in) {
		// The client half is done. EOF is the half-close, not a failure.
		return nil, io.EOF
	}
	m := s.in[s.next]
	s.next++
	return m, nil
}

// execResult is the flattened outcome of one Exec.
type execResult struct {
	stdout string
	stderr string
	exit   *setecv1grpc.SessionExecExit
}

// execTimeout bounds one Exec stream. Long enough for a microVM to run
// a short command, and the only thing that ends a command the session
// never terminates.
const execTimeout = 2 * time.Minute

// sessionExec runs argv in the session named by handle through fe and
// returns everything the stream produced. The stream must end with exactly
// one exit: a stream that ends without one is the confusion
// SessionExecExit.Status exists to prevent, so it fails the test here.
func sessionExec(t *testing.T, fe *frontend.Service, handle string, argv ...string) execResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), execTimeout)
	defer cancel()
	res, err := runSessionExec(ctx, fe, handle, argv...)
	if err != nil {
		t.Fatalf("Exec(%v): %v", argv, err)
	}
	if res.exit == nil {
		t.Fatalf("Exec(%v): stream ended with no exit status; stdout=%q stderr=%q", argv, res.stdout, res.stderr)
	}
	return res
}

// runSessionExec is sessionExec without the test assertions, for a caller
// that runs the command in the background and judges the outcome itself.
func runSessionExec(ctx context.Context, fe *frontend.Service, handle string, argv ...string) (execResult, error) {
	stream := &execStream{
		ctx: ctx,
		in: []*setecv1grpc.SandboxServiceExecRequest{
			{Request: &setecv1grpc.SandboxServiceExecRequest_Start{
				Start: &setecv1grpc.SessionExecStart{SandboxId: handle, Command: argv},
			}},
			// Close stdin at once: nothing here feeds input, and a command
			// that reads to EOF would otherwise hang for the whole timeout.
			{Request: &setecv1grpc.SandboxServiceExecRequest_StdinEof{StdinEof: true}},
		},
	}
	var res execResult
	if err := fe.Exec(stream); err != nil {
		return res, err
	}
	for _, m := range stream.out {
		switch r := m.GetResponse().(type) {
		case *setecv1grpc.SandboxServiceExecResponse_Output:
			if r.Output.GetStream() == "stderr" {
				res.stderr += string(r.Output.GetData())
			} else {
				res.stdout += string(r.Output.GetData())
			}
		case *setecv1grpc.SandboxServiceExecResponse_Exit:
			if res.exit != nil {
				return res, fmt.Errorf("more than one terminal exit on the stream")
			}
			res.exit = r.Exit
		}
	}
	return res, nil
}

// requireCleanExit fails unless the command ran to completion with code 0.
func requireCleanExit(t *testing.T, what string, res execResult) {
	t.Helper()
	if got := res.exit.GetStatus(); got != setecv1grpc.SessionExecExit_STATUS_EXITED {
		t.Fatalf("%s: status=%s (%q) — the command did not run to completion; stderr=%q",
			what, got, res.exit.GetMessage(), res.stderr)
	}
	if code := res.exit.GetExitCode(); code != 0 {
		t.Fatalf("%s: exit code %d; stdout=%q stderr=%q", what, code, res.stdout, res.stderr)
	}
}

// launchSession creates a session Sandbox with a durable /workspace on the
// suite's session class, waits for Running, and returns its handle.
func launchSession(t *testing.T, name string) string {
	t.Helper()
	installSessionClass(t)

	size := resource.MustParse("1Gi")
	spec := minimalSpec("/bin/sh", "-c", "sleep 3600")
	spec.SandboxClassName = sessionClassName()
	spec.Lifecycle = &setecv1alpha1.Lifecycle{
		Mode:      setecv1alpha1.LifecycleModeSession,
		Workspace: &setecv1alpha1.WorkspaceSpec{Size: &size},
	}
	sb := newSandbox(name, spec)
	createAndCleanup(t, sb)

	key := client.ObjectKeyFromObject(sb)
	waitForPhase(t, key, defaultWait, setecv1alpha1.SandboxPhaseRunning)

	var current setecv1alpha1.Sandbox
	if err := k8sClient.Get(context.Background(), key, &current); err != nil {
		t.Fatalf("get session sandbox: %v", err)
	}
	return fmt.Sprintf("%s/%s/%s", current.Namespace, current.Name, current.UID)
}

// attach resolves the handle through a brand-new frontend instance, which
// is a frontend restart as far as the session is concerned, and returns
// that instance for the commands that follow.
func attach(t *testing.T, handle string) *frontend.Service {
	t.Helper()
	fe := newInProcessFrontend()
	resp, err := fe.Attach(context.Background(), &setecv1grpc.AttachRequest{SandboxId: handle})
	if err != nil {
		t.Fatalf("Attach %s: %v", handle, err)
	}
	if resp.GetPhase() != string(setecv1alpha1.SandboxPhaseRunning) {
		t.Fatalf("Attach %s: phase = %q, want Running", handle, resp.GetPhase())
	}
	return fe
}

// TestSessionExec_AfterReattach is the acceptance criterion: work done
// in one turn is there in the next, through a fresh connection.
//
//  1. Launch a session Sandbox with a long-lived spec.command.
//  2. Attach by handle.
//  3. Exec `sh -c "echo turn1 > marker"`: STATUS_EXITED, exit code 0.
//  4. Drop the connection (the frontend instance goes away).
//  5. Attach again with the same handle, through a new instance.
//  6. Exec `cat marker`: stdout is turn1, STATUS_EXITED, exit code 0.
//
// `marker` is a relative path. Session Pods are rooted at /workspace
// (internal/podspec/builder.go), so it resolves on the durable volume.
func TestSessionExec_AfterReattach(t *testing.T) {
	handle := launchSession(t, "e2e-exec-reattach")

	turn1 := attach(t, handle)
	write := sessionExec(t, turn1, handle, "sh", "-c", "echo turn1 > marker")
	requireCleanExit(t, "turn 1 (write)", write)

	// The connection is gone: turn1 is never used again, and turn2 is a
	// new instance with no state from it.
	turn2 := attach(t, handle)
	read := sessionExec(t, turn2, handle, "cat", "marker")
	requireCleanExit(t, "turn 2 (read)", read)
	if got := strings.TrimSpace(read.stdout); got != "turn1" {
		t.Fatalf("turn 2 read %q from /workspace/marker, want %q (stderr=%q)", got, "turn1", read.stderr)
	}
}

// TestSessionExec_NonZeroExitIsDistinct asserts a failing command is
// reported as STATUS_EXITED with its real code, distinguishable from every
// unknown-outcome status. `false` exits 1 on every coreutils and busybox.
func TestSessionExec_NonZeroExitIsDistinct(t *testing.T) {
	handle := launchSession(t, "e2e-exec-false")
	fe := attach(t, handle)

	res := sessionExec(t, fe, handle, "false")
	if got := res.exit.GetStatus(); got != setecv1grpc.SessionExecExit_STATUS_EXITED {
		t.Fatalf("false: status=%s (%q), want STATUS_EXITED", got, res.exit.GetMessage())
	}
	if code := res.exit.GetExitCode(); code != 1 {
		t.Fatalf("false: exit code %d, want 1", code)
	}
}

// TestSessionExec_KillMidExec is the case gibson depends on most
// (gibson#1183): a session reaped while a command runs must not look like
// a command that failed. The stream must end with STATUS_SANDBOX_GONE,
// not with a bare close and not with an exit code.
func TestSessionExec_KillMidExec(t *testing.T) {
	handle := launchSession(t, "e2e-exec-kill")
	fe := attach(t, handle)

	type outcome struct {
		res execResult
		err error
	}
	done := make(chan outcome, 1)
	ctx, cancel := context.WithTimeout(context.Background(), execTimeout)
	defer cancel()
	go func() {
		res, err := runSessionExec(ctx, fe, handle, "sleep", "300")
		done <- outcome{res: res, err: err}
	}()

	// The command is in flight once the frontend has stamped the
	// session's activity annotation for it; that is the same signal the
	// idle evictor reads. Poll for a stamp newer than the Attach's.
	waitForExecInFlight(t, handle)

	if _, err := newInProcessFrontend().Kill(context.Background(), &setecv1grpc.KillRequest{SandboxId: handle}); err != nil {
		t.Fatalf("Kill %s: %v", handle, err)
	}

	var got outcome
	select {
	case got = <-done:
	case <-time.After(execTimeout):
		t.Fatalf("Exec did not end within %s after the session was killed", execTimeout)
	}
	if got.err != nil {
		t.Fatalf("Exec returned an error instead of a typed exit after Kill: %v", got.err)
	}
	if got.res.exit == nil {
		t.Fatalf("Exec stream ended with a bare close after Kill; want STATUS_SANDBOX_GONE (stdout=%q stderr=%q)",
			got.res.stdout, got.res.stderr)
	}
	if status := got.res.exit.GetStatus(); status != setecv1grpc.SessionExecExit_STATUS_SANDBOX_GONE {
		t.Fatalf("Exec after Kill: status=%s (%q), want STATUS_SANDBOX_GONE", status, got.res.exit.GetMessage())
	}
	if code := got.res.exit.GetExitCode(); code != 0 {
		t.Fatalf("Exec after Kill carries exit code %d; a gone sandbox has no code", code)
	}
}

// waitForExecInFlight polls the session's last-activity annotation until
// it moves past the value the Attach left, which the frontend does when
// an Exec starts and holds for the command's whole run.
func waitForExecInFlight(t *testing.T, handle string) {
	t.Helper()
	parts := strings.Split(handle, "/")
	key := client.ObjectKey{Namespace: parts[0], Name: parts[1]}

	var before setecv1alpha1.Sandbox
	if err := k8sClient.Get(context.Background(), key, &before); err != nil {
		t.Fatalf("get session sandbox: %v", err)
	}
	was := before.Annotations[setecv1alpha1.AnnotationLastActivity]

	deadline := time.Now().Add(briefWait)
	for time.Now().Before(deadline) {
		var now setecv1alpha1.Sandbox
		if err := k8sClient.Get(context.Background(), key, &now); err != nil {
			t.Fatalf("get session sandbox: %v", err)
		}
		if stamp := now.Annotations[setecv1alpha1.AnnotationLastActivity]; stamp != "" && stamp != was {
			return
		}
		time.Sleep(defaultPoll)
	}
	t.Fatalf("no activity stamp from the in-flight Exec within %s; cannot prove the kill lands mid-command", briefWait)
}
