// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package frontend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/httpstream"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	utilexec "k8s.io/utils/exec"
	"sigs.k8s.io/controller-runtime/pkg/client"

	setecv1grpc "github.com/zeroroot-ai/setec/api/grpc/v1"
	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	// defaultExecReadyTimeout bounds how long Exec waits for a
	// session's microVM to reach Running — including a resume from
	// paused or suspended, which has to restore a memory checkpoint.
	// A caller that gives up sooner cancels the RPC and gets a
	// STATUS_CANCELED exit.
	defaultExecReadyTimeout = 60 * time.Second

	// execReadyPollInterval is how often Exec re-reads the Sandbox
	// while waiting for it to become Running. Matches the cadence
	// StreamLogs polls the Pod with.
	execReadyPollInterval = time.Second

	// stdoutStreamName / stderrStreamName label SessionExecOutput
	// chunks. Same vocabulary as StreamLogsResponse.stream.
	stdoutStreamName = "stdout"
	stderrStreamName = "stderr"
)

// errDuplicateStart is raised when a client sends a second
// SessionExecStart on an established exec stream.
var errDuplicateStart = errors.New("exec stream carried a second start message")

// containerExecutor opens an exec session against one container of a
// running Pod and pumps stdio through it. It exists so the frontend's
// exec semantics — handle resolution, resume, activity, and above all
// the exit classification — are unit-testable without a live kubelet.
//
// The production implementation is the Kubernetes pods/exec
// subresource: kubelet hands the request to the CRI runtime, which for
// a kata-fc Sandbox is the Kata shim, which asks the in-guest
// kata-agent to spawn the process inside the workload container's
// namespaces. That IS the in-VM exec channel, and it lands in the same
// mount namespace as the booted workload — which is why the durable
// /workspace volume is visible to an exec'd command at all.
type containerExecutor interface {
	ExecInContainer(
		ctx context.Context,
		namespace, pod, container string,
		command []string,
		stdin io.Reader,
		stdout, stderr io.Writer,
	) error
}

// Exec runs a command inside a live session Sandbox and streams its
// stdio, terminating the stream with exactly one verdict.
//
// The contract this method exists to keep is the exit contract: a
// caller must be able to tell "the command finished and returned N"
// from "the command's outcome is unknown". Everything else here —
// resume, activity heartbeats, tenant scoping — is in service of
// running the command; the classification at the end is what makes the
// result usable.
func (s *Service) Exec(stream setecv1grpc.SandboxService_ExecServer) error {
	ctx := stream.Context()

	start, err := recvExecStart(stream)
	if err != nil {
		return err
	}

	ns, name, uid, err := parseSessionHandle(start.GetSandboxId())
	if err != nil {
		return err
	}
	if err := s.checkTenantNamespace(ctx, ns); err != nil {
		return err
	}
	if _, err := s.resolveLiveSession(ctx, ns, name, uid, start.GetSandboxId()); err != nil {
		return err
	}

	execer := s.containerExec()
	if execer == nil {
		return status.Error(codes.FailedPrecondition,
			"this frontend has no exec transport configured; Exec is unavailable")
	}

	// The whole exec counts as session activity, starting before the
	// resume wait: a session being resumed FOR an exec is by definition
	// in use and must not be idle-reaped out from under it.
	stopActivity := s.keepSessionActive(ctx, ns, name)
	defer stopActivity()

	if err := s.ensureSessionRunning(ctx, ns, name); err != nil {
		return err
	}

	return s.runExec(ctx, stream, execer, ns, name, start.GetCommand())
}

// recvExecStart reads the mandatory opening message and validates it.
// Failures here are RPC-level: no command has run, so no verdict is
// owed and none is sent.
func recvExecStart(stream setecv1grpc.SandboxService_ExecServer) (*setecv1grpc.SessionExecStart, error) {
	first, err := stream.Recv()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil, status.Error(codes.InvalidArgument,
				"exec stream closed before sending a start message")
		}
		return nil, err
	}
	start := first.GetStart()
	if start == nil {
		return nil, status.Error(codes.InvalidArgument,
			"the first Exec message must carry `start`; stdin before start has no session to go to")
	}
	if len(start.GetCommand()) == 0 {
		return nil, status.Error(codes.InvalidArgument,
			"start.command must name at least one argv entry")
	}
	return start, nil
}

// runExec performs the exec itself: pump client stdin in, container
// output out, then classify the outcome into exactly one
// SessionExecExit.
func (s *Service) runExec(
	ctx context.Context,
	stream setecv1grpc.SandboxService_ExecServer,
	execer containerExecutor,
	ns, name string,
	command []string,
) error {
	// A private cancel lets a client protocol violation tear the
	// command down without waiting for it to finish on its own.
	execCtx, cancelExec := context.WithCancel(ctx)
	defer cancelExec()

	sender := &execSender{stream: stream}
	stdinR, stdinW := io.Pipe()
	defer func() { _ = stdinR.Close() }()

	pump := &stdinPump{stream: stream, w: stdinW, cancel: cancelExec}
	pumpDone := make(chan struct{})
	go func() {
		defer close(pumpDone)
		pump.run()
	}()

	execErr := execer.ExecInContainer(execCtx, ns, podNameFor(name), workloadContainerName,
		command, stdinR, sender.writer(stdoutStreamName), sender.writer(stderrStreamName))

	// Stop the pump and let it settle before deciding anything: a
	// protocol violation it noticed is the reason the exec ended.
	cancelExec()
	_ = stdinW.Close()
	<-pumpDone

	if pErr := pump.err(); pErr != nil {
		// The client broke the wire protocol. It gets an RPC error, not
		// a verdict — a SessionExecExit means "Setec established what
		// happened to your command", and here Setec did not.
		return status.Errorf(codes.InvalidArgument, "invalid Exec stream: %v", pErr)
	}
	if err := sender.err(); err != nil {
		// The response stream is already broken; there is nowhere to
		// deliver a verdict to.
		return err
	}

	return sender.send(&setecv1grpc.SandboxServiceExecResponse{
		Response: &setecv1grpc.SandboxServiceExecResponse_Exit{
			Exit: s.classifyExecOutcome(ctx, ns, name, execErr),
		},
	})
}

// classifyExecOutcome turns the executor's error into the one verdict
// the caller is owed. This is the function gibson#1183's contract rests
// on, so it errs toward "unknown" and never toward a fabricated code:
// exit_code is populated ONLY on the branch where a wait status was
// genuinely reported.
func (s *Service) classifyExecOutcome(
	ctx context.Context,
	ns, name string,
	execErr error,
) *setecv1grpc.SessionExecExit {
	if execErr == nil {
		return &setecv1grpc.SessionExecExit{
			Status:   setecv1grpc.SessionExecExit_STATUS_EXITED,
			ExitCode: 0,
		}
	}

	// A reported wait status is the only source of an exit code.
	var coded utilexec.CodeExitError
	if errors.As(execErr, &coded) {
		return &setecv1grpc.SessionExecExit{
			Status:   setecv1grpc.SessionExecExit_STATUS_EXITED,
			ExitCode: int32(coded.ExitStatus()), //nolint:gosec // a wait status is always in int32 range
		}
	}

	// The caller walked away (or its deadline elapsed). The command was
	// torn down with the RPC; nothing waited on it.
	if ctx.Err() != nil {
		return &setecv1grpc.SessionExecExit{
			Status:  setecv1grpc.SessionExecExit_STATUS_CANCELED,
			Message: fmt.Sprintf("exec canceled by the caller: %v", ctx.Err()),
		}
	}

	// Distinguish "the sandbox stopped existing" from "the channel to a
	// healthy sandbox broke". The caller acts differently on each: the
	// first means the session is gone and the work must be restarted
	// elsewhere; the second is worth retrying against the same session.
	if gone, why := s.sessionGone(ctx, ns, name); gone {
		return &setecv1grpc.SessionExecExit{
			Status: setecv1grpc.SessionExecExit_STATUS_SANDBOX_GONE,
			Message: fmt.Sprintf(
				"the session's microVM stopped existing while the command was running (%s); "+
					"the command's outcome is unknown: %v", why, execErr),
		}
	}

	return &setecv1grpc.SessionExecExit{
		Status: setecv1grpc.SessionExecExit_STATUS_TRANSPORT_FAILED,
		Message: fmt.Sprintf(
			"the exec channel failed before a wait status was read, "+
				"so the command's outcome is unknown: %v", execErr),
	}
}

// sessionGone reports whether the session Sandbox has ceased to be a
// place a command could still be running, and why. A read failure is
// deliberately NOT treated as gone: the frontend must not upgrade its
// own API hiccup into "your sandbox died".
func (s *Service) sessionGone(ctx context.Context, ns, name string) (bool, string) {
	sb := &setecv1alpha1.Sandbox{}
	err := s.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, sb)
	switch {
	case apierrors.IsNotFound(err):
		return true, "the Sandbox no longer exists"
	case err != nil:
		return false, ""
	case !sb.DeletionTimestamp.IsZero():
		return true, "the Sandbox is being torn down"
	case isTerminal(sb.Status.Phase):
		return true, fmt.Sprintf("the Sandbox reached phase %s", sb.Status.Phase)
	default:
		return false, ""
	}
}

// resolveLiveSession validates a session handle against cluster state
// and returns the Sandbox it names. Shared by Attach and Exec so both
// verbs accept and refuse exactly the same handles, with the same typed
// AttachFailure detail.
func (s *Service) resolveLiveSession(
	ctx context.Context,
	ns, name, uid, handle string,
) (*setecv1alpha1.Sandbox, error) {
	sb := &setecv1alpha1.Sandbox{}
	if err := s.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, sb); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, attachFailure(codes.NotFound,
				setecv1grpc.AttachFailure_REASON_SESSION_NOT_FOUND, "",
				"no session for handle %q: Sandbox not found", handle)
		}
		return nil, status.Errorf(grpcCodeFor(err), "get Sandbox: %v", err)
	}

	// The UID pins the handle to one session. A same-name Sandbox
	// created after the original ended is a different session and must
	// not be reachable through the old handle (ADR-0005 invariant 3:
	// never cross-session reuse).
	if string(sb.UID) != uid {
		return nil, attachFailure(codes.NotFound,
			setecv1grpc.AttachFailure_REASON_SESSION_NOT_FOUND, "",
			"no session for handle %q: the Sandbox it referenced no longer exists", handle)
	}

	if !sb.Spec.IsSession() {
		return nil, attachFailure(codes.FailedPrecondition,
			setecv1grpc.AttachFailure_REASON_NOT_A_SESSION, "",
			"Sandbox %q is ephemeral; only lifecycle.mode=session supports this (ADR-0006)", name)
	}

	if !sb.DeletionTimestamp.IsZero() {
		return nil, attachFailure(codes.FailedPrecondition,
			setecv1grpc.AttachFailure_REASON_SESSION_ENDED, string(sb.Status.Phase),
			"session %q has ended: teardown is in progress", name)
	}
	if isTerminal(sb.Status.Phase) {
		return nil, attachFailure(codes.FailedPrecondition,
			setecv1grpc.AttachFailure_REASON_SESSION_ENDED, string(sb.Status.Phase),
			"session %q has ended (phase %s, reason %s)", name, sb.Status.Phase, sb.Status.Reason)
	}
	return sb, nil
}

// ensureSessionRunning brings the session's microVM to Running and
// waits for it, so an exec against an idle-suspended session resumes it
// rather than failing (ADR-0006). Flipping spec.desiredState is the
// same lever a client has; the controllers own the actual resume.
func (s *Service) ensureSessionRunning(ctx context.Context, ns, name string) error {
	deadline := time.Now().Add(s.execReadyBudget())
	requested := false

	for {
		sb := &setecv1alpha1.Sandbox{}
		if err := s.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, sb); err != nil {
			if apierrors.IsNotFound(err) {
				return attachFailure(codes.NotFound,
					setecv1grpc.AttachFailure_REASON_SESSION_NOT_FOUND, "",
					"session %s/%s disappeared while preparing the exec", ns, name)
			}
			return status.Errorf(grpcCodeFor(err), "get Sandbox: %v", err)
		}
		if isTerminal(sb.Status.Phase) || !sb.DeletionTimestamp.IsZero() {
			return attachFailure(codes.FailedPrecondition,
				setecv1grpc.AttachFailure_REASON_SESSION_ENDED, string(sb.Status.Phase),
				"session %q ended while preparing the exec (phase %s)", name, sb.Status.Phase)
		}
		if sb.Status.Phase == setecv1alpha1.SandboxPhaseRunning {
			return nil
		}

		if !requested && sb.Spec.DesiredState != setecv1alpha1.SandboxDesiredStateRunning {
			if err := s.requestSessionRunning(ctx, ns, name); err != nil {
				return status.Errorf(grpcCodeFor(err), "resume session: %v", err)
			}
			requested = true
		}

		if time.Now().After(deadline) {
			return attachFailure(codes.FailedPrecondition,
				setecv1grpc.AttachFailure_REASON_SESSION_NOT_RUNNING, string(sb.Status.Phase),
				"session %q is in phase %s and did not reach Running within %s; no command was run",
				name, sb.Status.Phase, s.execReadyBudget())
		}
		select {
		case <-ctx.Done():
			return status.FromContextError(ctx.Err()).Err()
		case <-time.After(execReadyPollInterval):
		}
	}
}

// requestSessionRunning flips spec.desiredState to Running with a
// spec-only merge patch, so it can never collide with the operator's
// status writes.
func (s *Service) requestSessionRunning(ctx context.Context, ns, name string) error {
	body, err := json.Marshal(map[string]any{
		"spec": map[string]any{
			"desiredState": string(setecv1alpha1.SandboxDesiredStateRunning),
		},
	})
	if err != nil {
		return fmt.Errorf("marshal resume patch: %w", err)
	}
	sb := &setecv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
	}
	return s.Client.Patch(ctx, sb, client.RawPatch(types.MergePatchType, body))
}

// execReadyBudget is the configured readiness budget, or the default.
func (s *Service) execReadyBudget() time.Duration {
	if s.execReadyTimeout > 0 {
		return s.execReadyTimeout
	}
	return defaultExecReadyTimeout
}

// containerExec returns the exec transport: the test override when set,
// otherwise a pods/exec executor built from the REST config. Nil means
// the frontend cannot exec and must say so rather than pretend.
func (s *Service) containerExec() containerExecutor {
	if s.execer != nil {
		return s.execer
	}
	if s.RESTConfig == nil {
		return nil
	}
	return &podSubresourceExecutor{cfg: s.RESTConfig}
}

// podNameFor mirrors the operator's Pod naming for a Sandbox.
func podNameFor(sandboxName string) string { return sandboxName + "-vm" }

// stdinPump forwards the client half of an Exec stream into the
// command's standard input, and records any protocol violation so the
// caller can be told it broke the contract rather than being handed a
// verdict Setec cannot vouch for.
type stdinPump struct {
	stream setecv1grpc.SandboxService_ExecServer
	w      *io.PipeWriter
	cancel context.CancelFunc

	mu       sync.Mutex
	protoErr error
}

func (p *stdinPump) run() {
	defer func() { _ = p.w.Close() }()
	eofSeen := false
	for {
		msg, err := p.stream.Recv()
		if err != nil {
			// io.EOF is the client half-closing: no more stdin, and the
			// command may still be running. Any other error means the
			// request stream broke; the exec below will notice.
			return
		}
		switch r := msg.GetRequest().(type) {
		case *setecv1grpc.SandboxServiceExecRequest_Start:
			p.fail(errDuplicateStart)
			return
		case *setecv1grpc.SandboxServiceExecRequest_Stdin:
			if eofSeen {
				p.fail(errors.New("stdin sent after stdin_eof"))
				return
			}
			if _, werr := p.w.Write(r.Stdin); werr != nil {
				return
			}
		case *setecv1grpc.SandboxServiceExecRequest_StdinEof:
			if r.StdinEof {
				eofSeen = true
				_ = p.w.Close()
			}
		default:
			p.fail(fmt.Errorf("unrecognised Exec request message %T", r))
			return
		}
	}
}

func (p *stdinPump) fail(err error) {
	p.mu.Lock()
	if p.protoErr == nil {
		p.protoErr = err
	}
	p.mu.Unlock()
	_ = p.w.CloseWithError(err)
	p.cancel()
}

func (p *stdinPump) err() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.protoErr
}

// execSender serialises writes to the response stream. remotecommand
// writes stdout and stderr from separate goroutines, and a gRPC stream
// tolerates exactly one Send at a time.
type execSender struct {
	stream setecv1grpc.SandboxService_ExecServer

	mu      sync.Mutex
	sendErr error
}

func (s *execSender) send(msg *setecv1grpc.SandboxServiceExecResponse) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sendErr != nil {
		return s.sendErr
	}
	if err := s.stream.Send(msg); err != nil {
		s.sendErr = err
		return err
	}
	return nil
}

func (s *execSender) err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sendErr
}

func (s *execSender) writer(name string) io.Writer {
	return &execStreamWriter{sender: s, name: name}
}

type execStreamWriter struct {
	sender *execSender
	name   string
}

func (w *execStreamWriter) Write(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	// Copy: the caller owns b and reuses its buffer between writes,
	// while the gRPC codec may still be marshalling ours.
	chunk := make([]byte, len(b))
	copy(chunk, b)
	if err := w.sender.send(&setecv1grpc.SandboxServiceExecResponse{
		Response: &setecv1grpc.SandboxServiceExecResponse_Output{
			Output: &setecv1grpc.SessionExecOutput{Data: chunk, Stream: w.name},
		},
	}); err != nil {
		return 0, err
	}
	return len(b), nil
}

// podSubresourceExecutor is the production transport: the Kubernetes
// pods/exec subresource. It negotiates WebSocket first and falls back
// to SPDY for older API servers, matching kubectl exec.
type podSubresourceExecutor struct {
	cfg *rest.Config
}

func (e *podSubresourceExecutor) ExecInContainer(
	ctx context.Context,
	namespace, pod, container string,
	command []string,
	stdin io.Reader,
	stdout, stderr io.Writer,
) error {
	cs, err := kubernetes.NewForConfig(e.cfg)
	if err != nil {
		return fmt.Errorf("build clientset for exec: %w", err)
	}
	req := cs.CoreV1().RESTClient().Post().
		Resource("pods").
		Namespace(namespace).
		Name(pod).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: container,
			Command:   command,
			Stdin:     stdin != nil,
			Stdout:    true,
			Stderr:    true,
			TTY:       false,
		}, scheme.ParameterCodec)

	spdyExec, err := remotecommand.NewSPDYExecutor(e.cfg, "POST", req.URL())
	if err != nil {
		return fmt.Errorf("build SPDY exec: %w", err)
	}
	wsExec, err := remotecommand.NewWebSocketExecutor(e.cfg, "GET", req.URL().String())
	if err != nil {
		return fmt.Errorf("build WebSocket exec: %w", err)
	}
	exec, err := remotecommand.NewFallbackExecutor(wsExec, spdyExec, httpstream.IsUpgradeFailure)
	if err != nil {
		return fmt.Errorf("build exec: %w", err)
	}

	return exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdin:  stdin,
		Stdout: stdout,
		Stderr: stderr,
		Tty:    false,
	})
}
