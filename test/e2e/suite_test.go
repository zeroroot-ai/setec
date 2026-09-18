// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

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
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
	"github.com/zeroroot-ai/setec/internal/chartname"
)

// Release / namespace / chart paths for the suite. The release name and
// namespace both embed a timestamp so parallel CI runs on the same cluster
// don't collide. Override via environment for local debugging.
var (
	helmReleaseName string
	testNamespace   string

	// chartFullname is the chart's `setec.fullname` for helmReleaseName:
	// the prefix of every object the release renders (the operator
	// Deployment is the fullname itself, then <fullname>-runtime-agent,
	// <fullname>-node-agent, <fullname>-installer, <fullname>-webhook). It
	// equals the release name only when that name contains "setec"; the
	// chain-6 job's chain6-exit renders chain6-exit-setec. Derived by
	// internal/chartname, which is unit-tested against the chart.
	chartFullname string

	// sandboxNamespace is where every scenario's Sandbox runs. It is a
	// sibling of testNamespace, never testNamespace itself (setec#10).
	//
	// The chart is explicit that a Sandbox namespace is a Sandbox-only
	// namespace: `sandboxNamespaces` is a list distinct from `namespace`,
	// the baseline default-deny NetworkPolicy and the host-access guard land
	// on every namespace in it, and the chart refuses to render a values
	// file that lists the release namespace there. A suite that put its
	// Sandboxes beside the operator was the one configuration reaching the
	// state the chart forbids, and so was not testing a supported
	// deployment.
	//
	// One fixed namespace per job, not one per scenario: the chart requires
	// listed namespaces to exist at install time, and one name keeps the
	// workflow's reap and diagnostics steps able to find everything. The
	// scenarios that test multi-tenant behavior keep creating their own
	// tenant namespaces through createTenantNamespace; that is what they
	// test. Override with SETEC_E2E_SANDBOX_NAMESPACE.
	sandboxNamespace string

	// restConfig is the kubeconfig the suite resolved. frontend.Service needs
	// it to run commands: Exec goes through the Kubernetes pods/exec
	// subresource, and a Service with a nil RESTConfig reports that it cannot
	// exec rather than pretending.
	restConfig *rest.Config

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

	// backendOverheads maps each enabled backend (kata-fc, kata-qemu, gvisor)
	// to its live RuntimeClass pod overhead, captured in preflight and applied
	// to runtimes.<backend>.defaultOverhead at install.
	backendOverheads map[string]corev1.ResourceList

	// imageTag is the tag of the locally-built setec component images that
	// the E2E workflow builds from the working tree and imports into the
	// cluster's container runtime. Every component (operator, runtime-agent)
	// shares this tag. Defaults to "dev", matching development/k3s. The chart
	// repositories are left at their defaults; only the tag is overridden,
	// and pullPolicy defaults to Never so a missed import fails loud
	// (ErrImageNeverPull) instead of silently pulling a stale/absent image.
	imageTag string

	// imagePullPolicy overrides the chart's pullPolicy for every setec
	// component. Never is right for a runner that side-loads images into
	// the node's runtime; it is WRONG on a shared cluster like staging
	// EKS, where the runner is an ordinary pod with no access to any
	// node's image store and the images have to come from ghcr. Set
	// SETEC_E2E_IMAGE_PULL_POLICY=IfNotPresent there.
	imagePullPolicy string

	// imageRepo / runtimeAgentImageRepo override the chart's image
	// repositories. Needed whenever the images under test are not the
	// chart defaults — e.g. a PR build pushed to a sha-tagged ghcr repo.
	imageRepo             string
	runtimeAgentImageRepo string

	// webhookEnabled controls whether the throwaway release installs the
	// admission webhook. The ValidatingWebhookConfiguration is
	// CLUSTER-scoped with failurePolicy=Fail, so on a SHARED cluster a run
	// that dies before cleanup fails every Sandbox/SandboxClass write in
	// the whole cluster closed until someone deletes it by hand. Default
	// stays true (the isolated-cluster case, and the webhook scenarios
	// need it); the staging job sets SETEC_E2E_WEBHOOK=0.
	webhookEnabled bool

	// k8sClient is a typed controller-runtime client bound to the real
	// cluster. Tests use it for all in-cluster assertions.
	k8sClient client.Client

	// sessionS3 carries the session-checkpoint object-store settings
	// (setec#194, ADR-0007). The checkpoint scenarios are the only ones
	// that need a node-agent at all, so the whole DaemonSet stays out of
	// the base install until SETEC_E2E_S3 turns it on.
	sessionS3 sessionS3Config

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
	chartFullname = chartname.Fullname(helmReleaseName)
	testNamespace = envOr("SETEC_E2E_NAMESPACE", fmt.Sprintf("setec-e2e-%s", stamp))
	sandboxNamespace = envOr("SETEC_E2E_SANDBOX_NAMESPACE", testNamespace+"-sandboxes")
	if sandboxNamespace == testNamespace {
		fmt.Fprintf(os.Stderr, "e2e: SETEC_E2E_SANDBOX_NAMESPACE=%q is the release namespace; Sandboxes need a namespace of their own (setec#10)\n", sandboxNamespace)
		os.Exit(1)
	}
	chartPath = envOr("SETEC_E2E_CHART", resolveChartPath())
	kataRuntimeClass = envOr("SETEC_E2E_RUNTIMECLASS", "kata-fc")
	imageTag = envOr("SETEC_E2E_IMAGE_TAG", "dev")
	imagePullPolicy = envOr("SETEC_E2E_IMAGE_PULL_POLICY", "Never")
	imageRepo = os.Getenv("SETEC_E2E_IMAGE_REPO")
	runtimeAgentImageRepo = os.Getenv("SETEC_E2E_RUNTIME_AGENT_IMAGE_REPO")
	webhookEnabled = envOr("SETEC_E2E_WEBHOOK", "1") != "0"

	var err error
	if sessionS3, err = loadSessionS3Config(); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: session-checkpoint S3 configuration is incomplete: %v\n", err)
		os.Exit(1)
	}

	if err := buildClient(); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: failed to build Kubernetes client: %v\n", err)
		os.Exit(1)
	}

	// Refuse to install into a cluster nobody asked for (setec#298).
	//
	// This TestMain does `helm install` + namespace create BEFORE any test
	// runs, against whatever kubeconfig context happens to be current. Running
	// `go test -tags=e2e ./test/e2e` on a laptop with a staging context loaded
	// therefore installs a full setec release into staging — no flag, no
	// prompt, no mention in the command. The suite is destructive by
	// construction, so consent has to be explicit rather than implied by a
	// context that was current for some unrelated reason.
	if err := requireClusterConsent(); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: %v\n", err)
		os.Exit(1)
	}

	if err := preflight(); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: preflight failed: %v\n", err)
		os.Exit(1)
	}

	if err := installChart(); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: helm install failed: %v\n", err)
		// Dump the wreckage BEFORE tearing it down. The teardown below is what
		// makes an install failure unreadable: by the time the workflow's own
		// "Collect diagnostics" step runs, this has already deleted the
		// namespace, so the step reports "No resources found" and the only
		// evidence left is helm's one-line verdict — "DaemonSet ... not ready,
		// Available: 3/4", with no way to learn WHICH replica or why (observed
		// on run 31916255452).
		dumpInstallFailureState()
		// Best-effort teardown of whatever partial state got created.
		_ = uninstallChart()
		os.Exit(1)
	}

	// The suite-owned default SandboxClass (setec#330). It has to exist
	// before the first scenario because minimalSpec names it, and it has to
	// be created here rather than by the chart because the install sets
	// sandboxClasses.enabled=false — the shared staging cluster already owns
	// objects of that name under a different release.
	if err := ensureDefaultSandboxClass(); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: default SandboxClass: %v\n", err)
		_ = uninstallChart()
		os.Exit(1)
	}

	code := m.Run()

	// Cluster-scoped and NOT release-prefixed, so helm uninstall will not
	// reap it. Leaving it behind poisons the next run: a stale class with a
	// stale toleration is exactly the state this issue was about.
	if err := deleteDefaultSandboxClass(); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: default SandboxClass cleanup warning: %v\n", err)
	}

	if err := uninstallChart(); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: helm uninstall warning: %v\n", err)
	}

	os.Exit(code)
}

// sessionS3Config is the resolved session-checkpoint object-store
// configuration (setec#194, ADR-0007). Zero value = disabled, which is
// what every environment without a checkpoint bucket gets.
type sessionS3Config struct {
	// enabled mirrors SETEC_E2E_S3. When false the base install keeps
	// nodeAgent.enabled=false and the checkpoint scenarios skip loudly.
	enabled bool
	// bucket is the checkpoint bucket. Required when enabled.
	bucket string
	// region is the bucket's region (signing region for the AWS SDK).
	region string
	// prefix scopes every object key. The staging IAM policy is written
	// against this prefix, so a mismatch reads as AccessDenied.
	prefix string
	// endpoint is empty for real S3 and set for MinIO / other
	// self-hosted S3-compatibles.
	endpoint string
	// roleARN annotates the node-agent ServiceAccount for IRSA. Empty
	// on non-EKS environments, where credentialsSecret or the ambient
	// credential chain applies instead.
	roleARN string
	// credentialsSecret names a pre-existing Secret of AWS_ACCESS_KEY_ID
	// / AWS_SECRET_ACCESS_KEY, for environments with no IRSA (MinIO).
	credentialsSecret string
	// nodeAgentImageTag / nodeAgentImageRepo pin the node-agent image.
	// The checkpoint scenarios are the only ones that run a node-agent,
	// so this is the one component the base install never references —
	// which also means nothing else would catch a missing image.
	nodeAgentImageTag  string
	nodeAgentImageRepo string
}

// loadSessionS3Config reads the SETEC_E2E_S3* environment and refuses
// a half-configured setup. Failing here rather than at install time
// keeps the failure legible: "you set SETEC_E2E_S3=1 but no bucket" is
// a configuration mistake, not a checkpoint defect, and the two must
// never be confusable in a first-execution run.
func loadSessionS3Config() (sessionS3Config, error) {
	if os.Getenv("SETEC_E2E_S3") == "" {
		return sessionS3Config{}, nil
	}
	cfg := sessionS3Config{
		enabled:            true,
		bucket:             os.Getenv("SETEC_E2E_S3_BUCKET"),
		region:             envOr("SETEC_E2E_S3_REGION", "us-east-1"),
		prefix:             os.Getenv("SETEC_E2E_S3_PREFIX"),
		endpoint:           os.Getenv("SETEC_E2E_S3_ENDPOINT"),
		roleARN:            os.Getenv("SETEC_E2E_S3_ROLE_ARN"),
		credentialsSecret:  os.Getenv("SETEC_E2E_S3_CREDENTIALS_SECRET"),
		nodeAgentImageTag:  envOr("SETEC_E2E_NODE_AGENT_IMAGE_TAG", imageTag),
		nodeAgentImageRepo: os.Getenv("SETEC_E2E_NODE_AGENT_IMAGE_REPO"),
	}
	if cfg.bucket == "" {
		return sessionS3Config{}, fmt.Errorf(
			"SETEC_E2E_S3=1 requires SETEC_E2E_S3_BUCKET (the checkpoint bucket); " +
				"refusing to install a node-agent whose checkpoint backend would fail closed at the first suspend")
	}
	if cfg.endpoint == "" && cfg.roleARN == "" && cfg.credentialsSecret == "" {
		return sessionS3Config{}, fmt.Errorf(
			"SETEC_E2E_S3=1 against real AWS S3 requires SETEC_E2E_S3_ROLE_ARN (IRSA) " +
				"or SETEC_E2E_S3_CREDENTIALS_SECRET; the node-agent would otherwise fall back to the node " +
				"instance profile, which has no access to the checkpoint prefix and reports AccessDenied " +
				"as an opaque checkpoint failure")
	}
	return cfg, nil
}

// helmArgs renders the chart overrides that turn the node-agent's S3
// session-checkpoint backend on.
func (c sessionS3Config) helmArgs() []string {
	if !c.enabled {
		return nil
	}
	args := []string{
		// The checkpoint backend lives in the node-agent, and the whole
		// snapshots subtree (including the operator's node-agent dialer
		// and its mTLS) is gated behind snapshots.enabled — which
		// snapshotsEnabled() turns on for this axis, in installChart, so
		// there is exactly one place that decides it.
		"--set", "nodeAgent.enabled=true",
		"--set", fmt.Sprintf("nodeAgent.image.tag=%s", c.nodeAgentImageTag),
		"--set", fmt.Sprintf("nodeAgent.image.pullPolicy=%s", imagePullPolicy),
		"--set", "snapshots.s3.enabled=true",
		"--set-string", fmt.Sprintf("snapshots.s3.bucket=%s", c.bucket),
		"--set-string", fmt.Sprintf("snapshots.s3.region=%s", c.region),
		"--set-string", fmt.Sprintf("snapshots.s3.prefix=%s", c.prefix),
		"--set-string", fmt.Sprintf("snapshots.s3.endpoint=%s", c.endpoint),
		// Real S3 rejects path-style addressing; MinIO requires it.
		"--set", fmt.Sprintf("snapshots.s3.pathStyle=%t", c.endpoint != ""),
		// The operator<->node-agent channel is mTLS, and the chart's
		// certManager path stays OFF here: it needs the right to create
		// an Issuer, which the ARC runner does not hold. installChart
		// mints all three Secrets from one CA instead and confirms the
		// CA with caProvided (see nodeagentcert_test.go).
		"--set", "snapshots.mTLS.certManager.enabled=false",
	}
	if c.nodeAgentImageRepo != "" {
		args = append(args, "--set-string",
			fmt.Sprintf("nodeAgent.image.repository=%s", c.nodeAgentImageRepo))
	}
	if c.roleARN != "" {
		args = append(args, "--set-string",
			fmt.Sprintf("nodeAgent.serviceAccount.annotations.eks\\.amazonaws\\.com/role-arn=%s", c.roleARN))
	}
	if c.credentialsSecret != "" {
		args = append(args, "--set-string",
			fmt.Sprintf("snapshots.s3.credentialsSecret=%s", c.credentialsSecret))
	}
	return args
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
	// Exec goes through the Kubernetes pods/exec subresource, so a
	// frontend.Service that runs commands needs the REST config as well as the
	// controller-runtime client. Keeping it here means a test does not rebuild
	// (and re-resolve) the kubeconfig it already loaded.
	restConfig = cfg
	return nil
}

// requireClusterConsent refuses to run the destructive suite unless the caller
// has named the cluster they intend to install into (setec#298).
//
// The suite installs a helm release and creates a namespace before the first
// test executes. Consent is therefore required up front, and "the context was
// already current" is not consent — that is how a routine `go test` run against
// a laptop with staging loaded becomes an install into staging.
//
// Two ways to consent:
//
//   - SETEC_E2E_CONTEXT=<name> — the current kubeconfig context must match
//     exactly. This is the form CI uses, and the form that makes a mistake
//     impossible rather than merely unlikely.
//   - SETEC_E2E_ALLOW_ANY_CLUSTER=1 — deliberate escape hatch for throwaway
//     kind clusters, where pinning a generated context name is pointless.
//
// A run with neither set is refused with the current context named, so the
// operator can see what they were about to install into.
func requireClusterConsent() error {
	// The explicit escape hatch, checked first because it is the only form
	// available to an in-cluster caller (see below).
	allowAny := os.Getenv("SETEC_E2E_ALLOW_ANY_CLUSTER") == "1"

	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	raw, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{}).RawConfig()
	if err != nil {
		return fmt.Errorf("read kubeconfig to verify the target cluster: %w", err)
	}
	current := raw.CurrentContext

	// In-cluster (an ARC runner pod): there is no kubeconfig and therefore no
	// context name to match, so SETEC_E2E_CONTEXT cannot express consent here.
	// The pod's own ServiceAccount is the credential, and the pod was
	// deliberately scheduled into this cluster by the workflow — but "the
	// workflow scheduled me" is not the same as "the workflow intends a
	// destructive install", so consent still has to be explicit.
	if current == "" && os.Getenv("KUBERNETES_SERVICE_HOST") != "" {
		if allowAny {
			fmt.Fprintln(os.Stderr, "e2e: in-cluster credentials, SETEC_E2E_ALLOW_ANY_CLUSTER=1 — proceeding")
			return nil
		}
		return errors.New(
			"refusing to run: using in-cluster credentials (no kubeconfig context to name), and this " +
				"suite helm-installs a setec release and creates a namespace BEFORE the first test runs.\n" +
				"A CI job that means to do that must say so:\n" +
				"    SETEC_E2E_ALLOW_ANY_CLUSTER: \"1\"")
	}

	if want := os.Getenv("SETEC_E2E_CONTEXT"); want != "" {
		if current != want {
			return fmt.Errorf(
				"refusing to run: SETEC_E2E_CONTEXT=%q but the current kubeconfig context is %q.\n"+
					"This suite helm-installs and creates a namespace before any test runs; "+
					"it will not do that to a cluster you did not name.", want, current)
		}
		return nil
	}

	if allowAny {
		fmt.Fprintf(os.Stderr, "e2e: SETEC_E2E_ALLOW_ANY_CLUSTER=1 — installing into context %q\n", current)
		return nil
	}

	return fmt.Errorf(
		"refusing to run against context %q: this suite is destructive — it helm-installs a "+
			"setec release and creates a namespace BEFORE the first test runs.\n"+
			"Name the cluster you mean:\n"+
			"    SETEC_E2E_CONTEXT=%s go test -tags=e2e ./test/e2e/...\n"+
			"or, for a throwaway kind cluster:\n"+
			"    SETEC_E2E_ALLOW_ANY_CLUSTER=1 go test -tags=e2e ./test/e2e/...",
		current, current)
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

	// Capture overhead for the other backends the suite enables (kata-qemu,
	// gvisor) so TestRuntimeBackends_Smoke's Sandboxes pass the same overhead
	// equality check. A backend whose RuntimeClass is absent or carries no
	// overhead simply gets no override.
	backendOverheads = map[string]corev1.ResourceList{}
	if kataOverhead != nil {
		backendOverheads["kata-fc"] = kataOverhead
	}
	for _, b := range []string{"kata-qemu", "gvisor"} {
		var brc nodev1.RuntimeClass
		if err := k8sClient.Get(ctx, client.ObjectKey{Name: b}, &brc); err != nil {
			continue
		}
		if brc.Overhead != nil {
			backendOverheads[b] = brc.Overhead.PodFixed
		} else {
			backendOverheads[b] = corev1.ResourceList{}
		}
	}
	return nil
}

// installChart creates the test namespace and helm-installs the chart into
// it, waiting for the operator Deployment to become Ready before returning.
// ensureDefaultSandboxClass creates the suite-owned SandboxClass that
// minimalSpec Sandboxes run under (setec#330).
//
// Deliberately NOT `spec.default: true`. SandboxClass is cluster-scoped and
// the shared staging cluster already carries a default (`tool`, from the
// gibson umbrella release); a second one makes the pair ambiguous and the
// resolver refuses to default at all — which would break every OTHER
// workload on the cluster for the length of the run, not just this suite.
// minimalSpec names the class explicitly instead.
//
// Idempotent: a run that died before its cleanup leaves the object behind,
// and the toleration may have changed since. Update rather than skip, so a
// stale class from a previous run cannot silently govern this one.
func ensureDefaultSandboxClass() error {
	ctx := context.Background()
	desired := newSandboxClass(e2eDefaultClassName, setecv1alpha1.SandboxClassSpec{
		VMM:              setecv1alpha1.VMMFirecracker,
		RuntimeClassName: kataRuntimeClass,
		// Generous ceiling: this class exists to carry a toleration, not to
		// constrain anything. Scenarios that test ceilings build their own.
		MaxResources: &setecv1alpha1.Resources{
			VCPU:   8,
			Memory: resource.MustParse("16Gi"),
		},
		DefaultNetworkMode: setecv1alpha1.NetworkModeExternalOnly,
		AllowedNetworkModes: []setecv1alpha1.NetworkMode{
			setecv1alpha1.NetworkModeExternalOnly,
			setecv1alpha1.NetworkModeEgressAllowList,
			setecv1alpha1.NetworkModeNone,
		},
	})

	err := k8sClient.Create(ctx, desired)
	if err == nil {
		return nil
	}
	if !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create %s: %w", e2eDefaultClassName, err)
	}

	var existing setecv1alpha1.SandboxClass
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: e2eDefaultClassName}, &existing); err != nil {
		return fmt.Errorf("get %s after AlreadyExists: %w", e2eDefaultClassName, err)
	}
	existing.Spec = desired.Spec
	if err := k8sClient.Update(ctx, &existing); err != nil {
		return fmt.Errorf("update stale %s: %w", e2eDefaultClassName, err)
	}
	return nil
}

// deleteDefaultSandboxClass removes the suite-owned class. Absent is success.
func deleteDefaultSandboxClass() error {
	err := k8sClient.Delete(context.Background(), &setecv1alpha1.SandboxClass{
		ObjectMeta: metav1.ObjectMeta{Name: e2eDefaultClassName},
	})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete %s: %w", e2eDefaultClassName, err)
	}
	return nil
}

func installChart() error {
	// Must outlast the helm --timeout below, or this context cancels the
	// install that is still legitimately waiting for a rollout.
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()

	// Create the namespace. Helm could do this via --create-namespace, but
	// keeping it explicit lets us set labels deterministically later if we
	// need to enforce PodSecurity standards per-namespace.
	nsObj := &corev1.Namespace{}
	nsObj.Name = testNamespace
	if err := k8sClient.Create(ctx, nsObj); err != nil {
		return fmt.Errorf("create namespace %q: %w", testNamespace, err)
	}
	// The Sandbox namespace exists before the install: the chart renders
	// the baseline NetworkPolicy and the host guard into every namespace it
	// lists, and requires those namespaces to be there already.
	sbNs := &corev1.Namespace{}
	sbNs.Name = sandboxNamespace
	if err := k8sClient.Create(ctx, sbNs); err != nil {
		return fmt.Errorf("create sandbox namespace %q: %w", sandboxNamespace, err)
	}

	// Provision the webhook serving cert before install so the operator can
	// mount it on startup: the Service is <fullname>-webhook and the cert
	// Secret is <fullname>-webhook-cert. caBundle is fed to the
	// ValidatingWebhookConfiguration so the API server trusts the webhook.
	var webhookCertSecret, caBundle string
	if webhookEnabled {
		var err error
		webhookSvc := chartFullname + "-webhook"
		webhookCertSecret = chartFullname + "-webhook-cert"
		caBundle, err = createWebhookCertSecret(ctx, webhookCertSecret, webhookSvc, testNamespace)
		if err != nil {
			return err
		}
	}

	// Same story one layer down: with snapshots on, the chart mounts three
	// operator<->node-agent mTLS Secrets it does not fully create, and
	// `helm --wait` blocks forever on a Pod whose Secret volume never
	// resolves. Mint them here, from ONE CA, before the install. See
	// nodeagentcert_test.go for why the suite does this rather than the
	// chart's cert-manager path (setec#320: the ARC runner cannot create an
	// Issuer, and issuing both leaves from a selfsigned ClusterIssuer gives
	// them no shared trust root anyway).
	if snapshotsEnabled() {
		if err := createNodeAgentMTLSSecrets(ctx, chartFullname, testNamespace); err != nil {
			return err
		}
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
		"--set", fmt.Sprintf("image.pullPolicy=%s", imagePullPolicy),
		"--set", fmt.Sprintf("runtimeAgent.image.tag=%s", imageTag),
		"--set", fmt.Sprintf("runtimeAgent.image.pullPolicy=%s", imagePullPolicy),
		// Node prep on the E2E host is kata-deploy-owned (preflight above
		// requires the kata-fc RuntimeClass to pre-exist), so the portable
		// installer DaemonSet (ADR-0003) stays out of the base install.
		// TestInstaller_Converges opts back in behind SETEC_E2E_INSTALLER=1
		// with the locally-built installer image.
		"--set", "installer.enabled=false",
		// The Sandbox namespace, declared to the chart (setec#10). The
		// release binds the operator's Pod-write RBAC there and covers it
		// with the baseline default-deny NetworkPolicy and the host-access
		// guard, which is the topology the chart documents and the one
		// every scenario that goes through newSandbox now runs in.
		"--set", fmt.Sprintf("sandboxNamespaces={%s}", sandboxNamespace),
		// Still required (setec#157): the multi-tenant scenarios create
		// their own tenant namespaces from the test bodies at run time
		// (createTenantNamespace: p2-netpol, p3-roundtrip, e2e-egress, and
		// more), and the operator has to be able to write Pods there too.
		// The chart's documented opt-out covers that, scoped to a throwaway
		// release that is uninstalled at the end of the run. It goes away
		// when those scenarios name their namespaces at install time.
		"--set", "rbac.allowClusterWideSandboxWrite=true",
		// The chart ships two SandboxClasses, `tool` and `connector`, and
		// SandboxClass is CLUSTER-scoped and NOT release-prefixed. Both
		// already exist on the shared staging cluster carrying no helm
		// ownership metadata at all, so a throwaway release rendering them
		// is refused ("exists and cannot be imported into the current
		// release"). Nothing is lost: every scenario in this suite builds
		// its own SandboxClass (e2e-tight, runc-dev-cls, fallback-test-cls,
		// e2e-session-checkpoint-*, …) and none references the chart's.
		// Same opt-out the roundtrip job takes, for the same reason.
		"--set", "sandboxClasses.enabled=false",
		// No --wait (setec#12). helm's wait calls a DaemonSet ready only at
		// numberAvailable == desiredNumberScheduled, and the DaemonSet
		// controller schedules a pod onto NotReady and unreachable nodes too
		// (it stamps the not-ready and unreachable NoExecute tolerations on
		// every DaemonSet pod itself), so a node that is gone holds a Pending
		// pod that no timeout outlasts. Staging always has one: the metal
		// node the previous run provisioned sits NotReady in the API while
		// Karpenter drains it. waitForInstallReady below gates on the nodes
		// the cluster reports Ready instead.
	}

	// WITHOUT snapshots.enabled THE SUITE IS A GREEN ALL-SKIP on the Phase 3
	// half. phase3Enabled() greps the operator Deployment for
	// `--snapshots-enabled`, which the chart only renders under
	// snapshots.enabled; left off, every Phase 3 scenario calls t.Skip —
	// including BOTH ADR-0005 invariants (TestPhase3_RestoredClonesDivergeInRNG,
	// TestPhase3_RestoredClonesHaveUniqueIdentity) and
	// TestGate_UnverifiedWarmStartFailsClosed — and the run reports PASS having
	// verified none of them. That is the failure mode TestEnv_KVMPresent exists
	// to prevent, arriving through a different door.
	if snapshotsEnabled() {
		args = append(args,
			"--set", "snapshots.enabled=true",
			// The acknowledgement the chart requires (setec#326): it refuses
			// to render a release that mounts snapshots.mTLS.caSecret without
			// someone declaring the Secret exists, because a non-optional
			// missing Secret wedges the Pods and surfaces as an opaque
			// "context deadline exceeded" ten minutes into the install.
			//
			// setec#331 spelled out what that leaves outstanding: the chart
			// refuses to render, but it does not conjure a CA, so turning
			// snapshots on still needs somebody to create setec-nodeagent-ca
			// signing BOTH leaves — with a selfsigned issuer each leaf is its
			// own root and the two sides do not trust each other — and to pass
			// this flag. "Somebody" is now this suite: createNodeAgentMTLSSecrets
			// a few lines above mints exactly that, one CA and both leaves from
			// it, which is the part the chart's own cert-manager path cannot do
			// (setec#320, still open for the chart-managed CA Issuer).
			"--set", "snapshots.mTLS.caProvided=true",
		)
	}

	// Enable the SandboxClass/Sandbox admission webhook with the
	// self-signed serving cert created above, so TestPhase2_WebhookRejects
	// exercises real admission. failurePolicy stays Fail (the chart
	// default) — the cert + caBundle must be correct or Sandbox creation
	// fails closed.
	if webhookEnabled {
		args = append(args,
			"--set", "webhook.enabled=true",
			"--set", fmt.Sprintf("webhook.certSecret=%s", webhookCertSecret),
			"--set-string", fmt.Sprintf("webhook.caBundle=%s", caBundle),
		)
	} else {
		args = append(args, "--set", "webhook.enabled=false")
	}
	if imageRepo != "" {
		args = append(args, "--set-string", fmt.Sprintf("image.repository=%s", imageRepo))
	}
	if runtimeAgentImageRepo != "" {
		args = append(args, "--set-string", fmt.Sprintf("runtimeAgent.image.repository=%s", runtimeAgentImageRepo))
	}
	args = append(args, sessionS3.helmArgs()...)

	// Enable every backend TestRuntimeBackends_Smoke exercises (kata-fc is
	// already enabled by default). With the webhook on, the vsandboxclass
	// validator rejects a SandboxClass whose backend is not enabled. Their
	// RuntimeClasses are provisioned externally (kata-deploy / gvisor install),
	// so install=false avoids the same ownership conflict as kata-fc. Match
	// each backend's stamped Sandbox overhead to its live RuntimeClass overhead
	// (captured in preflight) so the RuntimeClass admission controller accepts
	// the Sandbox pods.
	for _, b := range []string{"kata-fc", "kata-qemu", "gvisor"} {
		ovh, ok := backendOverheads[b]
		if !ok {
			continue // backend's RuntimeClass not present on this cluster
		}
		if b != "kata-fc" {
			args = append(args,
				"--set", fmt.Sprintf("runtimes.%s.enabled=true", b),
				"--set", fmt.Sprintf("runtimes.%s.install=false", b),
			)
		}
		if len(ovh) == 0 {
			// RuntimeClass declares no overhead (e.g. gvisor's runsc sentry is
			// not a VMM). The chart still defaults a non-zero defaultOverhead,
			// which the operator would stamp — and the admission controller
			// rejects "Pod Overhead set without corresponding RuntimeClass
			// defined Overhead". Null it so the operator stamps none.
			args = append(args, "--set", fmt.Sprintf("runtimes.%s.defaultOverhead=null", b))
			continue
		}
		if cpu, ok := ovh[corev1.ResourceCPU]; ok {
			args = append(args, "--set", fmt.Sprintf("runtimes.%s.defaultOverhead.cpu=%s", b, cpu.String()))
		}
		if mem, ok := ovh[corev1.ResourceMemory]; ok {
			args = append(args, "--set", fmt.Sprintf("runtimes.%s.defaultOverhead.memory=%s", b, mem.String()))
		}
	}

	args = append(args, crdInstallArgs()...)

	// SETEC_E2E_EXTRA_SET is a comma-separated list of extra `--set` values,
	// appended LAST so it can override anything above.
	//
	// It exists for the chain-6 exit test, which must run without staging or
	// prod: on a kind cluster there is no KVM, so the sandbox runs on the
	// `runc` backend, and the settings that enable it are per-run rather than
	// a property of this suite. Hard-coding a second backend here would mean
	// every metal run also carried it.
	//
	// Deliberately not a values FILE: a file makes it easy to smuggle in a
	// whole alternate configuration, and a short --set list stays legible in
	// the workflow that sets it.
	if extra := strings.TrimSpace(os.Getenv("SETEC_E2E_EXTRA_SET")); extra != "" {
		for _, kv := range strings.Split(extra, ",") {
			kv = strings.TrimSpace(kv)
			if kv == "" {
				continue
			}
			args = append(args, "--set", kv)
		}
	}

	cmd := exec.Command("helm", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("helm install: %w", err)
	}

	if err := waitForInstallReady(ctx); err != nil {
		return err
	}

	// The operator's /readyz gates on prereqs, not on the webhook server
	// having begun serving on :9443. With the webhook enabled + failurePolicy
	// Fail, a Sandbox/SandboxClass create issued before the server is up gets
	// a 502 "failed calling webhook". Block until the webhook actually admits
	// a request so test bodies don't race it.
	if webhookEnabled {
		if err := waitForWebhookReady(ctx); err != nil {
			return fmt.Errorf("webhook did not become ready: %w", err)
		}
	}
	return nil
}

// snapshotsEnabledArg is the operator flag the chart renders under
// snapshots.enabled=true. Its presence on the operator Deployment is what
// "Phase 3 is on" means to this suite.
const snapshotsEnabledArg = "--snapshots-enabled"

// operatorDeployment reads the operator Deployment of the suite's release,
// which the chart names with `setec.fullname` (chartFullname).
func operatorDeployment(ctx context.Context) (*appsv1.Deployment, error) {
	dep := &appsv1.Deployment{}
	key := types.NamespacedName{Namespace: testNamespace, Name: chartFullname}
	if err := k8sClient.Get(ctx, key, dep); err != nil {
		return nil, fmt.Errorf("get operator Deployment %s: %w", key, err)
	}
	return dep, nil
}

// operatorHasArg reports whether the manager container of a Pod spec
// carries the flag.
func operatorHasArg(spec corev1.PodSpec, flag string) bool {
	for _, c := range spec.Containers {
		if c.Name != "manager" {
			continue
		}
		for _, a := range c.Args {
			if a == flag {
				return true
			}
		}
	}
	return false
}

// operatorRolloutStable is how long the operator must hold a complete,
// Ready, restart-free rollout before waitForOperatorRollout accepts it.
// A restart loop shows up as a restart count, not as a missing Ready
// condition, so one green sample is not enough.
const operatorRolloutStable = 30 * time.Second

// waitForOperatorRollout blocks until the operator Deployment has rolled
// out with snapshotsEnabledArg present (snapshots=true) or absent
// (snapshots=false), every replica is updated and available, and every
// operator Pod is Ready with zero container restarts, and that state has
// held for operatorRolloutStable. It returns the last unmet condition on
// timeout.
func waitForOperatorRollout(ctx context.Context, snapshots bool) error {
	var stableSince time.Time
	var last string
	for {
		unmet := operatorRolloutUnmet(ctx, snapshots)
		switch {
		case unmet != "":
			stableSince = time.Time{}
			last = unmet
		case stableSince.IsZero():
			stableSince = time.Now()
		case time.Since(stableSince) >= operatorRolloutStable:
			return nil
		}
		select {
		case <-ctx.Done():
			if last == "" {
				last = "rollout was complete but did not hold for " + operatorRolloutStable.String()
			}
			return fmt.Errorf("operator rollout (snapshots=%t): %s: %w", snapshots, last, ctx.Err())
		case <-time.After(defaultPoll):
		}
	}
}

// operatorRolloutUnmet returns the first rollout condition that does not
// hold, or "" when the rollout is complete and healthy right now.
func operatorRolloutUnmet(ctx context.Context, snapshots bool) string {
	dep, err := operatorDeployment(ctx)
	if err != nil {
		return err.Error()
	}
	if operatorHasArg(dep.Spec.Template.Spec, snapshotsEnabledArg) != snapshots {
		return fmt.Sprintf("Deployment template has %s=%t, want %t", snapshotsEnabledArg, !snapshots, snapshots)
	}
	want := int32(1)
	if dep.Spec.Replicas != nil {
		want = *dep.Spec.Replicas
	}
	st := dep.Status
	if st.ObservedGeneration < dep.Generation {
		return fmt.Sprintf("observedGeneration %d < generation %d", st.ObservedGeneration, dep.Generation)
	}
	if st.UpdatedReplicas != want || st.AvailableReplicas != want || st.Replicas != want {
		return fmt.Sprintf("replicas want=%d updated=%d available=%d total=%d", want, st.UpdatedReplicas, st.AvailableReplicas, st.Replicas)
	}

	var pods corev1.PodList
	if err := k8sClient.List(ctx, &pods, client.InNamespace(testNamespace), operatorLabels()); err != nil {
		return fmt.Sprintf("list operator pods: %v", err)
	}
	live := 0
	for _, p := range pods.Items {
		if p.DeletionTimestamp != nil {
			continue
		}
		live++
		if operatorHasArg(p.Spec, snapshotsEnabledArg) != snapshots {
			return fmt.Sprintf("pod %s still runs the previous template", p.Name)
		}
		ready := false
		for _, c := range p.Status.Conditions {
			if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
				ready = true
			}
		}
		if !ready {
			return fmt.Sprintf("pod %s is not Ready (phase %s)", p.Name, p.Status.Phase)
		}
		for _, cs := range p.Status.ContainerStatuses {
			if cs.RestartCount > 0 {
				return fmt.Sprintf("pod %s container %s restarted %d time(s)", p.Name, cs.Name, cs.RestartCount)
			}
		}
	}
	if int32(live) != want {
		return fmt.Sprintf("%d live operator pod(s), want %d", live, want)
	}
	return ""
}

// helmRun runs one helm command against the suite's release and fails the
// test on a non-zero exit. Output goes to the test log.
func helmRun(t *testing.T, args ...string) {
	t.Helper()
	cmd := exec.Command("helm", args...)
	out, err := cmd.CombinedOutput()
	t.Logf("helm %s\n%s", strings.Join(args, " "), out)
	if err != nil {
		t.Fatalf("helm %s: %v", strings.Join(args, " "), err)
	}
}

// helmRevision returns the current revision number of the suite's release.
func helmRevision(t *testing.T) int {
	t.Helper()
	out, err := exec.Command("helm", "status", helmReleaseName, "--namespace", testNamespace, "-o", "json").Output()
	if err != nil {
		t.Fatalf("helm status %s: %v", helmReleaseName, err)
	}
	var status struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(out, &status); err != nil {
		t.Fatalf("parse helm status for %s: %v", helmReleaseName, err)
	}
	if status.Version < 1 {
		t.Fatalf("helm status for %s reports revision %d", helmReleaseName, status.Version)
	}
	return status.Version
}

// installReadyTimeout bounds the post-install readiness gate. Ten minutes,
// not five: the suites job pre-warms a metal node BEFORE installing
// (TestEnv_KVMPresent has to see a kata-fc-capable node), so the
// runtime-agent has to roll out onto an m5zn.metal that came up minutes ago
// with none of the images cached. Run 31916255452 died at five.
const installReadyTimeout = 10 * time.Minute

// waitForInstallReady is the readiness gate that replaces `helm --wait`
// (setec#12), ported from the roundtrip job's "Wait for the shadow release"
// step. It asserts:
//
//   - the operator Deployment rolled out, with the snapshots flag matching
//     the install, and holds Ready with no restart (waitForOperatorRollout);
//   - every runtime-agent pod whose node the cluster reports Ready is
//     itself Ready, with at least one such pod. Pods pinned to a node the
//     cluster does not report Ready are named and excluded, out loud,
//     rather than waited on: their node is gone, not slow;
//   - the same for the node-agent DaemonSet when the install renders it.
//
// A DaemonSet the scheduler places on no node at all (desired 0, as on the
// chain-6 kind cluster, which pins the runtime-agent to an unmatched
// selector) has nothing to gate on and passes.
func waitForInstallReady(parent context.Context) error {
	ctx, cancel := context.WithTimeout(parent, installReadyTimeout)
	defer cancel()

	if err := waitForOperatorRollout(ctx, snapshotsEnabled()); err != nil {
		return err
	}
	components := []string{"runtime-agent"}
	if sessionS3.enabled {
		components = append(components, "node-agent")
	}
	for _, c := range components {
		if err := waitForAgentsOnReadyNodes(ctx, c); err != nil {
			return err
		}
	}
	return nil
}

// agentGate is one sample of a DaemonSet component's readiness on the nodes
// the cluster reports Ready.
type agentGate struct {
	ready, considered int
	excluded, pending []string
	// unscheduled is true while the DaemonSet controller has not yet
	// reported how many pods it wants; the sample proves nothing then.
	unscheduled bool
	// desired is the DaemonSet's desiredNumberScheduled once reported.
	desired int32
}

// waitForAgentsOnReadyNodes polls sampleAgentGate for the component until
// every considered pod is Ready or ctx ends.
func waitForAgentsOnReadyNodes(ctx context.Context, component string) error {
	var last string
	for {
		g, err := sampleAgentGate(ctx, component)
		switch {
		case err != nil:
			last = err.Error()
		case g.unscheduled:
			last = "DaemonSet has not reported its desired pod count yet"
		case g.desired == 0:
			fmt.Fprintf(os.Stderr, "e2e: %s DaemonSet schedules onto no node on this cluster (desired 0); nothing to gate on\n", component)
			return nil
		case g.considered >= 1 && g.ready == g.considered:
			if len(g.excluded) > 0 {
				fmt.Fprintf(os.Stderr, "e2e: %s pods on nodes the cluster does not report Ready were excluded from the readiness gate: %s\n",
					component, strings.Join(g.excluded, " "))
			}
			fmt.Fprintf(os.Stderr, "e2e: %s ready %d/%d on Ready nodes\n", component, g.ready, g.considered)
			return nil
		default:
			last = fmt.Sprintf("ready %d/%d on Ready nodes, waiting on: %s, excluded (node not Ready): %s",
				g.ready, g.considered, orNone(g.pending), orNone(g.excluded))
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s readiness gate: %s: %w", component, last, ctx.Err())
		case <-time.After(10 * time.Second):
		}
	}
}

// sampleAgentGate classifies the component's pods against the Ready nodes.
func sampleAgentGate(ctx context.Context, component string) (agentGate, error) {
	var g agentGate

	ds := &appsv1.DaemonSet{}
	dsKey := types.NamespacedName{Namespace: testNamespace, Name: chartFullname + "-" + component}
	if err := k8sClient.Get(ctx, dsKey, ds); err != nil {
		return g, fmt.Errorf("get DaemonSet %s: %w", dsKey, err)
	}
	if ds.Status.ObservedGeneration < ds.Generation {
		g.unscheduled = true
		return g, nil
	}
	g.desired = ds.Status.DesiredNumberScheduled
	if g.desired == 0 {
		return g, nil
	}

	var nodes corev1.NodeList
	if err := k8sClient.List(ctx, &nodes); err != nil {
		return g, fmt.Errorf("list nodes: %w", err)
	}
	readyNodes := map[string]bool{}
	for _, n := range nodes.Items {
		for _, c := range n.Status.Conditions {
			if c.Type == corev1.NodeReady && c.Status == corev1.ConditionTrue {
				readyNodes[n.Name] = true
			}
		}
	}

	var pods corev1.PodList
	if err := k8sClient.List(ctx, &pods, client.InNamespace(testNamespace), client.MatchingLabels{
		"app.kubernetes.io/instance":  helmReleaseName,
		"app.kubernetes.io/component": component,
	}); err != nil {
		return g, fmt.Errorf("list %s pods: %w", component, err)
	}
	for _, p := range pods.Items {
		if p.DeletionTimestamp != nil {
			continue
		}
		node := p.Spec.NodeName
		if node == "" || !readyNodes[node] {
			if node == "" {
				node = "unscheduled"
			}
			g.excluded = append(g.excluded, p.Name+"("+node+")")
			continue
		}
		g.considered++
		ready := false
		for _, c := range p.Status.Conditions {
			if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
				ready = true
			}
		}
		if ready {
			g.ready++
		} else {
			g.pending = append(g.pending, p.Name+"("+node+")")
		}
	}
	return g, nil
}

func orNone(items []string) string {
	if len(items) == 0 {
		return "none"
	}
	return strings.Join(items, " ")
}

// dumpInstallFailureState prints the state of the half-installed release to
// stderr, so an install failure says which workload did not come up and why.
//
// Best-effort throughout: this runs on a path that is already failing, and a
// diagnostic that can itself fail the run is worse than no diagnostic.
func dumpInstallFailureState() {
	for _, args := range [][]string{
		{"get", "pods", "-n", testNamespace, "-o", "wide"},
		{"get", "daemonset,deployment", "-n", testNamespace, "-o", "wide"},
		// Why a pod is not ready is almost always in its events (ImagePullBackOff,
		// FailedScheduling, a failing probe) rather than in its status.
		{"get", "events", "-n", testNamespace, "--sort-by=.lastTimestamp"},
		{"describe", "pods", "-n", testNamespace},
		// The install renders the baseline NetworkPolicy and the host guard
		// into the Sandbox namespace; a refused render names them.
		{"get", "networkpolicy,validatingadmissionpolicybinding", "-n", sandboxNamespace},
	} {
		fmt.Fprintf(os.Stderr, "\ne2e: --- kubectl %s ---\n", strings.Join(args, " "))
		cmd := exec.Command("kubectl", args...)
		cmd.Stdout = os.Stderr
		cmd.Stderr = os.Stderr
		_ = cmd.Run()
	}
}

// snapshotsEnabled reports whether this install turns the whole snapshots
// subtree on — the operator's `--snapshots-enabled`, its node-agent dialer,
// and the three mTLS Secret mounts on both workloads.
//
// TWO axes reach it and they must not disagree, which is why this is one
// function rather than two call sites:
//
//   - SETEC_E2E_SNAPSHOTS=1 — local snapshot storage, what the Phase 3
//     scenarios and TestGate_UnverifiedWarmStartFailsClosed need in order to
//     run at all (phase3Enabled() greps the operator Deployment for
//     `--snapshots-enabled`). Left off, they all t.Skip and the run reports
//     PASS having verified none of them.
//   - SETEC_E2E_S3=1 — the session-checkpoint store (setec#194/#296). The
//     checkpoint backend lives in the node-agent, and the node-agent is
//     itself gated behind snapshots.enabled, so this axis implies the other.
//
// Whichever axis is on, installChart mints the mTLS trio first
// (createNodeAgentMTLSSecrets); the two used to configure that differently
// and the S3 path silently won.
func snapshotsEnabled() bool {
	return os.Getenv("SETEC_E2E_SNAPSHOTS") == "1" || sessionS3.enabled
}

// crdInstallArgs decides whether this install may touch the setec CRDs, and
// returns the helm flags that express the decision.
//
// # Why this exists (setec#298)
//
// Helm 4 applies chart CRDs SERVER-SIDE by default (`--server-side` defaults
// to true; `helm install --help` still claims CRDs are only installed "if not
// already present", but the apply happens regardless). The setec CRDs are
// CLUSTER-scoped and, on the shared staging cluster, are owned by
// `argocd-controller` — the live release is GitOps-managed. A throwaway
// shadow install therefore tries to take ownership of `.spec.versions` from
// Argo and the API server refuses:
//
//	INSTALLATION FAILED: failed to install CRD
//	setec.zeroroot.ai_sandboxclasses.yaml: conflict occurred while applying
//	object ... Apply failed with 1 conflict: conflict with "argocd-controller":
//	.spec.versions
//
// This is not recoverable from inside the suite and must not be forced:
// `--force-conflicts` would overwrite the LIVE staging CRD schema with
// whatever this PR happens to carry, on a cluster the suite does not own.
//
// So: if the CRDs are already on the cluster, leave them alone. If they are
// not (a throwaway kind cluster, `make e2e` on a fresh box), install them —
// that path has no other owner to conflict with, and skipping there would
// leave the suite with no CRDs at all.
//
// The cost of skipping is stated out loud rather than left implicit: a run
// that skips CRDs exercises the CLUSTER's CRD schema, not this PR's. A PR
// that changes an API type is not covered by such a run. That caveat was
// already true and already documented in .github/workflows/e2e.yml; what was
// missing is anything that says so at the point it applies.
func crdInstallArgs() []string {
	out, err := exec.Command("kubectl", "get", "crd",
		"-o", `jsonpath={range .items[*]}{.metadata.name}{"\n"}{end}`).Output()
	if err != nil {
		// Cannot tell. Let helm try: on a cluster with no setec CRDs that is
		// the correct behaviour anyway, and on one that has them the helm
		// error above is more legible than a guess made here.
		fmt.Fprintf(os.Stderr, "e2e: could not list CRDs to determine ownership (%v); letting helm install them\n", err)
		return nil
	}

	var present []string
	for _, name := range strings.Split(string(out), "\n") {
		if strings.HasSuffix(strings.TrimSpace(name), ".setec.zeroroot.ai") {
			present = append(present, strings.TrimSpace(name))
		}
	}
	if len(present) == 0 {
		fmt.Fprintln(os.Stderr, "e2e: no setec CRDs on this cluster — installing the chart's own")
		return nil
	}

	fmt.Fprintf(os.Stderr,
		"e2e: WARNING — %d setec CRD(s) already exist on this cluster and are owned by another "+
			"manager (%s). Installing with --skip-crds so this throwaway release does not fight "+
			"the owner for .spec.versions.\n"+
			"e2e: WARNING — this run therefore exercises the CLUSTER's CRD schema, NOT the chart's. "+
			"If this change touches an API type, that part of it is NOT covered by this run.\n",
		len(present), crdFieldManagers(present[0]))
	return []string{"--skip-crds"}
}

// crdFieldManagers reports the server-side-apply field managers on a CRD, so
// the skip-CRDs warning can name who actually owns the object rather than
// asserting "Argo" and being wrong on some other cluster.
func crdFieldManagers(name string) string {
	out, err := exec.Command("kubectl", "get", "crd", name,
		"-o", `jsonpath={range .metadata.managedFields[*]}{.manager}{" "}{end}`).Output()
	if err != nil {
		return "unknown"
	}
	if m := strings.TrimSpace(string(out)); m != "" {
		return m
	}
	return "unknown"
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

	// Delete both namespaces (ignore not-found; helm uninstall removes
	// neither). The Sandbox namespace goes first so its Pods are gone before
	// the operator that reaps them is.
	for _, ns := range []string{sandboxNamespace, testNamespace} {
		delCmd := exec.Command("kubectl", "delete", "namespace", ns, "--wait=true", "--ignore-not-found=true", "--timeout=2m")
		delCmd.Stdout = os.Stdout
		delCmd.Stderr = os.Stderr
		if err := delCmd.Run(); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("delete namespace %s: %w", ns, err)
		}
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
