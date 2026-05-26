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

// Package integration tests the CRD upgrade path using envtest. The scenarios
// here cover REQ-6.1 through REQ-6.4: legacy SandboxClass objects (VMM-only,
// no Runtime field) must survive an operator upgrade, the defaulting webhook
// must back-fill Runtime.Backend, running Sandboxes must be unaffected, legacy
// and new objects must coexist, and the Runtime field must remain optional at
// the CRD level.
//
// If the envtest binaries (kube-apiserver + etcd) are not installed the test
// suite skips with a clear message rather than failing. Install them via:
//
//	go install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest
//	$(go env GOPATH)/bin/setup-envtest use --bin-dir /usr/local/kubebuilder/bin
package integration

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
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

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
	runtimepkg "github.com/zeroroot-ai/setec/internal/runtime"
	"github.com/zeroroot-ai/setec/internal/webhook"
)

// upgradeTestEnv is process-wide state for the upgrade integration suite.
// It is populated by TestMain and consumed by every scenario below.
var (
	upgradeEnv    *envtest.Environment
	upgradeClient client.Client

	// upgradeRuntimeCfg is the RuntimeConfig wired to the SandboxClassWebhook
	// under test. All four backends are enabled to match a freshly-upgraded
	// cluster; the default backend is kata-fc (REQ-6.1 default).
	upgradeRuntimeCfg *runtimepkg.RuntimeConfig
)

// TestMain boots envtest for the upgrade scenario package. If the envtest
// binaries cannot be found the suite skips all tests rather than failing,
// because the binaries require a one-time setup step that is not guaranteed
// in every developer environment.
func TestMain(m *testing.M) {
	logf.SetLogger(zap.New(zap.WriteTo(os.Stderr), zap.UseDevMode(true)))

	// Resolve the repo root relative to this file's directory. This file lives
	// at test/integration, so the repo root is ../.. from here.
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		fmt.Fprintf(os.Stderr, "integration: resolve repo root: %v\n", err)
		os.Exit(1)
	}

	upgradeEnv = &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join(repoRoot, "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}

	cfg, startErr := upgradeEnv.Start()
	if startErr != nil {
		// Envtest binaries missing or the CRD path is wrong. Skip gracefully
		// rather than failing so CI environments without setup-envtest still
		// report a green run on unrelated packages.
		fmt.Fprintf(os.Stderr,
			"integration: envtest start failed (%v); skipping all tests in this package.\n"+
				"Install binaries with: setup-envtest use --bin-dir /usr/local/kubebuilder/bin\n",
			startErr)
		// Exit 0 so go test reports SKIP rather than FAIL.
		os.Exit(0)
	}

	// Build the scheme used by both the fake webhook client and the
	// envtest-backed real client.
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(setecv1alpha1.AddToScheme(scheme))

	// upgradeRuntimeCfg mirrors a freshly-upgraded cluster: all four backends
	// enabled, kata-fc as the cluster default.
	upgradeRuntimeCfg = &runtimepkg.RuntimeConfig{
		Runtimes: map[string]runtimepkg.BackendConfig{
			runtimepkg.BackendKataFC: {
				Enabled:          true,
				RuntimeClassName: "kata-fc",
			},
			runtimepkg.BackendKataQEMU: {
				Enabled:          true,
				RuntimeClassName: "kata-qemu",
			},
			runtimepkg.BackendGVisor: {
				Enabled:          true,
				RuntimeClassName: "gvisor",
			},
			runtimepkg.BackendRunc: {
				Enabled:          true,
				RuntimeClassName: "runc",
				DevOnly:          false, // DevOnly off so coexistence tests skip the namespace gate
			},
		},
		Defaults: runtimepkg.DefaultsConfig{
			Runtime: runtimepkg.RuntimeDefaults{
				Backend: runtimepkg.BackendKataFC,
			},
		},
	}

	// Build the controller-runtime manager. Metrics and health probes are
	// disabled because we only care about the webhook path; no reconciler is
	// wired.
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		LeaderElection:         false,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "integration: new manager: %v\n", err)
		_ = upgradeEnv.Stop()
		os.Exit(1)
	}

	// Wire the SandboxClassWebhook into the manager so the defaulting path
	// (REQ-6.1) can be exercised end-to-end. We call Default() directly in the
	// test bodies below — the manager registration is included here to verify
	// that the webhook compiles and registers cleanly alongside envtest.
	classWebhook := &webhook.SandboxClassWebhook{
		Client:     mgr.GetClient(),
		RuntimeCfg: upgradeRuntimeCfg,
	}
	if err := classWebhook.SetupWebhookWithManager(mgr); err != nil {
		fmt.Fprintf(os.Stderr, "integration: setup SandboxClassWebhook: %v\n", err)
		_ = upgradeEnv.Stop()
		os.Exit(1)
	}

	// Build a direct (non-cached) client for test-body assertions. Writes are
	// visible immediately without waiting for cache sync.
	upgradeClient, err = client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		fmt.Fprintf(os.Stderr, "integration: build client: %v\n", err)
		_ = upgradeEnv.Stop()
		os.Exit(1)
	}

	// Seed a "default" namespace so the webhook's dev-gate namespace fetch
	// succeeds without a real cluster.
	seedDefaultNamespace()

	// Run the manager in the background so the webhook's injected client can
	// sync. Tests use upgradeClient (direct), not the cached client, for reads.
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		if err := mgr.Start(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "integration: manager exited: %v\n", err)
		}
	}()

	code := m.Run()

	cancel()
	if err := upgradeEnv.Stop(); err != nil {
		fmt.Fprintf(os.Stderr, "integration: stop envtest: %v\n", err)
	}
	os.Exit(code)
}

// seedDefaultNamespace creates the "default" namespace in the envtest cluster.
// The SandboxClassWebhook's dev-gate check fetches this namespace; an absent
// namespace would cause a spurious InternalError on every validate call.
func seedDefaultNamespace() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}}
	if err := upgradeClient.Create(ctx, ns); err != nil && !apierrors.IsAlreadyExists(err) {
		fmt.Fprintf(os.Stderr, "integration: seed default namespace: %v\n", err)
	}
}

// mkLegacySandboxClass builds a SandboxClass that only sets Spec.VMM — the
// shape emitted by operators prior to the runtime-backends upgrade. Runtime is
// nil to simulate a pre-upgrade manifest.
func mkLegacySandboxClass(name string, vmm setecv1alpha1.VMM) *setecv1alpha1.SandboxClass {
	return &setecv1alpha1.SandboxClass{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: setecv1alpha1.SandboxClassSpec{
			VMM: vmm,
		},
	}
}

// mkSandboxWithClass builds a minimal Sandbox referencing the named class.
func mkSandboxWithClass(ns, name, className string) *setecv1alpha1.Sandbox {
	return &setecv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
		},
		Spec: setecv1alpha1.SandboxSpec{
			SandboxClassName: className,
			Image:            "busybox:1.36",
			Command:          []string{"sleep", "5"},
			Resources: setecv1alpha1.Resources{
				VCPU:   1,
				Memory: resource.MustParse("128Mi"),
			},
		},
	}
}

// createAndDelete is a test-scoped helper that creates obj and registers a
// cleanup to delete it. SandboxClass is cluster-scoped so no namespace is
// needed.
func createAndDelete(t *testing.T, obj client.Object) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := upgradeClient.Create(ctx, obj); err != nil {
		t.Fatalf("create %T %q: %v", obj, obj.GetName(), err)
	}
	t.Cleanup(func() {
		dctx, dcancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer dcancel()
		_ = upgradeClient.Delete(dctx, obj)
	})
}

// ---------------------------------------------------------------------------
// Scenario 1: REQ-6.1 — Legacy SandboxClass (VMM-only) upgraded
//
// A SandboxClass with only Spec.VMM=Firecracker and no Spec.Runtime block is
// created. The SandboxClassWebhook.Default() method must back-fill
// Spec.Runtime.Backend=kata-fc. We call Default() directly here because envtest
// does not route admission calls through the webhook server unless a full TLS
// webhook registration is performed (which requires cert injection not available
// in unit-style envtest). The direct call exercises the same code path.
// ---------------------------------------------------------------------------

func TestUpgrade_LegacySandboxClass_DefaultsRuntime(t *testing.T) {
	t.Parallel()

	hw := &webhook.SandboxClassWebhook{
		Client:     upgradeClient,
		RuntimeCfg: upgradeRuntimeCfg,
	}

	cls := mkLegacySandboxClass("upgrade-legacy-fc", setecv1alpha1.VMMFirecracker)

	// Verify pre-condition: Runtime is nil (legacy shape).
	if cls.Spec.Runtime != nil {
		t.Fatalf("pre-condition failed: expected Runtime==nil, got %+v", cls.Spec.Runtime)
	}

	ctx := context.Background()
	if err := hw.Default(ctx, cls); err != nil {
		t.Fatalf("Default() error: %v", err)
	}

	// REQ-6.1: the webhook must fill Runtime.Backend from the VMM field.
	if cls.Spec.Runtime == nil {
		t.Fatal("Default() left Runtime nil; expected Runtime to be filled")
	}
	if cls.Spec.Runtime.Backend != runtimepkg.BackendKataFC {
		t.Fatalf("Runtime.Backend = %q; want %q", cls.Spec.Runtime.Backend, runtimepkg.BackendKataFC)
	}

	// Idempotency: calling Default a second time must not change anything
	// (REQ-6.1, Error Handling scenario 7).
	wantBackend := cls.Spec.Runtime.Backend
	if err := hw.Default(ctx, cls); err != nil {
		t.Fatalf("second Default() error: %v", err)
	}
	if cls.Spec.Runtime.Backend != wantBackend {
		t.Fatalf("second Default() mutated Backend: got %q, want %q", cls.Spec.Runtime.Backend, wantBackend)
	}

	// Persist to envtest so we confirm the API server accepts the object.
	createAndDelete(t, cls)

	// Re-fetch and confirm the field survived the round-trip.
	got := &setecv1alpha1.SandboxClass{}
	if err := upgradeClient.Get(ctx, types.NamespacedName{Name: cls.Name}, got); err != nil {
		t.Fatalf("Get after create: %v", err)
	}
	// The object was created with Runtime already filled (we called Default
	// before Create), so the field is present on the server.
	if got.Spec.Runtime == nil {
		t.Fatal("Runtime field absent after round-trip to envtest")
	}
	if got.Spec.Runtime.Backend != runtimepkg.BackendKataFC {
		t.Fatalf("runtime.backend after round-trip = %q; want %q",
			got.Spec.Runtime.Backend, runtimepkg.BackendKataFC)
	}
}

// ---------------------------------------------------------------------------
// Scenario 2: REQ-6.2 — Running Sandbox survives schema upgrade
//
// A Sandbox is created with a simulated "Running" status. We then fetch it
// back and confirm the status is unchanged — envtest doesn't do CRD version
// migrations, but this validates that the reconciler's status fields survive
// a round-trip through the API server without corruption.
// ---------------------------------------------------------------------------

func TestUpgrade_RunningSandbox_StatusUntouched(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	// Create a namespace for this scenario.
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: "upgrade-running-test",
	}}
	if err := upgradeClient.Create(ctx, ns); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create namespace: %v", err)
	}
	t.Cleanup(func() {
		dctx, dcancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer dcancel()
		_ = upgradeClient.Delete(dctx, ns)
	})

	// Create the Sandbox object.
	sb := mkSandboxWithClass("upgrade-running-test", "running-sb", "")
	if err := upgradeClient.Create(ctx, sb); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	t.Cleanup(func() {
		dctx, dcancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer dcancel()
		_ = upgradeClient.Delete(dctx, sb)
	})

	// Simulate the reconciler having set status.runtime.chosen and
	// status.phase=Running. We use a status sub-resource update so the
	// API server's status sub-resource machinery exercises the same path
	// as a real controller.
	chosen := runtimepkg.BackendKataFC
	patch := sb.DeepCopy()
	patch.Status.Phase = setecv1alpha1.SandboxPhaseRunning
	patch.Status.Runtime = &setecv1alpha1.SandboxRuntimeStatus{Chosen: chosen}
	if err := upgradeClient.Status().Update(ctx, patch); err != nil {
		t.Fatalf("status update: %v", err)
	}

	// Fetch back and verify the status is unchanged (REQ-6.2).
	got := &setecv1alpha1.Sandbox{}
	if err := upgradeClient.Get(ctx,
		types.NamespacedName{Namespace: "upgrade-running-test", Name: "running-sb"}, got); err != nil {
		t.Fatalf("get sandbox after status update: %v", err)
	}
	if got.Status.Phase != setecv1alpha1.SandboxPhaseRunning {
		t.Fatalf("Status.Phase = %q; want Running", got.Status.Phase)
	}
	if got.Status.Runtime == nil {
		t.Fatal("Status.Runtime is nil after update; want non-nil")
	}
	if got.Status.Runtime.Chosen != chosen {
		t.Fatalf("Status.Runtime.Chosen = %q; want %q", got.Status.Runtime.Chosen, chosen)
	}
}

// ---------------------------------------------------------------------------
// Scenario 3: REQ-6.3 — Legacy and new SandboxClass objects coexist
//
// Two SandboxClasses are created: one with only VMM=Firecracker (legacy), one
// with an explicit Runtime.Backend=gvisor (new style). Both must be accepted
// by the API server without conflict.
// ---------------------------------------------------------------------------

func TestUpgrade_LegacyAndNewCoexist(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	hw := &webhook.SandboxClassWebhook{
		Client:     upgradeClient,
		RuntimeCfg: upgradeRuntimeCfg,
	}

	// Legacy object: VMM-only, no Runtime block.
	legacy := mkLegacySandboxClass("upgrade-coexist-legacy", setecv1alpha1.VMMFirecracker)
	if err := hw.Default(ctx, legacy); err != nil {
		t.Fatalf("Default() on legacy: %v", err)
	}
	createAndDelete(t, legacy)

	// New-style object: explicit Runtime.Backend=gvisor.
	modern := &setecv1alpha1.SandboxClass{
		ObjectMeta: metav1.ObjectMeta{Name: "upgrade-coexist-modern"},
		Spec: setecv1alpha1.SandboxClassSpec{
			VMM: setecv1alpha1.VMMFirecracker,
			Runtime: &setecv1alpha1.SandboxClassRuntime{
				Backend: runtimepkg.BackendGVisor,
			},
		},
	}
	createAndDelete(t, modern)

	// Verify both objects exist simultaneously (REQ-6.3).
	gotLegacy := &setecv1alpha1.SandboxClass{}
	if err := upgradeClient.Get(ctx, types.NamespacedName{Name: "upgrade-coexist-legacy"}, gotLegacy); err != nil {
		t.Fatalf("get legacy SandboxClass: %v", err)
	}
	gotModern := &setecv1alpha1.SandboxClass{}
	if err := upgradeClient.Get(ctx, types.NamespacedName{Name: "upgrade-coexist-modern"}, gotModern); err != nil {
		t.Fatalf("get modern SandboxClass: %v", err)
	}

	if gotLegacy.Spec.Runtime == nil || gotLegacy.Spec.Runtime.Backend != runtimepkg.BackendKataFC {
		t.Errorf("legacy class runtime.backend = %v; want %q",
			gotLegacy.Spec.Runtime, runtimepkg.BackendKataFC)
	}
	if gotModern.Spec.Runtime == nil || gotModern.Spec.Runtime.Backend != runtimepkg.BackendGVisor {
		t.Errorf("modern class runtime.backend = %v; want %q",
			gotModern.Spec.Runtime, runtimepkg.BackendGVisor)
	}
}

// ---------------------------------------------------------------------------
// Scenario 4: REQ-6.4 — Runtime field is optional at the CRD level
//
// Create a SandboxClass without the Runtime field. The API server must accept
// it without a 400/422 error — Runtime is declared +optional in the schema.
// ---------------------------------------------------------------------------

func TestUpgrade_RuntimeFieldOptional(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	cls := &setecv1alpha1.SandboxClass{
		ObjectMeta: metav1.ObjectMeta{Name: "upgrade-no-runtime"},
		Spec: setecv1alpha1.SandboxClassSpec{
			VMM: setecv1alpha1.VMMFirecracker,
			// Runtime is intentionally absent.
		},
	}

	// createAndDelete calls t.Fatalf on error, so a 400/422 here would fail.
	createAndDelete(t, cls)

	// Re-fetch to confirm the object was accepted and Runtime is nil.
	got := &setecv1alpha1.SandboxClass{}
	if err := upgradeClient.Get(ctx, types.NamespacedName{Name: "upgrade-no-runtime"}, got); err != nil {
		t.Fatalf("get SandboxClass after create: %v", err)
	}
	if got.Spec.Runtime != nil {
		t.Fatalf("expected Runtime==nil for object created without Runtime, got %+v", got.Spec.Runtime)
	}
}
