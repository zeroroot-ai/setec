//go:build e2e

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

// Package e2e contains the hardware-gated end-to-end test suite for Setec.
//
// Every file in this package carries the `e2e` build tag so that
// `go test ./...` never compiles or runs these tests. The suite is intended
// to run only on a self-hosted CI runner with KVM, Kata Containers, and the
// `kata-fc` RuntimeClass installed. See .github/workflows/e2e.yml.
//
// The suite installs the charts/setec Helm chart into a throwaway namespace,
// runs the 6 scenarios from design.md against real Kata+Firecracker, and then
// uninstalls. Assertions are expressed through controller-runtime's typed
// client against the live cluster; cluster mutation (install/uninstall) goes
// through helm and kubectl subprocesses since that is idiomatic for E2E
// harnesses and keeps Go code uncoupled from a specific helm-sdk version.
package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
)

// Release / namespace / chart paths for the suite. The release name and
// namespace both embed a timestamp so parallel CI runs on the same cluster
// don't collide. Override via environment for local debugging.
var (
	helmReleaseName string
	testNamespace   string

	// chartPath points at the Helm chart under source control. Defaults to
	// the repo-relative path but callers can override for out-of-tree runs.
	chartPath string

	// kataRuntimeClass is the RuntimeClass the operator is configured to
	// target. It must already exist on the cluster (kata-deploy provisions
	// it). Scenario 5 temporarily removes and restores this resource.
	kataRuntimeClass string

	// kataOverhead is the kata-fc RuntimeClass's pod overhead, read from the
	// live cluster in preflight. installChart passes it to the chart so the
	// operator stamps Sandbox pods with overhead that matches the
	// RuntimeClass (the admission controller requires exact equality).
	kataOverhead corev1.ResourceList

	// imageTag is the tag of the locally-built setec component images that
	// the E2E workflow builds from the working tree and imports into the
	// cluster's container runtime. Every component (operator, runtime-agent)
	// shares this tag. Defaults to "dev", matching development/k3s. The chart
	// repositories are left at their defaults; only the tag is overridden,
	// and pullPolicy is forced to Never so a missed import fails loud
	// (ErrImageNeverPull) instead of silently pulling a stale/absent image.
	imageTag string

	// k8sClient is a typed controller-runtime client bound to the real
	// cluster. Tests use it for all in-cluster assertions.
	k8sClient client.Client

	// scheme is shared by the k8sClient and by tests that need to decode
	// YAML into typed objects.
	scheme = runtime.NewScheme()
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(setecv1alpha1.AddToScheme(scheme))
	utilruntime.Must(nodev1.AddToScheme(scheme))
}

// TestMain bootstraps the E2E environment before running any test function,
// and tears it down afterward regardless of outcome. Failures during setup
// cause the whole suite to bail out with a non-zero exit code so CI reports
// a real failure rather than a flurry of individual "kubectl not found" style
// errors.
func TestMain(m *testing.M) {
	stamp := time.Now().UTC().Format("20060102-150405")
	helmReleaseName = envOr("SETEC_E2E_RELEASE", fmt.Sprintf("setec-e2e-%s", stamp))
	testNamespace = envOr("SETEC_E2E_NAMESPACE", fmt.Sprintf("setec-e2e-%s", stamp))
	chartPath = envOr("SETEC_E2E_CHART", resolveChartPath())
	kataRuntimeClass = envOr("SETEC_E2E_RUNTIMECLASS", "kata-fc")
	imageTag = envOr("SETEC_E2E_IMAGE_TAG", "dev")

	if err := buildClient(); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: failed to build Kubernetes client: %v\n", err)
		os.Exit(1)
	}

	if err := preflight(); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: preflight failed: %v\n", err)
		os.Exit(1)
	}

	if err := installChart(); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: helm install failed: %v\n", err)
		// Best-effort teardown of whatever partial state got created.
		_ = uninstallChart()
		os.Exit(1)
	}

	code := m.Run()

	if err := uninstallChart(); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: helm uninstall warning: %v\n", err)
	}

	os.Exit(code)
}

// buildClient constructs a controller-runtime client against the active
// kubeconfig context. It uses the default loading rules so KUBECONFIG /
// --kubeconfig / in-cluster all work without extra wiring.
func buildClient() error {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	overrides := &clientcmd.ConfigOverrides{}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, overrides).ClientConfig()
	if err != nil {
		return fmt.Errorf("load kubeconfig: %w", err)
	}

	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return fmt.Errorf("build client: %w", err)
	}
	k8sClient = c
	return nil
}

// preflight verifies the environment has the tools and cluster features the
// suite requires. It fails fast with actionable messages instead of panicking
// deep inside a test case.
func preflight() error {
	for _, bin := range []string{"helm", "kubectl"} {
		if _, err := exec.LookPath(bin); err != nil {
			return fmt.Errorf("required binary %q not on PATH: %w", bin, err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Minimum sanity check: we can reach the cluster at all.
	var ns corev1.NamespaceList
	if err := k8sClient.List(ctx, &ns); err != nil {
		return fmt.Errorf("list namespaces: %w", err)
	}

	// The kata-fc RuntimeClass is required for all scenarios except #5,
	// which temporarily deletes and restores it.
	var rc nodev1.RuntimeClass
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: kataRuntimeClass}, &rc); err != nil {
		return fmt.Errorf("RuntimeClass %q not found on cluster: %w (install kata-deploy before running E2E)", kataRuntimeClass, err)
	}

	// Capture the RuntimeClass's pod overhead. The operator stamps Sandbox
	// VM pods with the chart's runtimes.<backend>.defaultOverhead, and the
	// RuntimeClass admission controller rejects any pod whose overhead does
	// not EQUAL the RuntimeClass's own overhead ("Pod's Overhead doesn't
	// match RuntimeClass's defined Overhead"). The chart default (128Mi)
	// will not match a kata-deploy-provisioned RuntimeClass (130Mi), so we
	// read the live overhead here and pass it through to helm (installChart)
	// instead of hard-coding a value that drifts with kata-deploy.
	if rc.Overhead != nil {
		kataOverhead = rc.Overhead.PodFixed
	}
	return nil
}

// installChart creates the test namespace and helm-installs the chart into
// it, waiting for the operator Deployment to become Ready before returning.
func installChart() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// Create the namespace. Helm could do this via --create-namespace, but
	// keeping it explicit lets us set labels deterministically later if we
	// need to enforce PodSecurity standards per-namespace.
	nsObj := &corev1.Namespace{}
	nsObj.Name = testNamespace
	if err := k8sClient.Create(ctx, nsObj); err != nil {
		return fmt.Errorf("create namespace %q: %w", testNamespace, err)
	}

	// Provision the webhook serving cert before install so the operator can
	// mount it on startup (the chart's fullname is the release name here, so
	// the Service is <release>-webhook and the cert Secret is
	// <release>-webhook-cert). caBundle is fed to the
	// ValidatingWebhookConfiguration so the API server trusts the webhook.
	webhookSvc := helmReleaseName + "-webhook"
	webhookCertSecret := helmReleaseName + "-webhook-cert"
	caBundle, err := createWebhookCertSecret(ctx, webhookCertSecret, webhookSvc, testNamespace)
	if err != nil {
		return err
	}

	args := []string{
		"install", helmReleaseName, chartPath,
		"--namespace", testNamespace,
		"--set", fmt.Sprintf("namespace=%s", testNamespace),
		"--set", fmt.Sprintf("runtimeClassName=%s", kataRuntimeClass),
		// The E2E cluster gets its kata-fc RuntimeClass from kata-deploy
		// (preflight requires it to pre-exist; scenario 5 deletes/restores
		// it). The chart must therefore NOT render its own kata-fc
		// RuntimeClass — otherwise helm refuses the install with an
		// ownership conflict ("cannot be imported into the current release"
		// because the object is already owned by the kata-deploy release).
		// runtimes.<backend>.install=false is the chart's documented knob
		// for "an external process owns the RuntimeClass lifecycle".
		"--set", "runtimes.kata-fc.install=false",
		// The component images are built from the working tree and imported
		// into the cluster runtime by the E2E workflow (there is no registry
		// to pull them from on the bare-metal runner). Point every deployed
		// component at the locally-built tag and force pullPolicy=Never so a
		// missed import fails loud instead of silently pulling from a
		// registry. node-agent is disabled by default, so only the operator
		// (image.*) and runtime-agent (runtimeAgent.image.*) need overriding.
		"--set", fmt.Sprintf("image.tag=%s", imageTag),
		"--set", "image.pullPolicy=Never",
		"--set", fmt.Sprintf("runtimeAgent.image.tag=%s", imageTag),
		"--set", "runtimeAgent.image.pullPolicy=Never",
		// Enable the SandboxClass/Sandbox admission webhook with the
		// self-signed serving cert created above, so TestPhase2_WebhookRejects
		// exercises real admission. failurePolicy stays Fail (the chart
		// default) — the cert + caBundle must be correct or Sandbox creation
		// fails closed.
		"--set", "webhook.enabled=true",
		"--set", fmt.Sprintf("webhook.certSecret=%s", webhookCertSecret),
		"--set-string", fmt.Sprintf("webhook.caBundle=%s", caBundle),
		"--wait",
		"--timeout", "5m",
	}

	// Match the chart's stamped Sandbox overhead to the live RuntimeClass's
	// overhead (captured in preflight) so the admission controller accepts
	// Sandbox pods. The chart key is the backend name (kata-fc), which equals
	// the RuntimeClass name on this cluster.
	if cpu, ok := kataOverhead[corev1.ResourceCPU]; ok {
		args = append(args, "--set", fmt.Sprintf("runtimes.kata-fc.defaultOverhead.cpu=%s", cpu.String()))
	}
	if mem, ok := kataOverhead[corev1.ResourceMemory]; ok {
		args = append(args, "--set", fmt.Sprintf("runtimes.kata-fc.defaultOverhead.memory=%s", mem.String()))
	}

	cmd := exec.Command("helm", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("helm install: %w", err)
	}

	// The operator's /readyz gates on prereqs, not on the webhook server
	// having begun serving on :9443. With the webhook enabled + failurePolicy
	// Fail, a Sandbox/SandboxClass create issued before the server is up gets
	// a 502 "failed calling webhook". Block until the webhook actually admits
	// a request so test bodies don't race it.
	if err := waitForWebhookReady(ctx); err != nil {
		return fmt.Errorf("webhook did not become ready: %w", err)
	}
	return nil
}

// waitForWebhookReady polls until the admission webhook is serving by issuing a
// real admission request (create+delete a throwaway SandboxClass, which the
// mutating msandboxclass webhook intercepts). A 502 / "failed calling webhook"
// means the server is not up yet; any other outcome (success, or a validation
// rejection) proves the webhook responded.
func waitForWebhookReady(ctx context.Context) error {
	deadline := time.Now().Add(2 * time.Minute)
	probe := &setecv1alpha1.SandboxClass{}
	probe.Name = "webhook-readiness-probe"
	probe.Spec = setecv1alpha1.SandboxClassSpec{VMM: setecv1alpha1.VMMFirecracker}
	for {
		err := k8sClient.Create(ctx, probe)
		if err == nil {
			_ = k8sClient.Delete(ctx, probe)
			return nil
		}
		// A webhook that responded (even to reject) proves it is serving.
		if !strings.Contains(err.Error(), "failed calling webhook") &&
			!strings.Contains(err.Error(), "connection refused") &&
			!strings.Contains(err.Error(), "502") {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("webhook still unavailable after 2m: %w", err)
		}
		time.Sleep(2 * time.Second)
	}
}

// uninstallChart removes the Helm release and then deletes the namespace.
// It is best-effort: we log but don't fail if teardown partially fails,
// because the surrounding CI runner is responsible for returning the host
// to a clean state between jobs.
func uninstallChart() error {
	var firstErr error
	cmd := exec.Command("helm", "uninstall", helmReleaseName, "--namespace", testNamespace, "--wait", "--timeout", "2m")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		firstErr = fmt.Errorf("helm uninstall: %w", err)
	}

	// Delete namespace (ignore not-found; helm uninstall does not remove it).
	delCmd := exec.Command("kubectl", "delete", "namespace", testNamespace, "--wait=true", "--ignore-not-found=true", "--timeout=2m")
	delCmd.Stdout = os.Stdout
	delCmd.Stderr = os.Stderr
	if err := delCmd.Run(); err != nil && firstErr == nil {
		firstErr = fmt.Errorf("delete namespace: %w", err)
	}

	return firstErr
}

// resolveChartPath walks up from the test file location to find charts/setec.
// Tests run with cwd=test/e2e, so ../../charts/setec is the canonical path.
// We check existence to give a friendlier error than helm's opaque output.
func resolveChartPath() string {
	candidates := []string{
		"../../charts/setec",
		"charts/setec",
	}
	for _, c := range candidates {
		if stat, err := os.Stat(c); err == nil && stat.IsDir() {
			return c
		}
	}
	// Fall through; helm will fail with a clear error if the path is wrong.
	return "../../charts/setec"
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
