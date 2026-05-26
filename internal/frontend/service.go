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
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	setecv1alpha1grpc "github.com/zeroroot-ai/setec/api/grpc/v1alpha1"
	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
	"github.com/zeroroot-ai/setec/internal/tenancy"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// waitPollInterval is how often Wait polls the Sandbox's status. Keeping
// it short (500ms) means callers see terminal phases quickly; it is
// bounded by context so a client that Cancels gets the Cancel promptly.
const waitPollInterval = 500 * time.Millisecond

// streamLogsPodPollInterval is how often StreamLogs polls for the Pod
// to reach a phase where log bytes are available. Kept aligned with
// waitPollInterval so the operator event cadence is consistent.
const streamLogsPodPollInterval = 1 * time.Second

// streamLogsPodPollTimeout bounds the Follow=true wait for the Pod to
// become loggable. 30s matches the Requirement 2.5 budget.
const streamLogsPodPollTimeout = 30 * time.Second

// workloadContainerName is the name the podspec builder assigns to the
// workload container. Duplicated here (rather than imported) to keep
// the frontend free of a dependency on the podspec package.
const workloadContainerName = "workload"

// TenantResolver maps a TenantID to the Kubernetes namespace the
// frontend should operate against. Implementations typically list
// namespaces with the tenant label and return the first match.
type TenantResolver interface {
	NamespaceFor(ctx context.Context, t tenancy.TenantID) (string, error)
}

// Service is the SandboxService implementation backed by a
// controller-runtime client. It enforces tenant scoping on every RPC:
// no matter what sandbox_id a caller passes, the service confirms the
// CR is in the caller's tenant namespace before acting.
type Service struct {
	setecv1alpha1grpc.UnimplementedSandboxServiceServer

	// Client is the controller-runtime client used for CR operations.
	// Required.
	Client client.Client

	// Clientset is the client-go clientset used for operations that
	// controller-runtime's cached client does not surface well
	// (notably Pods/log streaming). Required for StreamLogs; other
	// RPCs degrade gracefully when nil.
	Clientset kubernetes.Interface

	// TenantResolver maps a tenant identity to its namespace. Required
	// unless AuthDisabled is true.
	TenantResolver TenantResolver

	// AuthDisabled, when true, skips TLS peer cert extraction and uses
	// DefaultNamespace for every call. Exists SOLELY for unit-test
	// convenience — the production frontend binary never sets this
	// field and no command-line flag exposes it. Setting it in
	// production would have no effect because the gRPC server refuses
	// to start without TLS certs.
	AuthDisabled bool

	// DefaultNamespace is the namespace used when AuthDisabled is true.
	// Unit-test-only, same as AuthDisabled above.
	DefaultNamespace string
}

// resolveNamespace returns the namespace for the caller. mTLS-authenticated
// path extracts tenant from peer cert and delegates to TenantResolver;
// insecure path returns DefaultNamespace.
func (s *Service) resolveNamespace(ctx context.Context) (string, error) {
	if s.AuthDisabled {
		if s.DefaultNamespace == "" {
			return "", status.Error(codes.FailedPrecondition,
				"AuthDisabled set but DefaultNamespace empty")
		}
		return s.DefaultNamespace, nil
	}
	tid, err := TenantFromContext(ctx)
	if err != nil {
		return "", err
	}
	if s.TenantResolver == nil {
		return "", status.Error(codes.FailedPrecondition, "TenantResolver not configured")
	}
	ns, err := s.TenantResolver.NamespaceFor(ctx, tid)
	if err != nil {
		return "", status.Errorf(codes.PermissionDenied,
			"tenant %q has no accessible namespace: %v", tid, err)
	}
	return ns, nil
}

// parseSandboxID splits a sandbox_id of the form <namespace>/<name>/<uid>
// into its components. Returns InvalidArgument if the id shape is wrong.
func parseSandboxID(id string) (ns, name string, err error) {
	parts := strings.Split(id, "/")
	if len(parts) != 3 {
		return "", "", status.Errorf(codes.InvalidArgument,
			"sandbox_id %q must be <namespace>/<name>/<uid>", id)
	}
	return parts[0], parts[1], nil
}

// Launch translates LaunchRequest into a Sandbox CR create.
func (s *Service) Launch(ctx context.Context, req *setecv1alpha1grpc.LaunchRequest) (*setecv1alpha1grpc.LaunchResponse, error) {
	ns, err := s.resolveNamespace(ctx)
	if err != nil {
		return nil, err
	}

	if req.GetImage() == "" {
		return nil, status.Error(codes.InvalidArgument, "image is required")
	}
	if len(req.GetCommand()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "command must have at least one entry")
	}

	sb := &setecv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "sbx-",
			Namespace:    ns,
		},
		Spec: setecv1alpha1.SandboxSpec{
			SandboxClassName: req.GetSandboxClass(),
			Image:            req.GetImage(),
			Command:          append([]string(nil), req.GetCommand()...),
		},
	}

	if r := req.GetResources(); r != nil {
		sb.Spec.Resources = setecv1alpha1.Resources{
			VCPU: int32(r.GetVcpu()),
		}
		if mem := r.GetMemory(); mem != "" {
			q, err := resource.ParseQuantity(mem)
			if err != nil {
				return nil, status.Errorf(codes.InvalidArgument,
					"resources.memory %q: %v", mem, err)
			}
			sb.Spec.Resources.Memory = q
		}
	}
	if n := req.GetNetwork(); n != nil {
		sb.Spec.Network = &setecv1alpha1.Network{
			Mode: setecv1alpha1.NetworkMode(n.GetMode()),
		}
		for _, a := range n.GetAllow() {
			sb.Spec.Network.Allow = append(sb.Spec.Network.Allow, setecv1alpha1.NetworkAllow{
				Host: a.GetHost(),
				Port: int32(a.GetPort()),
			})
		}
	}
	if lc := req.GetLifecycle(); lc != nil && lc.GetTimeout() != "" {
		d, err := time.ParseDuration(lc.GetTimeout())
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument,
				"lifecycle.timeout %q: %v", lc.GetTimeout(), err)
		}
		sb.Spec.Lifecycle = &setecv1alpha1.Lifecycle{
			Timeout: &metav1.Duration{Duration: d},
		}
	}
	for k, v := range req.GetEnv() {
		sb.Spec.Env = append(sb.Spec.Env, corev1.EnvVar{Name: k, Value: v})
	}

	if err := s.Client.Create(ctx, sb); err != nil {
		return nil, status.Errorf(grpcCodeFor(err),
			"create Sandbox: %v", err)
	}

	return &setecv1alpha1grpc.LaunchResponse{
		SandboxId: fmt.Sprintf("%s/%s/%s", sb.Namespace, sb.Name, string(sb.UID)),
		Name:      sb.Name,
		Namespace: sb.Namespace,
	}, nil
}

// Wait polls the Sandbox until it reaches a terminal phase and returns.
// The caller's context controls the timeout; no server-side deadline.
func (s *Service) Wait(ctx context.Context, req *setecv1alpha1grpc.WaitRequest) (*setecv1alpha1grpc.WaitResponse, error) {
	ns, name, err := parseSandboxID(req.GetSandboxId())
	if err != nil {
		return nil, err
	}
	if err := s.checkTenantNamespace(ctx, ns); err != nil {
		return nil, err
	}

	for {
		sb := &setecv1alpha1.Sandbox{}
		if err := s.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, sb); err != nil {
			return nil, status.Errorf(grpcCodeFor(err), "get Sandbox: %v", err)
		}
		if isTerminal(sb.Status.Phase) {
			resp := &setecv1alpha1grpc.WaitResponse{
				Phase:  string(sb.Status.Phase),
				Reason: sb.Status.Reason,
			}
			if sb.Status.ExitCode != nil {
				resp.ExitCode = *sb.Status.ExitCode
			}
			return resp, nil
		}
		select {
		case <-ctx.Done():
			return nil, status.FromContextError(ctx.Err()).Err()
		case <-time.After(waitPollInterval):
		}
	}
}

// Kill deletes the Sandbox CR. Owner-reference GC collects the Pod and
// any NetworkPolicy.
func (s *Service) Kill(ctx context.Context, req *setecv1alpha1grpc.KillRequest) (*setecv1alpha1grpc.KillResponse, error) {
	ns, name, err := parseSandboxID(req.GetSandboxId())
	if err != nil {
		return nil, err
	}
	if err := s.checkTenantNamespace(ctx, ns); err != nil {
		return nil, err
	}

	sb := &setecv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
	}
	if err := s.Client.Delete(ctx, sb); err != nil {
		if apierrors.IsNotFound(err) {
			return &setecv1alpha1grpc.KillResponse{}, nil
		}
		return nil, status.Errorf(grpcCodeFor(err), "delete Sandbox: %v", err)
	}
	return &setecv1alpha1grpc.KillResponse{}, nil
}

// StreamLogs streams the Pod's workload-container log bytes to the
// gRPC client. Tenant scope is enforced up-front; the Sandbox and its
// owned Pod must both live in the caller's resolved namespace.
//
// Follow semantics match the underlying kubectl-style GetLogs call:
// when Follow is false the server sends every available log byte and
// closes cleanly on EOF; when Follow is true the server keeps the
// stream open until either the underlying container exits, the client
// cancels, or streamLogsPodPollTimeout elapses waiting for the Pod to
// reach a loggable phase.
func (s *Service) StreamLogs(req *setecv1alpha1grpc.StreamLogsRequest, stream setecv1alpha1grpc.SandboxService_StreamLogsServer) error {
	ctx := stream.Context()

	ns, name, err := parseSandboxID(req.GetSandboxId())
	if err != nil {
		return err
	}
	if err := s.checkTenantNamespace(ctx, ns); err != nil {
		return err
	}

	// Resolve the Sandbox so we can surface NotFound distinctly from
	// Pod-not-yet-created (which is FailedPrecondition).
	sb := &setecv1alpha1.Sandbox{}
	if err := s.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, sb); err != nil {
		return status.Errorf(grpcCodeFor(err), "get Sandbox: %v", err)
	}

	if s.Clientset == nil {
		return status.Error(codes.FailedPrecondition,
			"StreamLogs: kubernetes clientset is not configured on the frontend")
	}

	podName := name + "-vm"
	if err := s.waitForLoggablePod(ctx, ns, podName, req.GetFollow()); err != nil {
		return err
	}

	opts := &corev1.PodLogOptions{
		Follow:    req.GetFollow(),
		Container: workloadContainerName,
	}
	logStream, err := s.Clientset.CoreV1().Pods(ns).GetLogs(podName, opts).Stream(ctx)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
			// Client hung up before the stream opened; no error
			// surfaced because this is a normal shutdown shape.
			return nil
		}
		if apierrors.IsNotFound(err) {
			return status.Errorf(codes.FailedPrecondition,
				"Pod %q not found; wait for the Sandbox to reach Running before streaming logs",
				podName)
		}
		if apierrors.IsForbidden(err) {
			return status.Errorf(codes.PermissionDenied, "get logs: %v", err)
		}
		return status.Errorf(codes.Internal, "open log stream: %v", err)
	}
	defer func() {
		_ = logStream.Close()
	}()

	return relayLogStream(ctx, logStream, stream)
}

// waitForLoggablePod polls the Pod until it reaches a phase where the
// kubelet exposes container log bytes (Running, Succeeded, or Failed).
// When follow is false and the Pod already exists in any phase the
// call returns immediately so callers can fetch terminal logs of an
// already-exited Sandbox. When follow is true and the Pod has yet to
// progress past Pending the call waits up to streamLogsPodPollTimeout
// and returns FailedPrecondition on timeout so the client sees a
// clean remediation path.
func (s *Service) waitForLoggablePod(ctx context.Context, ns, podName string, follow bool) error {
	deadline := time.Now().Add(streamLogsPodPollTimeout)
	ticker := time.NewTicker(streamLogsPodPollInterval)
	defer ticker.Stop()

	for {
		pod := &corev1.Pod{}
		err := s.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: podName}, pod)
		switch {
		case apierrors.IsNotFound(err):
			if !follow {
				return status.Errorf(codes.FailedPrecondition,
					"Pod %q not yet created; wait for the Sandbox to reach Running", podName)
			}
		case err != nil:
			return status.Errorf(grpcCodeFor(err), "get Pod: %v", err)
		default:
			if podLogsAvailable(pod) {
				return nil
			}
			if !follow {
				return status.Errorf(codes.FailedPrecondition,
					"Pod %q is in phase %q; no logs available", podName, pod.Status.Phase)
			}
		}

		if time.Now().After(deadline) {
			return status.Errorf(codes.FailedPrecondition,
				"timed out waiting for Pod %q to reach a loggable phase", podName)
		}
		select {
		case <-ctx.Done():
			return status.FromContextError(ctx.Err()).Err()
		case <-ticker.C:
		}
	}
}

// podLogsAvailable reports whether the kubelet is willing to serve log
// bytes for the Pod. Running, Succeeded, and Failed all satisfy this;
// Pending and Unknown do not.
func podLogsAvailable(pod *corev1.Pod) bool {
	switch pod.Status.Phase {
	case corev1.PodRunning, corev1.PodSucceeded, corev1.PodFailed:
		return true
	default:
		return false
	}
}

// relayLogStream reads the Pod log stream line-by-line and forwards
// each line as a StreamLogsResponse over the gRPC server-streaming channel. A
// client cancel becomes a clean return (no error); a Scanner error is
// surfaced as Internal unless it was driven by context cancellation.
func relayLogStream(ctx context.Context, r io.Reader, stream setecv1alpha1grpc.SandboxService_StreamLogsServer) error {
	scanner := bufio.NewScanner(r)
	// 1 MiB per line upper bound — matches kubelet's log line limit
	// and avoids an OOM if a workload ever emits something huge on
	// a single line.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := append(scanner.Bytes(), '\n')
		chunk := &setecv1alpha1grpc.StreamLogsResponse{
			Data:   append([]byte(nil), line...),
			Stream: "stdout",
		}
		if err := stream.Send(chunk); err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
				return nil
			}
			return status.Errorf(codes.Internal, "send log chunk: %v", err)
		}
	}
	if err := scanner.Err(); err != nil {
		if errors.Is(ctx.Err(), context.Canceled) {
			return nil
		}
		return status.Errorf(codes.Internal, "read log stream: %v", err)
	}
	return nil
}

// checkTenantNamespace is the tenant-scope guard. It verifies the
// requested namespace is the caller's resolved namespace.
func (s *Service) checkTenantNamespace(ctx context.Context, ns string) error {
	mine, err := s.resolveNamespace(ctx)
	if err != nil {
		return err
	}
	if mine != ns {
		return status.Errorf(codes.PermissionDenied,
			"tenant does not own namespace %q", ns)
	}
	return nil
}

// isTerminal mirrors the controller's isTerminalPhase; duplicated here
// to avoid importing the controller package from the frontend (keeps
// dependencies acyclic).
func isTerminal(p setecv1alpha1.SandboxPhase) bool {
	return p == setecv1alpha1.SandboxPhaseCompleted || p == setecv1alpha1.SandboxPhaseFailed
}

// grpcCodeFor maps K8s API errors to sensible gRPC status codes.
func grpcCodeFor(err error) codes.Code {
	switch {
	case apierrors.IsNotFound(err):
		return codes.NotFound
	case apierrors.IsAlreadyExists(err):
		return codes.AlreadyExists
	case apierrors.IsForbidden(err):
		return codes.PermissionDenied
	case apierrors.IsConflict(err):
		return codes.Aborted
	case apierrors.IsInvalid(err):
		return codes.InvalidArgument
	default:
		return codes.Internal
	}
}
