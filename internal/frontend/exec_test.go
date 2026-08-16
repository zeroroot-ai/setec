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

package frontend

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/types"
	utilexec "k8s.io/utils/exec"

	setecv1grpc "github.com/zeroroot-ai/setec/api/grpc/v1"
	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
)

// fakeExecStream is an in-memory SandboxService_ExecServer. Client
// messages are queued up front; server messages are collected.
type fakeExecStream struct {
	grpc.ServerStream

	ctx context.Context

	mu   sync.Mutex
	in   []*setecv1grpc.SessionExecRequest
	sent []*setecv1grpc.SessionExecResponse
}

func (f *fakeExecStream) Context() context.Context {
	if f.ctx != nil {
		return f.ctx
	}
	return context.Background()
}

func (f *fakeExecStream) Recv() (*setecv1grpc.SessionExecRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.in) == 0 {
		// Exhausted: the client has half-closed, which is what a
		// well-behaved caller that sends no stdin does immediately.
		return nil, io.EOF
	}
	m := f.in[0]
	f.in = f.in[1:]
	return m, nil
}

func (f *fakeExecStream) Send(m *setecv1grpc.SessionExecResponse) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, m)
	return nil
}

// exits returns every terminal message the server produced.
func (f *fakeExecStream) exits() []*setecv1grpc.SessionExecExit {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*setecv1grpc.SessionExecExit
	for _, m := range f.sent {
		if e := m.GetExit(); e != nil {
			out = append(out, e)
		}
	}
	return out
}

// output concatenates the payloads sent on the named stream.
func (f *fakeExecStream) output(name string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var b strings.Builder
	for _, m := range f.sent {
		if o := m.GetOutput(); o != nil && o.GetStream() == name {
			b.Write(o.GetData())
		}
	}
	return b.String()
}

// stubExecutor records the exec it was asked for and replays a
// scripted result.
type stubExecutor struct {
	mu sync.Mutex

	gotNamespace string
	gotPod       string
	gotContainer string
	gotCommand   []string
	gotStdin     string

	stdout string
	stderr string
	err    error

	// block, when non-nil, is waited on before the exec returns.
	block chan struct{}

	// onExec, when non-nil, runs once the exec has started and before
	// it returns — the window in which a session can be evicted out
	// from under a running command.
	onExec func()
}

func (s *stubExecutor) ExecInContainer(
	ctx context.Context,
	namespace, pod, container string,
	command []string,
	stdin io.Reader,
	stdout, stderr io.Writer,
) error {
	s.mu.Lock()
	s.gotNamespace, s.gotPod, s.gotContainer = namespace, pod, container
	s.gotCommand = append([]string(nil), command...)
	s.mu.Unlock()

	if stdin != nil {
		b, _ := io.ReadAll(stdin)
		s.mu.Lock()
		s.gotStdin = string(b)
		s.mu.Unlock()
	}
	if s.stdout != "" {
		_, _ = io.WriteString(stdout, s.stdout)
	}
	if s.stderr != "" {
		_, _ = io.WriteString(stderr, s.stderr)
	}
	if s.onExec != nil {
		s.onExec()
	}
	if s.block != nil {
		select {
		case <-s.block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return s.err
}

func (s *stubExecutor) stdinSeen() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gotStdin
}

// runningSession builds a session Sandbox already in phase Running.
func runningSession(ns, name, uid string) *setecv1alpha1.Sandbox {
	return sessionCR(ns, name, uid, setecv1alpha1.SandboxPhaseRunning)
}

// execService wires a Service around a fake cluster holding sb, with
// the exec transport stubbed out.
func execService(t *testing.T, ex containerExecutor, sb *setecv1alpha1.Sandbox) *Service {
	t.Helper()
	s := &Service{
		Client:           newClient(t, sb),
		AuthDisabled:     true,
		DefaultNamespace: sb.Namespace,
		execReadyTimeout: 2 * time.Second,
	}
	if ex != nil {
		s.execer = ex
	}
	return s
}

func startMsg(handle string, argv ...string) *setecv1grpc.SessionExecRequest {
	return &setecv1grpc.SessionExecRequest{
		Request: &setecv1grpc.SessionExecRequest_Start{
			Start: &setecv1grpc.SessionExecStart{SandboxId: handle, Command: argv},
		},
	}
}

func stdinMsg(b string) *setecv1grpc.SessionExecRequest {
	return &setecv1grpc.SessionExecRequest{
		Request: &setecv1grpc.SessionExecRequest_Stdin{Stdin: []byte(b)},
	}
}

func stdinEOFMsg() *setecv1grpc.SessionExecRequest {
	return &setecv1grpc.SessionExecRequest{
		Request: &setecv1grpc.SessionExecRequest_StdinEof{StdinEof: true},
	}
}

// TestExec_Success streams stdout and stderr separately and closes
// with exactly one STATUS_EXITED carrying the real exit code.
func TestExec_Success(t *testing.T) {
	sb := runningSession("tenant-a", "sess", "uid-1")
	ex := &stubExecutor{stdout: "hello\n", stderr: "warn\n"}
	svc := execService(t, ex, sb)

	st := &fakeExecStream{in: []*setecv1grpc.SessionExecRequest{
		startMsg("tenant-a/sess/uid-1", "make", "build"),
	}}
	if err := svc.Exec(st); err != nil {
		t.Fatalf("Exec: %v", err)
	}

	if got := st.output("stdout"); got != "hello\n" {
		t.Errorf("stdout = %q, want %q", got, "hello\n")
	}
	if got := st.output("stderr"); got != "warn\n" {
		t.Errorf("stderr = %q, want %q", got, "warn\n")
	}
	exits := st.exits()
	if len(exits) != 1 {
		t.Fatalf("got %d exit messages, want exactly 1", len(exits))
	}
	if exits[0].GetStatus() != setecv1grpc.SessionExecExit_STATUS_EXITED {
		t.Errorf("status = %v, want STATUS_EXITED", exits[0].GetStatus())
	}
	if exits[0].GetExitCode() != 0 {
		t.Errorf("exit_code = %d, want 0", exits[0].GetExitCode())
	}
	if ex.gotNamespace != "tenant-a" {
		t.Errorf("namespace = %q, want tenant-a", ex.gotNamespace)
	}
	if ex.gotPod != "sess-vm" {
		t.Errorf("pod = %q, want sess-vm", ex.gotPod)
	}
	if ex.gotContainer != workloadContainerName {
		t.Errorf("container = %q, want %q", ex.gotContainer, workloadContainerName)
	}
	if strings.Join(ex.gotCommand, " ") != "make build" {
		t.Errorf("command = %v, want [make build]", ex.gotCommand)
	}
}

// TestExec_NonZeroExitReportsCode asserts a failing command yields
// STATUS_EXITED with the command's real code — the case that must stay
// distinguishable from every "no code was reported" outcome.
func TestExec_NonZeroExitReportsCode(t *testing.T) {
	sb := runningSession("tenant-a", "sess", "uid-1")
	ex := &stubExecutor{err: utilexec.CodeExitError{Err: errors.New("exit 17"), Code: 17}}
	svc := execService(t, ex, sb)

	st := &fakeExecStream{in: []*setecv1grpc.SessionExecRequest{
		startMsg("tenant-a/sess/uid-1", "false"),
	}}
	if err := svc.Exec(st); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	exits := st.exits()
	if len(exits) != 1 {
		t.Fatalf("got %d exit messages, want exactly 1", len(exits))
	}
	if exits[0].GetStatus() != setecv1grpc.SessionExecExit_STATUS_EXITED {
		t.Fatalf("status = %v, want STATUS_EXITED", exits[0].GetStatus())
	}
	if exits[0].GetExitCode() != 17 {
		t.Errorf("exit_code = %d, want 17", exits[0].GetExitCode())
	}
}

// TestExec_TransportFailureIsNotExitZero is the invariant gibson#1183
// depends on: a broken exec channel must never look like a clean exit.
func TestExec_TransportFailureIsNotExitZero(t *testing.T) {
	sb := runningSession("tenant-a", "sess", "uid-1")
	ex := &stubExecutor{err: errors.New("stream reset by peer")}
	svc := execService(t, ex, sb)

	st := &fakeExecStream{in: []*setecv1grpc.SessionExecRequest{
		startMsg("tenant-a/sess/uid-1", "make"),
	}}
	if err := svc.Exec(st); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	exits := st.exits()
	if len(exits) != 1 {
		t.Fatalf("got %d exit messages, want exactly 1", len(exits))
	}
	if exits[0].GetStatus() != setecv1grpc.SessionExecExit_STATUS_TRANSPORT_FAILED {
		t.Fatalf("status = %v, want STATUS_TRANSPORT_FAILED", exits[0].GetStatus())
	}
	if exits[0].GetExitCode() != 0 {
		t.Errorf("exit_code = %d, want 0 (must be ignored for non-EXITED)", exits[0].GetExitCode())
	}
	if exits[0].GetMessage() == "" {
		t.Error("message empty; a non-EXITED status must explain itself")
	}
}

// TestExec_SandboxGoneMidExec asserts that a session that disappears
// while the command runs is reported as SANDBOX_GONE, not as a
// transport blip and certainly not as an exit code. This is the
// eviction-mid-build case: a plugin must be able to tell it from a
// build that genuinely failed.
func TestExec_SandboxGoneMidExec(t *testing.T) {
	sb := runningSession("tenant-a", "sess", "uid-1")
	ex := &stubExecutor{err: errors.New("connection closed")}
	svc := execService(t, ex, sb)
	ex.onExec = func() {
		_ = svc.Client.Delete(context.Background(), sb)
	}

	st := &fakeExecStream{in: []*setecv1grpc.SessionExecRequest{
		startMsg("tenant-a/sess/uid-1", "make"),
	}}
	if err := svc.Exec(st); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	exits := st.exits()
	if len(exits) != 1 {
		t.Fatalf("got %d exit messages, want exactly 1", len(exits))
	}
	if exits[0].GetStatus() != setecv1grpc.SessionExecExit_STATUS_SANDBOX_GONE {
		t.Fatalf("status = %v, want STATUS_SANDBOX_GONE", exits[0].GetStatus())
	}
	if exits[0].GetExitCode() != 0 {
		t.Errorf("exit_code = %d, want 0", exits[0].GetExitCode())
	}
}

// TestExec_CanceledReportsCanceled asserts a caller-canceled exec is
// reported as CANCELED rather than as a transport failure.
func TestExec_CanceledReportsCanceled(t *testing.T) {
	sb := runningSession("tenant-a", "sess", "uid-1")
	ex := &stubExecutor{block: make(chan struct{})}
	svc := execService(t, ex, sb)

	ctx, cancel := context.WithCancel(context.Background())
	st := &fakeExecStream{
		ctx: ctx,
		in:  []*setecv1grpc.SessionExecRequest{startMsg("tenant-a/sess/uid-1", "sleep", "60")},
	}
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	if err := svc.Exec(st); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	exits := st.exits()
	if len(exits) != 1 {
		t.Fatalf("got %d exit messages, want exactly 1", len(exits))
	}
	if exits[0].GetStatus() != setecv1grpc.SessionExecExit_STATUS_CANCELED {
		t.Errorf("status = %v, want STATUS_CANCELED", exits[0].GetStatus())
	}
}

// TestExec_StdinIsForwarded asserts stdin chunks reach the command and
// that stdin_eof closes the pipe so a reader-to-EOF command finishes.
func TestExec_StdinIsForwarded(t *testing.T) {
	sb := runningSession("tenant-a", "sess", "uid-1")
	ex := &stubExecutor{}
	svc := execService(t, ex, sb)

	st := &fakeExecStream{in: []*setecv1grpc.SessionExecRequest{
		startMsg("tenant-a/sess/uid-1", "cat"),
		stdinMsg("one "),
		stdinMsg("two"),
		stdinEOFMsg(),
	}}
	if err := svc.Exec(st); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if got := ex.stdinSeen(); got != "one two" {
		t.Errorf("stdin = %q, want %q", got, "one two")
	}
}

// TestExec_FirstMessageMustBeStart rejects a client that streams stdin
// before naming a session.
func TestExec_FirstMessageMustBeStart(t *testing.T) {
	sb := runningSession("tenant-a", "sess", "uid-1")
	svc := execService(t, &stubExecutor{}, sb)

	st := &fakeExecStream{in: []*setecv1grpc.SessionExecRequest{stdinMsg("data")}}
	err := svc.Exec(st)
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument (err=%v)", status.Code(err), err)
	}
	if len(st.exits()) != 0 {
		t.Error("an RPC-level rejection must not send an exit message")
	}
}

// TestExec_SecondStartRejected keeps the protocol single-shot. A
// client that breaks the wire contract gets an RPC error, NOT an exit
// message: a SessionExecExit is Setec's verdict on a command, and
// Setec has no verdict to give here.
func TestExec_SecondStartRejected(t *testing.T) {
	sb := runningSession("tenant-a", "sess", "uid-1")
	blocked := make(chan struct{})
	svc := execService(t, &stubExecutor{block: blocked}, sb)

	st := &fakeExecStream{in: []*setecv1grpc.SessionExecRequest{
		startMsg("tenant-a/sess/uid-1", "sh"),
		startMsg("tenant-a/sess/uid-1", "sh"),
	}}
	err := svc.Exec(st)
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument (err=%v)", status.Code(err), err)
	}
	if n := len(st.exits()); n != 0 {
		t.Errorf("got %d exit messages, want 0: a protocol violation is not a verdict", n)
	}
}

// TestExec_EmptyCommandRejected — argv is required.
func TestExec_EmptyCommandRejected(t *testing.T) {
	sb := runningSession("tenant-a", "sess", "uid-1")
	svc := execService(t, &stubExecutor{}, sb)

	st := &fakeExecStream{in: []*setecv1grpc.SessionExecRequest{
		startMsg("tenant-a/sess/uid-1"),
	}}
	if err := svc.Exec(st); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument (err=%v)", status.Code(err), err)
	}
}

// TestExec_RejectsEphemeralSandbox — the ephemeral lifecycle has one
// command and no second turn (ADR-0006).
func TestExec_RejectsEphemeralSandbox(t *testing.T) {
	sb := runningSession("tenant-a", "sess", "uid-1")
	sb.Spec.Lifecycle = nil
	svc := execService(t, &stubExecutor{}, sb)

	st := &fakeExecStream{in: []*setecv1grpc.SessionExecRequest{
		startMsg("tenant-a/sess/uid-1", "ls"),
	}}
	err := svc.Exec(st)
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("code = %v, want FailedPrecondition (err=%v)", status.Code(err), err)
	}
	if r := attachFailureDetail(t, err).GetReason(); r != setecv1grpc.AttachFailure_REASON_NOT_A_SESSION {
		t.Errorf("reason = %v, want REASON_NOT_A_SESSION", r)
	}
}

// TestExec_UnknownHandle — a handle whose UID belongs to an earlier
// same-name Sandbox must not reach the live one.
func TestExec_UnknownHandle(t *testing.T) {
	sb := runningSession("tenant-a", "sess", "uid-1")
	svc := execService(t, &stubExecutor{}, sb)

	st := &fakeExecStream{in: []*setecv1grpc.SessionExecRequest{
		startMsg("tenant-a/sess/uid-OTHER", "ls"),
	}}
	err := svc.Exec(st)
	if status.Code(err) != codes.NotFound {
		t.Fatalf("code = %v, want NotFound (err=%v)", status.Code(err), err)
	}
	if r := attachFailureDetail(t, err).GetReason(); r != setecv1grpc.AttachFailure_REASON_SESSION_NOT_FOUND {
		t.Errorf("reason = %v, want REASON_SESSION_NOT_FOUND", r)
	}
}

// TestExec_TerminalSessionRejected — a finished session has nothing to
// exec into.
func TestExec_TerminalSessionRejected(t *testing.T) {
	sb := runningSession("tenant-a", "sess", "uid-1")
	sb.Status.Phase = setecv1alpha1.SandboxPhaseCompleted
	svc := execService(t, &stubExecutor{}, sb)

	st := &fakeExecStream{in: []*setecv1grpc.SessionExecRequest{
		startMsg("tenant-a/sess/uid-1", "ls"),
	}}
	err := svc.Exec(st)
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("code = %v, want FailedPrecondition (err=%v)", status.Code(err), err)
	}
	if r := attachFailureDetail(t, err).GetReason(); r != setecv1grpc.AttachFailure_REASON_SESSION_ENDED {
		t.Errorf("reason = %v, want REASON_SESSION_ENDED", r)
	}
}

// TestExec_CrossTenantRefused — the handle names another tenant's
// namespace.
func TestExec_CrossTenantRefused(t *testing.T) {
	sb := runningSession("tenant-b", "sess", "uid-1")
	svc := execService(t, &stubExecutor{}, sb)
	svc.DefaultNamespace = "tenant-a"

	st := &fakeExecStream{in: []*setecv1grpc.SessionExecRequest{
		startMsg("tenant-b/sess/uid-1", "ls"),
	}}
	if err := svc.Exec(st); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("code = %v, want PermissionDenied (err=%v)", status.Code(err), err)
	}
}

// TestExec_StampsSessionActivity asserts an exec counts as session
// activity so the idle reaper cannot evict a session mid-build.
func TestExec_StampsSessionActivity(t *testing.T) {
	sb := runningSession("tenant-a", "sess", "uid-1")
	svc := execService(t, &stubExecutor{}, sb)

	st := &fakeExecStream{in: []*setecv1grpc.SessionExecRequest{
		startMsg("tenant-a/sess/uid-1", "ls"),
	}}
	if err := svc.Exec(st); err != nil {
		t.Fatalf("Exec: %v", err)
	}

	got := &setecv1alpha1.Sandbox{}
	if err := svc.Client.Get(context.Background(), types.NamespacedName{Namespace: "tenant-a", Name: "sess"}, got); err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	if got.Annotations[setecv1alpha1.AnnotationLastActivity] == "" {
		t.Fatalf("last-activity annotation not stamped; an exec must register as session activity")
	}
}

// TestExec_ResumesPausedSession asserts an exec against a paused
// session flips desiredState back to Running rather than failing.
func TestExec_ResumesPausedSession(t *testing.T) {
	sb := runningSession("tenant-a", "sess", "uid-1")
	sb.Status.Phase = setecv1alpha1.SandboxPhasePaused
	sb.Spec.DesiredState = setecv1alpha1.SandboxDesiredStatePaused
	svc := execService(t, &stubExecutor{}, sb)

	// Simulate the controller honouring the resume request.
	go func() {
		for range 200 {
			time.Sleep(5 * time.Millisecond)
			cur := &setecv1alpha1.Sandbox{}
			if err := svc.Client.Get(context.Background(), types.NamespacedName{Namespace: "tenant-a", Name: "sess"}, cur); err != nil {
				continue
			}
			if cur.Spec.DesiredState != setecv1alpha1.SandboxDesiredStateRunning {
				continue
			}
			cur.Status.Phase = setecv1alpha1.SandboxPhaseRunning
			_ = svc.Client.Status().Update(context.Background(), cur)
			return
		}
	}()

	st := &fakeExecStream{in: []*setecv1grpc.SessionExecRequest{
		startMsg("tenant-a/sess/uid-1", "ls"),
	}}
	if err := svc.Exec(st); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	exits := st.exits()
	if len(exits) != 1 || exits[0].GetStatus() != setecv1grpc.SessionExecExit_STATUS_EXITED {
		t.Fatalf("exits = %v, want one STATUS_EXITED", exits)
	}

	got := &setecv1alpha1.Sandbox{}
	if err := svc.Client.Get(context.Background(), types.NamespacedName{Namespace: "tenant-a", Name: "sess"}, got); err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	if got.Spec.DesiredState != setecv1alpha1.SandboxDesiredStateRunning {
		t.Errorf("desiredState = %q, want Running", got.Spec.DesiredState)
	}
}

// TestExec_NotRunningInTime refuses cleanly (no command ran, so no
// exit message) when the VM never reaches Running.
func TestExec_NotRunningInTime(t *testing.T) {
	sb := runningSession("tenant-a", "sess", "uid-1")
	sb.Status.Phase = setecv1alpha1.SandboxPhasePending
	svc := execService(t, &stubExecutor{}, sb)
	svc.execReadyTimeout = 100 * time.Millisecond

	st := &fakeExecStream{in: []*setecv1grpc.SessionExecRequest{
		startMsg("tenant-a/sess/uid-1", "ls"),
	}}
	err := svc.Exec(st)
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("code = %v, want FailedPrecondition (err=%v)", status.Code(err), err)
	}
	if r := attachFailureDetail(t, err).GetReason(); r != setecv1grpc.AttachFailure_REASON_SESSION_NOT_RUNNING {
		t.Errorf("reason = %v, want REASON_SESSION_NOT_RUNNING", r)
	}
	if len(st.exits()) != 0 {
		t.Error("no command ran, so no exit message may be sent")
	}
}

// TestExec_NoExecutorConfigured fails loudly rather than pretending the
// command succeeded.
func TestExec_NoExecutorConfigured(t *testing.T) {
	sb := runningSession("tenant-a", "sess", "uid-1")
	svc := execService(t, nil, sb)

	st := &fakeExecStream{in: []*setecv1grpc.SessionExecRequest{
		startMsg("tenant-a/sess/uid-1", "ls"),
	}}
	if err := svc.Exec(st); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("code = %v, want FailedPrecondition (err=%v)", status.Code(err), err)
	}
}
