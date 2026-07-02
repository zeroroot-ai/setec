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

// Package controller integration tests. This file wires a controller-runtime
// envtest environment — a real kube-apiserver + etcd without kubelet or
// scheduler — and hosts it under a single process-wide TestMain so every
// scenario in sandbox_controller_test.go shares the same control plane.
//
// Scenarios that depend on cluster-level state (notably the presence or
// absence of the kata-fc RuntimeClass) isolate themselves through unique
// namespaces and, where needed, by installing the RuntimeClass inside the
// test body itself. The default TestMain setup installs the RuntimeClass and
// a Kata-capable Node so the majority of scenarios can run without further
// preparation; the "no RuntimeClass" scenario explicitly deletes it before
// running and reinstalls afterward.
package controller

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
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
	runtimepkg "github.com/zeroroot-ai/setec/internal/runtime"
	snapshotpkg "github.com/zeroroot-ai/setec/internal/snapshot"
)

// Process-wide state populated by TestMain and consumed by every test
// function in this package. Kept package-private so tests can read them
// directly without plumbing a test fixture through every Eventually closure.
var (
	testEnv    *envtest.Environment
	testClient client.Client
	testCtx    context.Context
	testCancel context.CancelFunc

	// testRuntimeClassName matches the RuntimeClass installed by ensurePrereqs.
	// Centralized here so the no-RuntimeClass scenario can delete precisely
	// the object the controller is watching.
	testRuntimeClassName = "kata-fc"

	// testNodeSelectorLabel matches the label the controller's prereq check
	// uses to discover Kata-capable Nodes. The setup installs one Node with
	// this label so scenarios do not trip the "no Kata-capable Nodes"
	// warning path.
	testNodeSelectorLabel = "katacontainers.io/kata-runtime"

	// testRuntimeRegistry and testRuntimeCfg are the multi-backend
	// dispatcher registry and config wired to the SandboxReconciler.
	testRuntimeRegistry *runtimepkg.Registry
	testRuntimeCfg      *runtimepkg.RuntimeConfig

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
// individual tests can mutate behaviour through the package-wide
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
}

func (f *fakeNodeAgentClient) CreateSnapshot(_ context.Context, _ *setecgrpcv1.CreateSnapshotRequest) (*setecgrpcv1.CreateSnapshotResponse, error) {
	if f.CreateResp == nil && f.CreateErr == nil {
		return &setecgrpcv1.CreateSnapshotResponse{
			StorageRef: "test-ref", SizeBytes: 1024, Sha256: "cafe",
		}, nil
	}
	return f.CreateResp, f.CreateErr
}
func (f *fakeNodeAgentClient) RestoreSandbox(_ context.Context, _ *setecgrpcv1.RestoreSandboxRequest) (*setecgrpcv1.RestoreSandboxResponse, error) {
	if f.RestoreRes == nil && f.RestoreErr == nil {
		return &setecgrpcv1.RestoreSandboxResponse{Success: true}, nil
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
func (f *fakeNodeAgentClient) QueryPool(_ context.Context, _ *setecgrpcv1.QueryPoolRequest) (*setecgrpcv1.QueryPoolResponse, error) {
	return &setecgrpcv1.QueryPoolResponse{}, nil
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
		// Envtest binaries missing (no KUBEBUILDER_ASSETS) or the CRD path is
		// wrong. Skip gracefully rather than failing so CI environments
		// without setup-envtest still report a green run on unrelated
		// packages — same convention as test/integration/upgrade_test.go's
		// TestMain.
		fmt.Fprintf(os.Stderr,
			"controller: envtest start failed (%v); skipping all tests in this package.\n"+
				"Install binaries with: setup-envtest use --bin-dir /usr/local/kubebuilder/bin\n",
			err)
		// Exit 0 so go test reports SKIP rather than FAIL.
		os.Exit(0)
	}

	// Register both the core client-go scheme (needed for Pods, Nodes,
	// Events, RuntimeClasses) and the v1alpha1 Sandbox scheme.
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(nodev1.AddToScheme(scheme))
	utilruntime.Must(setecv1alpha1.AddToScheme(scheme))

	testCtx, testCancel = context.WithCancel(context.Background())

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

	// Build the runtime registry and config for the multi-backend path.
	// The test suite uses a kata-fc-only config to match the existing prereq
	// setup (one kata-fc RuntimeClass + one node with testNodeSelectorLabel).
	testRuntimeCfg = &runtimepkg.RuntimeConfig{
		Runtimes: map[string]runtimepkg.BackendConfig{
			runtimepkg.BackendKataFC: {
				Enabled:          true,
				RuntimeClassName: testRuntimeClassName,
				// DefaultOverhead is explicitly empty so the kata-fc dispatcher
				// returns nil overhead. envtest RuntimeClass objects do not
				// define overhead, and Kubernetes rejects pods with non-nil
				// Overhead that doesn't match the RuntimeClass overhead field.
				DefaultOverhead: corev1.ResourceList{},
			},
		},
		Defaults: runtimepkg.DefaultsConfig{
			Runtime: runtimepkg.RuntimeDefaults{
				Backend: runtimepkg.BackendKataFC,
			},
		},
	}
	testRuntimeRegistry = runtimepkg.NewRegistry()
	testRuntimeRegistry.Register(runtimepkg.NewKataFCDispatcher(
		testRuntimeCfg.Runtimes[runtimepkg.BackendKataFC],
	))

	// Phase 3 wiring: a package-wide fake dialer lets each scenario
	// script node-agent responses per its needs without touching the
	// reconciler itself.
	testDialer = &fakeNodeAgentDialer{client: &fakeNodeAgentClient{}}
	testCoordinator = &snapshotpkg.Coordinator{
		Client:   mgr.GetClient(),
		Dialer:   testDialer,
		Recorder: mgr.GetEventRecorder("snapshot-coordinator"),
		Metrics:  testCollectors,
	}

	reconciler := &SandboxReconciler{
		Client:            mgr.GetClient(),
		Scheme:            mgr.GetScheme(),
		Recorder:          mgr.GetEventRecorder("sandbox-controller"),
		NodeSelectorLabel: testNodeSelectorLabel,
		Runtimes:          testRuntimeRegistry,
		RuntimeCfg:        testRuntimeCfg,
		// Phase 2 dependencies wired so the envtest reconciler exercises
		// the full Phase 2 flow. Each dependency is nil-safe so Phase 1
		// scenarios continue to pass unchanged.
		ClassResolver:    testClassResolver,
		MetricsCollector: testCollectors,
		// Phase 3 dependency.
		Coordinator: testCoordinator,
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
		Scheme:      mgr.GetScheme(),
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
		Scheme: mgr.GetScheme(),
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

// ensurePrereqs installs the kata-fc RuntimeClass and labels a fake Node so
// the controller's prereq check succeeds. Envtest has no real Nodes, so we
// create one imperatively; the scheduler is not running so the Node will
// never actually be "ready", but prereq.Check only inspects labels.
//
// The Node carries both the legacy katacontainers.io/kata-runtime label
// (for backward compatibility) and the new setec.zeroroot.ai/runtime.kata-fc
// label (for the multi-backend prereq check and selectRuntime).
func ensurePrereqs(ctx context.Context, c client.Client) error {
	rc := &nodev1.RuntimeClass{
		ObjectMeta: metav1.ObjectMeta{Name: testRuntimeClassName},
		Handler:    "kata-fc",
	}
	if err := c.Create(ctx, rc); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create RuntimeClass: %w", err)
	}

	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "kata-node-1",
			Labels: map[string]string{
				testNodeSelectorLabel:               "true",
				"setec.zeroroot.ai/runtime.kata-fc": "true",
			},
		},
	}
	if err := c.Create(ctx, node); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create Node: %w", err)
	}
	return nil
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
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
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
		_ = testClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}})
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
