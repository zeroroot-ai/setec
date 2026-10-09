// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

// Package controller integration tests. This file wires a controller-runtime
// envtest environment — a real kube-apiserver + etcd without kubelet or
// scheduler — and hosts it under a single process-wide TestMain so every
// scenario in sandbox_controller_test.go shares the same control plane.
//
// Scenarios isolate themselves through unique namespaces. Each Sandbox gets
// a launcher Pod; the suite plays the Job controller for the disk builder
// Jobs (completeDiskJobs) and each test plays the kubelet for its Pods.
package controller

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	setecgrpcv1 "github.com/zeroroot-ai/setec/api/grpc/v1"
	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
	classpkg "github.com/zeroroot-ai/setec/internal/class"
	metricspkg "github.com/zeroroot-ai/setec/internal/metrics"
	"github.com/zeroroot-ai/setec/internal/netpol"
	snapshotpkg "github.com/zeroroot-ai/setec/internal/snapshot"
	"github.com/zeroroot-ai/setec/internal/snapshot/gate"
)

// Process-wide state populated by TestMain and consumed by every test
// function in this package. Kept package-private so tests can read them
// directly without plumbing a test fixture through every Eventually closure.
var (
	testEnv    *envtest.Environment
	testClient client.Client
	testCtx    context.Context
	testCancel context.CancelFunc

	// testDiskNamespace is the operator namespace of the suite: the disk
	// builder Jobs run there.
	testDiskNamespace = "setec-system"

	// testNetPolConfig is the egress posture the envtest reconciler
	// generates policies from. Scenarios assert against these exact
	// values, so keep them stable.
	testNetPolConfig = netpol.Config{
		ReservedCIDRs: []string{"10.0.0.0/8", "169.254.0.0/16"},
		ResolverIPs:   []string{"1.1.1.1"},
		// An egress-allow-list entry now names the addresses its host
		// resolves to (setec#130). The suite supplies a fixed table
		// rather than the process resolver so scenarios assert on exact
		// peers and never depend on the network the test host is on.
		Resolver: testHostResolver{},
	}

	// testEgressHosts is the DNS the envtest suite runs against. Addresses are
	// TEST-NET fixtures outside every entry in testNetPolConfig.ReservedCIDRs,
	// so a resolved peer is never suppressed by the reserved list.
	testEgressHosts = map[string][]string{
		"api.example.com":     {"203.0.113.10/32"},
		"metrics.example.com": {"198.51.100.7/32"},
	}

	// Phase 2 dependencies that scenarios may inspect directly.
	testClassResolver   *classpkg.Resolver
	testCollectors      *metricspkg.Collectors
	testMetricsRegistry *prometheus.Registry

	// Phase 3 dependencies wired when the envtest suite needs to drive
	// snapshot scenarios. testDialer is a mutable fake the Coordinator
	// dials instead of a real node-agent; tests swap its embedded
	// client to script per-scenario responses.
	testDialer      *fakeNodeAgentDialer
	testCoordinator *snapshotpkg.Coordinator
)

// fakeNodeAgentClient is the package-wide test double satisfying
// snapshot.NodeAgentClient. Each RPC returns the configured response
// and, when set, the configured error. Fields are exported so
// individual tests can mutate behavior through the package-wide
// testDialer.
type fakeNodeAgentClient struct {
	CreateResp *setecgrpcv1.CreateSnapshotResponse
	CreateErr  error
	RestoreRes *setecgrpcv1.RestoreSandboxResponse
	RestoreErr error
	PauseRes   *setecgrpcv1.PauseSandboxResponse
	PauseErr   error
	ResumeRes  *setecgrpcv1.ResumeSandboxResponse
	ResumeErr  error
	DeleteRes  *setecgrpcv1.DeleteSnapshotResponse
	DeleteErr  error
	// LastCreate is the last CreateSnapshot request.
	LastCreate *setecgrpcv1.CreateSnapshotRequest
}

func (f *fakeNodeAgentClient) CreateSnapshot(_ context.Context, in *setecgrpcv1.CreateSnapshotRequest) (*setecgrpcv1.CreateSnapshotResponse, error) {
	f.LastCreate = in
	if f.CreateResp == nil && f.CreateErr == nil {
		// Mirror the real backends (LocalDisk/S3): the storage ref
		// echoes the snapshot id. Session-checkpoint refs therefore
		// stay inside the SessionCheckpointID namespace the invariant
		// gate's same-session binding checks.
		return &setecgrpcv1.CreateSnapshotResponse{
			StorageRef: in.GetSnapshotId(), SizeBytes: 1024,
		}, nil
	}
	return f.CreateResp, f.CreateErr
}
func (f *fakeNodeAgentClient) RestoreSandbox(_ context.Context, _ *setecgrpcv1.RestoreSandboxRequest) (*setecgrpcv1.RestoreSandboxResponse, error) {
	if f.RestoreRes == nil && f.RestoreErr == nil {
		// The default fake models a healthy production node: every
		// docs/design/isolation.md per-restore verification is confirmed, so the
		// invariant gate admits the restore. Scenarios that exercise
		// the gate override RestoreRes with a degraded response.
		return &setecgrpcv1.RestoreSandboxResponse{
			Success:         true,
			EntropyReseeded: true,
			Uniquified:      true,
			EncryptedAtRest: true,
		}, nil
	}
	return f.RestoreRes, f.RestoreErr
}
func (f *fakeNodeAgentClient) PauseSandbox(_ context.Context, _ *setecgrpcv1.PauseSandboxRequest) (*setecgrpcv1.PauseSandboxResponse, error) {
	if f.PauseRes == nil && f.PauseErr == nil {
		return &setecgrpcv1.PauseSandboxResponse{Success: true}, nil
	}
	return f.PauseRes, f.PauseErr
}
func (f *fakeNodeAgentClient) ResumeSandbox(_ context.Context, _ *setecgrpcv1.ResumeSandboxRequest) (*setecgrpcv1.ResumeSandboxResponse, error) {
	if f.ResumeRes == nil && f.ResumeErr == nil {
		return &setecgrpcv1.ResumeSandboxResponse{Success: true}, nil
	}
	return f.ResumeRes, f.ResumeErr
}
func (f *fakeNodeAgentClient) DeleteSnapshot(_ context.Context, _ *setecgrpcv1.DeleteSnapshotRequest) (*setecgrpcv1.DeleteSnapshotResponse, error) {
	if f.DeleteRes == nil && f.DeleteErr == nil {
		return &setecgrpcv1.DeleteSnapshotResponse{Success: true}, nil
	}
	return f.DeleteRes, f.DeleteErr
}

// fakeNodeAgentDialer is the package-wide test double satisfying
// snapshot.NodeAgentDialer. The single embedded client is returned
// for every node so tests can rewrite its fields between scenarios.
type fakeNodeAgentDialer struct {
	client *fakeNodeAgentClient
}

func (d *fakeNodeAgentDialer) Dial(_ context.Context, _ string) (snapshotpkg.NodeAgentClient, error) {
	return d.client, nil
}

// TestMain boots envtest once for the whole package. We use TestMain rather
// than Ginkgo's BeforeSuite so individual scenarios can be written with the
// standard testing.T API and gomega's NewWithT helper. Ginkgo is available in
// go.mod for other test files in the project but is not required here.
func TestMain(m *testing.M) {
	logf.SetLogger(zap.New(zap.WriteTo(os.Stderr), zap.UseDevMode(true)))

	// The kubebuilder-style CRD manifests live at config/crd/bases relative
	// to the repository root. This file is at internal/controller, so we
	// resolve the path with ../.. — mirroring the standard scaffold.
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		fmt.Fprintf(os.Stderr, "resolve repo root: %v\n", err)
		os.Exit(1)
	}

	testEnv = &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join(repoRoot, "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}

	cfg, err := testEnv.Start()
	if err != nil {
		// A failed envtest start FAILS the package (setec#302). This package
		// holds every reconciler behavioral test — Phase 2/3, session
		// lifecycle, pause timeouts, the invariant gate, runtime selection —
		// so exiting 0 here reported `ok` with zero tests run and made the
		// whole suite silently evaporate whenever KUBEBUILDER_ASSETS was
		// empty, relative, or pointed at a stale cache. `ok` with no `=== RUN`
		// is indistinguishable from a pass to a human scanning output and to
		// anything keying on the exit code, so the gate could not fail.
		//
		// Two ways to legitimately not run here, and neither can hide a
		// broken envtest lane:
		//
		//  1. SETEC_SKIP_ENVTEST=1 — explicit opt-out.
		//  2. KUBEBUILDER_ASSETS unset — this is not the envtest lane at all.
		//     The org-wide `fast` tier (reusable-go-ci.yml) runs a generic
		//     `go test ./...` and never installs envtest binaries; failing it
		//     would block every PR on a lane that was never meant to run
		//     these tests.
		//
		// The lane that IS meant to run them is `make test`, which resolves
		// KUBEBUILDER_ASSETS to an absolute path and asserts an executable
		// etcd is there BEFORE invoking go test. So "assets absent" cannot
		// occur in that lane, and "assets present but envtest won't start" —
		// the setec#302 case, a relative or stale path — is fatal below.
		if skip, why := envtestOptOut(); skip {
			fmt.Fprintf(os.Stderr,
				"controller: envtest start failed (%v); %s — skipping this package.\n", err, why)
			os.Exit(0)
		}
		fmt.Fprintf(os.Stderr,
			"controller: envtest start failed: %v\n\n"+
				"Every reconciler test lives in this package; refusing to report success with zero tests run.\n"+
				"Install the binaries and export an ABSOLUTE path:\n"+
				"    make setup-envtest\n"+
				"    export KUBEBUILDER_ASSETS=$(setup-envtest use --bin-dir \"$PWD/bin\" -p path)\n"+
				"A relative KUBEBUILDER_ASSETS makes envtest fail to exec etcd from a test's working directory.\n"+
				"To deliberately skip (unit-testing without envtest binaries): SETEC_SKIP_ENVTEST=1\n",
			err)
		os.Exit(1)
	}

	// Register both the core client-go scheme (needed for Pods, Nodes,
	// Events, RuntimeClasses) and the v1alpha1 Sandbox scheme.
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(setecv1alpha1.AddToScheme(scheme))

	testCtx, testCancel = context.WithCancel(context.Background()) //nolint:fatcontext // TestMain sets the suite context once

	// Build a manager backed by the envtest apiserver. Metrics and health
	// probes are disabled because this manager is not long-lived and we do
	// not want random listener addresses leaking across parallel test runs.
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		LeaderElection:         false,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "new manager: %v\n", err)
		_ = testEnv.Stop()
		os.Exit(1)
	}

	// Phase 2 dependencies are constructed with a fresh Prometheus
	// registry and no OTLP endpoint so tests stay isolated from the
	// global controller-runtime registry and the process's OTEL state.
	testMetricsRegistry = prometheus.NewRegistry()
	testCollectors = metricspkg.NewCollectorsWith(testMetricsRegistry)
	testClassResolver = classpkg.NewResolver(mgr.GetClient())

	testDialer = &fakeNodeAgentDialer{client: &fakeNodeAgentClient{}}
	testCoordinator = &snapshotpkg.Coordinator{
		Client:   mgr.GetClient(),
		Dialer:   testDialer,
		Recorder: mgr.GetEventRecorder("snapshot-coordinator"),
		Metrics:  testCollectors,
	}

	reconciler := &SandboxReconciler{
		Client:        mgr.GetClient(),
		Scheme:        mgr.GetScheme(),
		Recorder:      mgr.GetEventRecorder("sandbox-controller"),
		LauncherImage: "launcher:test",
		DiskRepo:      "registry.example/disks",
		DiskKeys:      []string{"key"},
		DiskBuilder: DiskBuilderConfig{
			Image: "disk-builder:test", Namespace: testDiskNamespace, SigningSecret: "disk-seed",
		},
		// Phase 2 dependencies wired so the envtest reconciler exercises
		// the full Phase 2 flow. Each dependency is nil-safe so Phase 1
		// scenarios continue to pass unchanged.
		ClassResolver:    testClassResolver,
		MetricsCollector: testCollectors,
		// Phase 3 dependency.
		Coordinator: testCoordinator,
		// The egress posture the envtest reconciler builds policies from.
		NetPol: testNetPolConfig,
		// On by default in the operator, so envtest exercises the same
		// path production takes.
		NamespaceBaselineDeny: true,
		// Tracer and MultiTenancyEnabled stay at zero values — individual
		// scenarios that need them can construct their own reconciler.
	}
	if err := reconciler.SetupWithManager(mgr); err != nil {
		fmt.Fprintf(os.Stderr, "setup reconciler: %v\n", err)
		_ = testEnv.Stop()
		os.Exit(1)
	}

	snapshotReconciler := &SnapshotReconciler{
		Client:      mgr.GetClient(),
		Recorder:    mgr.GetEventRecorder("snapshot-controller"),
		Coordinator: testCoordinator,
	}
	if err := snapshotReconciler.SetupWithManager(mgr); err != nil {
		fmt.Fprintf(os.Stderr, "setup Snapshot reconciler: %v\n", err)
		_ = testEnv.Stop()
		os.Exit(1)
	}

	classReconciler := &SandboxClassReconciler{
		Client: mgr.GetClient(),
		Gate:   &gate.Gate{Reader: mgr.GetClient()},
	}
	if err := classReconciler.SetupWithManager(mgr); err != nil {
		fmt.Fprintf(os.Stderr, "setup SandboxClass reconciler: %v\n", err)
		_ = testEnv.Stop()
		os.Exit(1)
	}

	// The manager's cached client is what the reconciler sees; tests use a
	// direct (non-cached) client built from the same REST config so writes
	// are visible immediately without waiting for cache sync.
	testClient, err = client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		fmt.Fprintf(os.Stderr, "build test client: %v\n", err)
		_ = testEnv.Stop()
		os.Exit(1)
	}

	// Seed the cluster with the prerequisites the controller expects. The
	// no-RuntimeClass scenario deletes and later restores the RuntimeClass
	// inside its own body, so other tests are unaffected.
	if err := ensurePrereqs(testCtx, testClient); err != nil {
		fmt.Fprintf(os.Stderr, "seed prereqs: %v\n", err)
		_ = testEnv.Stop()
		os.Exit(1)
	}

	// Run the manager in the background. Its lifetime is bounded by
	// testCtx which TestMain cancels on teardown.
	go completeDiskJobs(testCtx, testClient)
	go func() {
		if err := mgr.Start(testCtx); err != nil {
			fmt.Fprintf(os.Stderr, "manager exited: %v\n", err)
		}
	}()

	// Wait for the manager's cache to sync so the first test does not race
	// with the controller's startup. A short bounded wait is sufficient
	// because envtest is local and the cache only holds Sandboxes + Pods.
	if ok := mgr.GetCache().WaitForCacheSync(testCtx); !ok {
		fmt.Fprintf(os.Stderr, "cache sync timed out\n")
		_ = testEnv.Stop()
		os.Exit(1)
	}

	code := m.Run()

	testCancel()
	if err := testEnv.Stop(); err != nil {
		fmt.Fprintf(os.Stderr, "stop envtest: %v\n", err)
	}
	os.Exit(code)
}

// ensurePrereqs creates the operator namespace of the disk builder Jobs and
// one fleet Node. Envtest has no real Nodes and no scheduler, so tests bind
// their Pods to the Node by hand.
func ensurePrereqs(ctx context.Context, c client.Client) error {
	for _, obj := range []client.Object{
		&corev1.Namespace{Name: testDiskNamespace},
		&corev1.Node{Name: "fleet-node-1"},
	} {
		if err := c.Create(ctx, obj); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create %s: %w", obj.GetName(), err)
		}
	}
	return nil
}

// completeDiskJobs plays the Job controller, which envtest does not run:
// it marks each disk builder Job of the suite Complete, as a cluster does
// once the disk is in the registry. The kubelet is played the same way:
// each test sets the status of its Pods.
func completeDiskJobs(ctx context.Context, c client.Client) {
	t := time.NewTicker(50 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		jobs := &batchv1.JobList{}
		if err := c.List(ctx, jobs, client.InNamespace(testDiskNamespace)); err != nil {
			continue
		}
		for i := range jobs.Items {
			job := &jobs.Items[i]
			if len(job.Status.Conditions) > 0 {
				continue
			}
			now := metav1.Now()
			job.Status.StartTime = &now
			job.Status.CompletionTime = &now
			job.Status.Succeeded = 1
			job.Status.Conditions = []batchv1.JobCondition{
				{Type: batchv1.JobSuccessCriteriaMet, Status: corev1.ConditionTrue, LastTransitionTime: now},
				{Type: batchv1.JobComplete, Status: corev1.ConditionTrue, LastTransitionTime: now},
			}
			_ = c.Status().Update(ctx, job)
		}
	}
}

// newNamespace creates a uniquely-named namespace and registers a cleanup
// with the test so scenarios cannot see each other's Sandboxes. The returned
// name is suitable for metadata.namespace on every object the test creates.
func newNamespace(t *testing.T, prefix string) string {
	t.Helper()
	// Namespace names must be DNS-1123, so we use a timestamp + test name
	// hash; the t.Name() value is already lowercased and path-like so only
	// light sanitization is needed.
	name := fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
	ns := &corev1.Namespace{Name: name}
	if err := testClient.Create(testCtx, ns); err != nil {
		t.Fatalf("create namespace %q: %v", name, err)
	}
	t.Cleanup(func() {
		// Best-effort cleanup. Envtest does not run the namespace
		// controller so child objects are not GC'd by namespace deletion;
		// tests that care about leak-free shutdown should delete their
		// own Sandboxes/Pods explicitly.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = testClient.Delete(ctx, &corev1.Namespace{Name: name})
	})
	return name
}

// getSandbox is a thin wrapper around Get that returns the fetched Sandbox
// by value. Centralizing it lets Eventually closures stay readable.
func getSandbox(ctx context.Context, ns, name string) (*setecv1alpha1.Sandbox, error) {
	sb := &setecv1alpha1.Sandbox{}
	if err := testClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, sb); err != nil {
		return nil, err
	}
	return sb, nil
}

// getPod is the Pod analogue of getSandbox.
func getPod(ctx context.Context, ns, name string) (*corev1.Pod, error) {
	pod := &corev1.Pod{}
	if err := testClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, pod); err != nil {
		return nil, err
	}
	return pod, nil
}

// testHostResolver is the netpol.HostResolver the envtest suite wires in.
// It performs no I/O.
type testHostResolver struct{}

func (testHostResolver) Resolve(_ context.Context, host string) ([]string, error) {
	addrs, ok := testEgressHosts[host]
	if !ok {
		return nil, fmt.Errorf("%w: %q: suite has no record", netpol.ErrResolveFailed, host)
	}
	return append([]string(nil), addrs...), nil
}
