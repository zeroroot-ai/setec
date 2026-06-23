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
	"fmt"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	setecv1grpc "github.com/zeroroot-ai/setec/api/grpc/v1"
	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
	"github.com/zeroroot-ai/setec/internal/leasepool"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// defaultPoolReplenishInterval is how often each tenant's pool Manager
// background-replenishes registered classes.
const defaultPoolReplenishInterval = 10 * time.Second

// LeaseService implements setec.v1.LeaseService — the warm-pool lease
// layer over SandboxService. It maintains one leasepool.Manager per
// resolved tenant namespace so leases and pools never cross tenant
// boundaries, mirroring SandboxService's per-RPC tenant scoping.
//
// Lease claims a pre-warmed Sandbox; Exec runs the caller's command (a
// fresh Sandbox launched in the leased entry's class, snapshot-restored
// when the class declares a snapshot, so it inherits the warm base);
// Release destroys the leased warm Sandbox (destroy-on-release) and
// replenishes the pool.
type LeaseService struct {
	setecv1grpc.UnimplementedLeaseServiceServer

	// Client is the controller-runtime client for Sandbox/SandboxClass
	// operations. Required.
	Client client.Client

	// Clientset streams Pod logs for Exec. Required for Exec output.
	Clientset kubernetes.Interface

	// TenantResolver maps a tenant identity to its namespace. Required
	// unless AuthDisabled.
	TenantResolver TenantResolver

	// AuthDisabled / DefaultNamespace are unit-test-only, identical in
	// meaning to the SandboxService Service fields.
	AuthDisabled     bool
	DefaultNamespace string

	// ReplenishInterval overrides the background replenish cadence. Zero
	// uses defaultPoolReplenishInterval.
	ReplenishInterval time.Duration

	// runCtx bounds the lifetime of every Manager's background Run loop.
	// Set by Start; nil managers do not run a loop (tests drive Replenish
	// directly).
	runCtx context.Context

	mu       sync.Mutex
	managers map[string]*leasepool.Manager // namespace -> manager
}

// Start binds the background replenish loops to ctx. Call once before
// serving; the loops stop when ctx is cancelled.
func (s *LeaseService) Start(ctx context.Context) {
	s.mu.Lock()
	s.runCtx = ctx
	s.mu.Unlock()
}

// resolveNamespace mirrors Service.resolveNamespace.
func (s *LeaseService) resolveNamespace(ctx context.Context) (string, error) {
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

// managerFor returns the pool Manager for a namespace, lazily creating it
// (and starting its background loop, if Start was called).
func (s *LeaseService) managerFor(ns string) *leasepool.Manager {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.managers == nil {
		s.managers = map[string]*leasepool.Manager{}
	}
	if m, ok := s.managers[ns]; ok {
		return m
	}
	m := leasepool.NewManager(newCRBackend(s.Client), leasepool.Hooks{
		OnFill: func(class string, ready, leased int) {
			poolFillGauge(ns, class, ready, leased)
		},
	})
	s.managers[ns] = m
	if s.runCtx != nil {
		interval := s.ReplenishInterval
		if interval <= 0 {
			interval = defaultPoolReplenishInterval
		}
		go m.Run(s.runCtx, interval)
	}
	return m
}

// templateForClass builds a PoolTemplate from a SandboxClass, grounding
// the warm pool in the existing class policy (PreWarmImage, PreWarmPoolSize,
// DefaultResources). A class without a PreWarmImage has no warm template
// and is rejected at Lease so the caller gets a clear remediation.
func (s *LeaseService) templateForClass(ctx context.Context, ns, className string) (leasepool.PoolTemplate, error) {
	if className == "" {
		return leasepool.PoolTemplate{}, status.Error(codes.InvalidArgument,
			"sandbox_class is required to lease from a pool")
	}
	sc := &setecv1alpha1.SandboxClass{}
	if err := s.Client.Get(ctx, types.NamespacedName{Name: className}, sc); err != nil {
		if apierrors.IsNotFound(err) {
			return leasepool.PoolTemplate{}, status.Errorf(codes.NotFound,
				"SandboxClass %q not found", className)
		}
		return leasepool.PoolTemplate{}, status.Errorf(grpcCodeFor(err),
			"get SandboxClass: %v", err)
	}
	if sc.Spec.PreWarmImage == "" {
		return leasepool.PoolTemplate{}, status.Errorf(codes.FailedPrecondition,
			"SandboxClass %q declares no preWarmImage; the lease pool needs a warm image", className)
	}
	tmpl := leasepool.PoolTemplate{
		Namespace:    ns,
		SandboxClass: className,
		Image:        sc.Spec.PreWarmImage,
		Target:       int(sc.Spec.PreWarmPoolSize),
	}
	return tmpl, nil
}

// Lease claims a pre-warmed Sandbox for the requested class.
func (s *LeaseService) Lease(ctx context.Context, req *setecv1grpc.LeaseRequest) (*setecv1grpc.LeaseResponse, error) {
	ns, err := s.resolveNamespace(ctx)
	if err != nil {
		return nil, err
	}
	tmpl, err := s.templateForClass(ctx, ns, req.GetSandboxClass())
	if err != nil {
		return nil, err
	}

	m := s.managerFor(ns)
	m.Register(tmpl)

	res, err := m.Lease(ctx, req.GetSandboxClass(), req.GetFailIfEmpty())
	if err != nil {
		if errors.Is(err, leasepool.ErrPoolEmpty) {
			return nil, status.Error(codes.ResourceExhausted,
				"warm pool empty and fail_if_empty set")
		}
		return nil, status.Errorf(codes.Internal, "lease: %v", err)
	}

	return &setecv1grpc.LeaseResponse{
		LeaseId:   leaseTokenFor(ns, res.LeaseID),
		SandboxId: res.Ref.ID,
		Warm:      res.Warm,
	}, nil
}

// Release destroys the leased Sandbox and replenishes the pool.
func (s *LeaseService) Release(ctx context.Context, req *setecv1grpc.ReleaseRequest) (*setecv1grpc.ReleaseResponse, error) {
	ns, err := s.resolveNamespace(ctx)
	if err != nil {
		return nil, err
	}
	tokNS, leaseID, err := parseLeaseToken(req.GetLeaseId())
	if err != nil {
		return nil, err
	}
	if tokNS != ns {
		return nil, status.Error(codes.PermissionDenied, "lease does not belong to caller's tenant")
	}
	m := s.managerFor(ns)
	if err := m.Release(ctx, leaseID); err != nil {
		return nil, status.Errorf(codes.Internal, "release: %v", err)
	}
	return &setecv1grpc.ReleaseResponse{}, nil
}

// PoolStatus reports the warm fill level for a class.
func (s *LeaseService) PoolStatus(ctx context.Context, req *setecv1grpc.PoolStatusRequest) (*setecv1grpc.PoolStatusResponse, error) {
	ns, err := s.resolveNamespace(ctx)
	if err != nil {
		return nil, err
	}
	if req.GetSandboxClass() == "" {
		return nil, status.Error(codes.InvalidArgument, "sandbox_class is required")
	}
	m := s.managerFor(ns)
	ready, target, leased := m.Status(req.GetSandboxClass())
	return &setecv1grpc.PoolStatusResponse{
		Ready:  uint32(ready),
		Target: uint32(target),
		Leased: uint32(leased),
	}, nil
}

// Exec runs the caller's command in the leased Sandbox and streams its
// output. Because Setec Sandboxes are one-shot — the microVM runs its
// immutable spec.command then terminates and there is no in-VM exec
// channel in the v1 surface — Exec launches a fresh workload Sandbox in
// the leased entry's class, snapshot-restored from the class snapshot
// when one is configured so it inherits the warm base, then streams its
// logs to terminal. Exactly one Exec is permitted per lease.
func (s *LeaseService) Exec(req *setecv1grpc.ExecRequest, stream setecv1grpc.LeaseService_ExecServer) error {
	ctx := stream.Context()
	ns, err := s.resolveNamespaceStream(ctx)
	if err != nil {
		return err
	}
	tokNS, leaseID, err := parseLeaseToken(req.GetLeaseId())
	if err != nil {
		return err
	}
	if tokNS != ns {
		return status.Error(codes.PermissionDenied, "lease does not belong to caller's tenant")
	}
	if len(req.GetCommand()) == 0 {
		return status.Error(codes.InvalidArgument, "command must have at least one entry")
	}

	m := s.managerFor(ns)
	_, className, alreadyRun, err := m.LeaseInfo(leaseID)
	if err != nil {
		if errors.Is(err, leasepool.ErrLeaseNotFound) {
			return status.Error(codes.NotFound, "lease not found")
		}
		return status.Errorf(codes.Internal, "lookup lease: %v", err)
	}
	if alreadyRun {
		return status.Error(codes.FailedPrecondition, "lease already has a running Exec")
	}

	// Launch the workload Sandbox carrying the caller's command.
	workload := &setecv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "exec-",
			Namespace:    ns,
			Labels:       map[string]string{leaseClassLabel: className},
		},
		Spec: setecv1alpha1.SandboxSpec{
			SandboxClassName: className,
			Command:          append([]string(nil), req.GetCommand()...),
		},
	}
	sc := &setecv1alpha1.SandboxClass{}
	if err := s.Client.Get(ctx, types.NamespacedName{Name: className}, sc); err != nil {
		return status.Errorf(grpcCodeFor(err), "get SandboxClass: %v", err)
	}
	workload.Spec.Image = sc.Spec.PreWarmImage
	for k, v := range req.GetEnv() {
		workload.Spec.Env = append(workload.Spec.Env, corev1.EnvVar{Name: k, Value: v})
	}
	if err := s.Client.Create(ctx, workload); err != nil {
		return status.Errorf(grpcCodeFor(err), "launch exec Sandbox: %v", err)
	}
	// The exec Sandbox is owned by the lease lifecycle; clean it up when
	// Exec returns so a leaked workload never outlives its stream.
	defer func() {
		_ = s.Client.Delete(context.WithoutCancel(ctx), workload)
	}()

	return s.streamExec(ctx, ns, workload.Name, stream)
}

// streamExec waits for the exec Sandbox to reach terminal and streams its
// logs (when a clientset is available), then sends a final done message.
func (s *LeaseService) streamExec(ctx context.Context, ns, name string, stream setecv1grpc.LeaseService_ExecServer) error {
	// Stream logs best-effort when a clientset is wired. Log streaming is
	// optional so the lease/exec contract still works in environments
	// (and tests) without a Pod log backend.
	if s.Clientset != nil {
		podName := name + "-vm"
		if waitErr := s.waitLoggable(ctx, ns, podName); waitErr == nil {
			logStream, err := s.Clientset.CoreV1().
				Pods(ns).
				GetLogs(podName, &corev1.PodLogOptions{Follow: true, Container: workloadContainerName}).
				Stream(ctx)
			if err == nil {
				_ = relayExecLogs(ctx, logStream, stream)
				_ = logStream.Close()
			}
		}
	}

	// Wait for terminal phase to report the exit outcome.
	for {
		sb := &setecv1alpha1.Sandbox{}
		if err := s.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, sb); err != nil {
			return status.Errorf(grpcCodeFor(err), "get exec Sandbox: %v", err)
		}
		if isTerminal(sb.Status.Phase) {
			done := &setecv1grpc.ExecResponse{
				Done:   true,
				Reason: sb.Status.Reason,
			}
			if sb.Status.ExitCode != nil {
				done.ExitCode = *sb.Status.ExitCode
			}
			return stream.Send(done)
		}
		select {
		case <-ctx.Done():
			return status.FromContextError(ctx.Err()).Err()
		case <-time.After(waitPollInterval):
		}
	}
}

// waitLoggable reuses the loggable-pod wait shape from StreamLogs.
func (s *LeaseService) waitLoggable(ctx context.Context, ns, podName string) error {
	deadline := time.Now().Add(streamLogsPodPollTimeout)
	ticker := time.NewTicker(streamLogsPodPollInterval)
	defer ticker.Stop()
	for {
		pod := &corev1.Pod{}
		err := s.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: podName}, pod)
		switch {
		case apierrors.IsNotFound(err):
			// keep waiting
		case err != nil:
			return err
		default:
			if podLogsAvailable(pod) {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for exec Pod %q", podName)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// resolveNamespaceStream is resolveNamespace for streaming RPCs.
func (s *LeaseService) resolveNamespaceStream(ctx context.Context) (string, error) {
	return s.resolveNamespace(ctx)
}
