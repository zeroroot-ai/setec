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

// Command manager is the Setec operator entrypoint. It wires the
// controller-runtime Manager, registers the SandboxReconciler, runs the
// cluster-prerequisite checker once at startup (logging warnings rather than
// failing), and exposes /healthz and /readyz endpoints. The /readyz body is a
// JSON document whose `kata_runtime_available` field reflects the prereq
// result so operators can observe cluster misconfiguration without parsing
// events.
//
// No cloud-vendor SDKs are linked into this binary by design: Setec is a
// single-tenant, vendor-neutral operator and its distroless image is expected
// to stay small and auditable.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"net/http"
	"os"
	"sync/atomic"
	"time"

	"github.com/spf13/pflag"
	grpccreds "google.golang.org/grpc/credentials"
	nodev1 "k8s.io/api/node/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	webhookserver "sigs.k8s.io/controller-runtime/pkg/webhook"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
	"github.com/zeroroot-ai/setec/internal/class"
	"github.com/zeroroot-ai/setec/internal/controller"
	"github.com/zeroroot-ai/setec/internal/credentials"
	"github.com/zeroroot-ai/setec/internal/metrics"
	"github.com/zeroroot-ai/setec/internal/netpol"
	"github.com/zeroroot-ai/setec/internal/prereq"
	runtimepkg "github.com/zeroroot-ai/setec/internal/runtime"
	"github.com/zeroroot-ai/setec/internal/snapshot"
	"github.com/zeroroot-ai/setec/internal/tracing"
	"github.com/zeroroot-ai/setec/internal/webhook"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

// prereqCheckTimeout bounds the one-shot startup prerequisite check. The
// check runs in a goroutine and its outcome feeds /readyz; a hung API server
// must not leave /readyz reporting a stale unknown state forever.
const prereqCheckTimeout = 30 * time.Second

func init() {
	// clientgoscheme already registers node/v1, but we register it
	// explicitly so the intent of this binary's scheme is obvious and
	// survives any future change in client-go's default registrations.
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(nodev1.AddToScheme(scheme))
	utilruntime.Must(setecv1alpha1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

// readyzState holds the most recent prereq result. The startup goroutine
// writes it exactly once; the /readyz handler reads it on every request. An
// atomic pointer avoids locks in the hot path and lets the handler
// distinguish "check still running" (nil) from "check complete" (non-nil).
type readyzState struct {
	result atomic.Pointer[prereq.CheckResult]
}

// readyzBody is the JSON shape written to /readyz. Field names match
// Requirement 5.3 (`kata_runtime_available`) and are snake_case to align
// with Kubernetes and Prometheus conventions. Consumers MUST tolerate
// unknown fields because this schema may grow.
type readyzBody struct {
	KataRuntimeAvailable bool     `json:"kata_runtime_available"`
	KataCapableNodes     bool     `json:"kata_capable_nodes"`
	PrereqCheckComplete  bool     `json:"prereq_check_complete"`
	Warnings             []string `json:"warnings,omitempty"`
}

// defaultReservedCIDRs is the address space Sandboxes are denied out of
// the box. It covers private (RFC1918), link-local — which includes the
// cloud instance-metadata address — carrier-grade NAT, loopback and
// multicast ranges.
//
// It deliberately does NOT include this cluster's Service or Pod CIDRs,
// because those vary per cluster and are usually carved out of RFC1918
// anyway. A cluster whose Service CIDR sits outside these ranges must add
// it explicitly via --reserved-cidrs.
func defaultReservedCIDRs() []string {
	return []string{
		"10.0.0.0/8",
		"172.16.0.0/12",
		"192.168.0.0/16",
		"169.254.0.0/16",
		"100.64.0.0/10",
		"127.0.0.0/8",
		"224.0.0.0/4",
	}
}

// nolint:gocyclo
func main() {
	var (
		metricsBindAddr     string
		probeBindAddr       string
		enableLeaderElect   bool
		runtimeClassName    string
		runtimesConfig      string
		nodeSelectorLabel   string
		multiTenancyEnabled bool
		tenantLabelKey      string
		otlpEndpoint        string
		otlpInsecure        bool
		otlpCAFile          string
		webhookEnabled      bool
		webhookCertDir      string

		// Sandbox egress posture. Both lists are validated at startup;
		// there is no runtime path that degrades to unrestricted egress.
		reservedCIDRs    []string
		nsBaselineDeny   bool
		sandboxResolvers []string
		egressHostTTL    time.Duration
		egressHostGrace  time.Duration

		// Phase 3 flags. Zero values preserve Phase 1/2 behaviour.
		snapshotsEnabled  bool
		nodeAgentEndpoint string
		nodeAgentTLSCert  string
		nodeAgentTLSKey   string
		nodeAgentTLSCA    string
		kataSocketPattern string
	)

	pflag.StringVar(&metricsBindAddr, "metrics-bind-address", ":8080",
		"The address the metrics endpoint binds to. Use 0 to disable.")
	pflag.StringVar(&probeBindAddr, "health-probe-bind-address", ":8081",
		"The address the probe endpoint binds to. Serves /healthz and /readyz.")
	pflag.BoolVar(&enableLeaderElect, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	pflag.StringVar(&runtimesConfig, "runtimes-config", "",
		"Path to a YAML file describing enabled runtime backends (runtimes block). "+
			"When set, --runtime-class-name is ignored. See charts/setec/templates/configmap-runtimes.yaml for the schema.")
	pflag.StringVar(&runtimeClassName, "runtime-class-name", "kata-fc",
		"Deprecated: use --runtimes-config instead. Name of the Kata RuntimeClass the Sandbox Pods will reference. "+
			"When --runtimes-config is absent, this flag synthesizes a kata-fc-only RuntimeConfig.")
	pflag.StringVar(&nodeSelectorLabel, "node-selector-label", "katacontainers.io/kata-runtime",
		"Label key Nodes must carry to be considered Kata-capable. "+
			"Used by the startup prerequisite check only; scheduling uses the RuntimeClass.")
	// Phase 2 flags. Zero values reproduce Phase 1 behaviour exactly.
	pflag.BoolVar(&multiTenancyEnabled, "multi-tenancy-enabled", false,
		"Require Sandboxes' namespaces to carry the tenant label.")
	pflag.StringVar(&tenantLabelKey, "tenant-label-key", "setec.zeroroot.ai/tenant",
		"Namespace label key consulted when multi-tenancy is enabled.")
	pflag.StringVar(&otlpEndpoint, "otlp-endpoint", "",
		"OTLP/gRPC collector endpoint for trace export. Empty disables tracing.")
	pflag.BoolVar(&otlpInsecure, "otel-insecure", false,
		"DANGEROUS — export OTLP traces in plaintext. Set only in dev clusters; the operator logs a loud warning at startup.")
	pflag.StringVar(&otlpCAFile, "otel-ca-file", "",
		"Optional path to a PEM CA bundle used to verify the OTLP collector. Empty uses system roots.")
	pflag.BoolVar(&webhookEnabled, "webhook-enabled", false,
		"Register the validating admission webhook with the manager.")
	pflag.StringVar(&webhookCertDir, "webhook-cert-dir", "/tmp/k8s-webhook-server/serving-certs",
		"Directory containing tls.crt and tls.key for the webhook server.")

	// Sandbox egress posture.
	pflag.StringSliceVar(&reservedCIDRs, "reserved-cidrs", defaultReservedCIDRs(),
		"Address ranges no Sandbox may reach. Subtracted from every permissive egress rule via "+
			"ipBlock.except. Add this cluster's Service and Pod CIDRs. Self-hosted operators whose "+
			"authorised scan scope is private address space must narrow this list to their own "+
			"control-plane ranges instead of clearing it. May not be empty.")
	pflag.StringSliceVar(&sandboxResolvers, "sandbox-resolvers", []string{"1.1.1.1", "8.8.8.8"},
		"DNS servers Sandboxes resolve through. Written into each Sandbox Pod's dnsConfig and into "+
			"the generated NetworkPolicy's port-53 rule, so Sandboxes never query cluster DNS and "+
			"cannot enumerate in-cluster Services by name. May not be empty.")

	pflag.DurationVar(&egressHostTTL, "egress-host-resolve-ttl", netpol.DefaultResolveTTL,
		"How long a resolved egress-allow-list host is reused before it is looked up again. This "+
			"is also the reconcile cadence for a Sandbox whose policy names hosts, and therefore "+
			"the worst-case lag between a permitted destination changing address and the "+
			"NetworkPolicy following it.")
	pflag.DurationVar(&egressHostGrace, "egress-host-resolve-grace", netpol.DefaultResolveGrace,
		"How long the last successful answer for an egress-allow-list host keeps being used after "+
			"lookups start failing. Past this window the entry is dropped from the policy rather "+
			"than widened, so a sustained resolver outage costs egress rather than containment. "+
			"Set to a very small value only if you would rather lose egress immediately than run "+
			"on a stale address.")

	pflag.BoolVar(&nsBaselineDeny, "namespace-baseline-deny", true,
		"Ensure a namespace-wide default-deny NetworkPolicy (podSelector: {}) in the namespace of "+
			"every reconciled Sandbox, before its own policy and its Pod. The per-Sandbox policies "+
			"select on the setec.zeroroot.ai/sandbox label and therefore confine only Pods this "+
			"operator built; a Pod created in the same namespace by any other route is selected by "+
			"no policy and is unrestricted. This is what removes that state. Disable only where "+
			"Sandbox namespaces are shared with workloads that need ordinary egress.")

	// Phase 3 flags.
	pflag.BoolVar(&snapshotsEnabled, "snapshots-enabled", false,
		"Phase 3 kill-switch: register the Snapshot CRD controller and wire snapshot.Coordinator"+
			" for the Sandbox reconciler. Default false preserves Phase 2 behaviour.")
	pflag.StringVar(&nodeAgentEndpoint, "nodeagent-endpoint-pattern",
		"%s.setec-node-agent.setec-system.svc:50052",
		"Phase 3: format string that renders a dial target from a node name. %s is substituted with Pod.Spec.NodeName.")
	pflag.StringVar(&nodeAgentTLSCert, "nodeagent-tls-cert", "",
		"Phase 3: path to the operator's client certificate for mTLS to node-agents.")
	pflag.StringVar(&nodeAgentTLSKey, "nodeagent-tls-key", "",
		"Phase 3: path to the operator's client private key.")
	pflag.StringVar(&nodeAgentTLSCA, "nodeagent-ca", "",
		"Phase 3: path to the CA used to verify node-agent server certificates. Required when --snapshots-enabled.")
	pflag.StringVar(&kataSocketPattern, "kata-socket-pattern",
		"/run/kata-containers/%s/firecracker.socket",
		"Phase 3: format string used by the Coordinator to render a Firecracker socket path from a Pod UID.")

	// Controller-runtime's zap helper registers its flags on the stdlib
	// flag.CommandLine. We bridge the stdlib set into pflag so --help
	// lists every flag together and the standard --zap-* options keep
	// working exactly as documented upstream.
	zapOpts := zap.Options{Development: false}
	zapOpts.BindFlags(flag.CommandLine)
	pflag.CommandLine.AddGoFlagSet(flag.CommandLine)
	pflag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zapOpts)))

	// --- Runtime backend configuration ---
	//
	// When --runtimes-config is provided, load the full multi-backend config.
	// When only the legacy --runtime-class-name is set (no --runtimes-config),
	// synthesize a minimal config that enables only kata-fc. This preserves
	// full backward compatibility for existing deployments (REQ-6.1).
	var (
		runtimeCfg      *runtimepkg.RuntimeConfig
		runtimeRegistry *runtimepkg.Registry
	)
	if runtimesConfig != "" {
		var err error
		runtimeCfg, err = runtimepkg.LoadFromFile(runtimesConfig)
		if err != nil {
			setupLog.Error(err, "unable to load runtimes config", "path", runtimesConfig)
			os.Exit(1)
		}
	} else {
		// Legacy path: synthesize a kata-fc-only config from --runtime-class-name.
		setupLog.Info("DEPRECATION WARNING: --runtimes-config is not set; "+
			"falling back to legacy --runtime-class-name flag. "+
			"Please migrate to --runtimes-config for multi-backend support.",
			"runtime-class-name", runtimeClassName,
		)
		runtimeCfg = &runtimepkg.RuntimeConfig{
			Runtimes: map[string]runtimepkg.BackendConfig{
				runtimepkg.BackendKataFC: {
					Enabled:          true,
					RuntimeClassName: runtimeClassName,
				},
			},
			Defaults: runtimepkg.DefaultsConfig{
				Runtime: runtimepkg.RuntimeDefaults{
					Backend: runtimepkg.BackendKataFC,
				},
			},
		}
	}

	// --- Sandbox egress posture ---
	//
	// Validated before the manager starts. An empty or unparseable list is
	// a startup failure rather than a per-reconcile error: a running
	// operator that cannot express the reserved ranges would emit
	// permissive policies, which is the outcome this configuration exists
	// to prevent.
	// The resolver is what makes an egress-allow-list entry name a
	// destination rather than a port (setec#130). It is always wired: with
	// no resolver, a host that is not a literal address cannot be turned
	// into an ipBlock at all, and the generator drops the entry.
	netpolCfg := netpol.Config{
		ReservedCIDRs: reservedCIDRs,
		ResolverIPs:   sandboxResolvers,
		Resolver: netpol.NewCachingResolver(netpol.ResolverOptions{
			TTL:   egressHostTTL,
			Grace: egressHostGrace,
		}),
		RefreshInterval: egressHostTTL,
	}
	if err := netpolCfg.Validate(); err != nil {
		setupLog.Error(err, "invalid sandbox egress configuration; "+
			"check --reserved-cidrs and --sandbox-resolvers")
		os.Exit(1)
	}
	setupLog.Info("sandbox egress posture configured",
		"reservedCIDRs", netpolCfg.ReservedCIDRs,
		"resolvers", netpolCfg.ResolverIPs,
		"hostResolveTTL", egressHostTTL,
		"hostResolveGrace", egressHostGrace,
	)

	// Build the dispatcher Registry and register one Dispatcher per enabled backend.
	runtimeRegistry = runtimepkg.NewRegistry()
	for _, backend := range runtimeCfg.EnabledBackends() {
		bc := runtimeCfg.Runtimes[backend]
		switch backend {
		case runtimepkg.BackendKataFC:
			runtimeRegistry.Register(runtimepkg.NewKataFCDispatcher(bc))
		case runtimepkg.BackendKataQEMU:
			runtimeRegistry.Register(runtimepkg.NewKataQEMUDispatcher(bc))
		case runtimepkg.BackendGVisor:
			runtimeRegistry.Register(runtimepkg.NewGVisorDispatcher(bc))
		case runtimepkg.BackendRunc:
			runtimeRegistry.Register(runtimepkg.NewRuncDispatcher(bc))
		default:
			setupLog.Info("unknown backend in runtimes config; skipping", "backend", backend)
		}
	}
	setupLog.Info("runtime registry built", "enabled", runtimeRegistry.EnabledBackends())

	restCfg := ctrl.GetConfigOrDie()

	// The Manager's built-in health probe server is intentionally left
	// disabled (empty HealthProbeBindAddress) because we register our own
	// HTTP server below — the /readyz body must contain structured JSON,
	// which controller-runtime's default handler does not support.
	mgrOpts := ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress: metricsBindAddr,
		},
		LeaderElection:                enableLeaderElect,
		LeaderElectionID:              "setec.zeroroot.ai",
		LeaderElectionReleaseOnCancel: true,
	}
	if webhookEnabled {
		mgrOpts.WebhookServer = webhookserver.NewServer(webhookserver.Options{
			Port:    9443,
			CertDir: webhookCertDir,
		})
	}
	mgr, err := ctrl.NewManager(restCfg, mgrOpts)
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	// Phase 2: init tracing (no-op when otlpEndpoint is empty).
	tracer, tracerShutdown, err := tracing.Setup(tracing.Config{
		Endpoint: otlpEndpoint,
		Insecure: otlpInsecure,
		CAFile:   otlpCAFile,
	})
	if err != nil {
		setupLog.Error(err, "unable to initialise tracing")
		os.Exit(1)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tracerShutdown(ctx)
	}()

	// Phase 2: register Prometheus collectors with controller-runtime's
	// shared metrics registry.
	collectors := metrics.NewCollectors()

	// Phase 2: resolver reads SandboxClass objects.
	resolver := class.NewResolver(mgr.GetClient())

	// Phase 3: optionally construct the snapshot.Coordinator. Kept
	// entirely behind --snapshots-enabled so default installs remain
	// Phase 2-equivalent.
	var coordinator *snapshot.Coordinator
	if snapshotsEnabled {
		creds, err := nodeAgentClientCredentials(context.Background(), nodeAgentCredentialFlags{
			certPath: nodeAgentTLSCert,
			keyPath:  nodeAgentTLSKey,
			caPath:   nodeAgentTLSCA,
		})
		if err != nil {
			setupLog.Error(err, "unable to load node-agent client credentials")
			os.Exit(1)
		}
		dialer := snapshot.NewGRPCDialer(nodeAgentEndpoint, creds)
		snapshotCoordRecorder := mgr.GetEventRecorder("snapshot-coordinator")
		coordinator = &snapshot.Coordinator{
			Client:            mgr.GetClient(),
			Dialer:            dialer,
			Recorder:          snapshotCoordRecorder,
			Metrics:           collectors,
			Tracer:            tracer,
			KataSocketPattern: kataSocketPattern,
		}
	}

	sandboxRecorder := mgr.GetEventRecorder("sandbox-controller")
	if err := (&controller.SandboxReconciler{
		Client:                mgr.GetClient(),
		Scheme:                mgr.GetScheme(),
		Recorder:              sandboxRecorder,
		NodeSelectorLabel:     nodeSelectorLabel,
		Runtimes:              runtimeRegistry,
		RuntimeCfg:            runtimeCfg,
		ClassResolver:         resolver,
		MetricsCollector:      collectors,
		Tracer:                tracer,
		MultiTenancyEnabled:   multiTenancyEnabled,
		TenantLabelKey:        tenantLabelKey,
		Coordinator:           coordinator,
		NetPol:                netpolCfg,
		NamespaceBaselineDeny: nsBaselineDeny,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to set up SandboxReconciler")
		os.Exit(1)
	}

	// Phase 3: register the SnapshotReconciler when enabled.
	if snapshotsEnabled {
		snapshotCtrlRecorder := mgr.GetEventRecorder("snapshot-controller")
		if err := (&controller.SnapshotReconciler{
			Client:      mgr.GetClient(),
			Scheme:      mgr.GetScheme(),
			Recorder:    snapshotCtrlRecorder,
			Coordinator: coordinator,
		}).SetupWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to set up SnapshotReconciler")
			os.Exit(1)
		}
	}

	// Phase 2: SandboxClass controller (trivial watch).
	if err := (&controller.SandboxClassReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to set up SandboxClassReconciler")
		os.Exit(1)
	}

	// Phase 2: register the validating webhook when enabled.
	if webhookEnabled {
		validator := &webhook.SandboxValidator{
			Resolver:            resolver,
			MultiTenancyEnabled: multiTenancyEnabled,
			TenantLabelKey:      tenantLabelKey,
			NamespaceGetter:     &webhook.ClientNamespaceGetter{Client: mgr.GetClient()},
			Client:              mgr.GetClient(),
		}
		if err := validator.SetupWebhookWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to set up webhook")
			os.Exit(1)
		}
		if snapshotsEnabled {
			snapVal := &webhook.SnapshotValidator{Client: mgr.GetClient()}
			if err := snapVal.SetupWebhookWithManager(mgr); err != nil {
				setupLog.Error(err, "unable to set up snapshot webhook")
				os.Exit(1)
			}
		}
		// Runtime backends: SandboxClass defaulting + validating webhook.
		// RuntimeCfg is guaranteed non-nil here — the operator would have
		// exited above if LoadFromFile or the synthetic config failed.
		scWebhook := &webhook.SandboxClassWebhook{
			Client:     mgr.GetClient(),
			RuntimeCfg: runtimeCfg,
		}
		if err := scWebhook.SetupWebhookWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to set up SandboxClass webhook")
			os.Exit(1)
		}
	}
	// +kubebuilder:scaffold:builder

	// Run the prerequisite check once at startup in a goroutine so a slow
	// API server never delays manager startup (and therefore /healthz
	// readiness). The check logs warnings for each missing prerequisite
	// and never errors; missing prerequisites are cluster-configuration
	// issues, not operator failures.
	state := &readyzState{}
	go runStartupPrereqCheck(restCfg, runtimeCfg, nodeSelectorLabel, state)

	// Serve /healthz and /readyz on the probe bind address as a
	// manager-managed Runnable so the listener shares the manager's
	// context and gets a graceful shutdown on SIGTERM.
	if err := mgr.Add(newProbeServer(probeBindAddr, state)); err != nil {
		setupLog.Error(err, "unable to register health probe server")
		os.Exit(1)
	}

	setupLog.Info("starting manager",
		"metrics-bind-address", metricsBindAddr,
		"health-probe-bind-address", probeBindAddr,
		"leader-elect", enableLeaderElect,
		"runtimes-config", runtimesConfig,
		"enabled-backends", runtimeRegistry.EnabledBackends(),
		"node-selector-label", nodeSelectorLabel,
	)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "manager exited with error")
		os.Exit(1)
	}
}

// nodeAgentCredentialFlags carries the credential-related flag values
// the operator parsed for its client hop to the node-agents. It exists
// so the translation from flags to a credential source happens in
// exactly one place, and so a test can exercise that translation
// without the process exiting.
type nodeAgentCredentialFlags struct {
	certPath string
	keyPath  string
	caPath   string
}

// credentialConfig translates the parsed flags into the credential
// module's configuration, or reports why this component must not
// start. It is the client-side twin of the check cmd/frontend and
// cmd/node-agent make on their server surfaces: same shape, same
// refusal to start on a half-configured credential.
func (f nodeAgentCredentialFlags) credentialConfig() (credentials.Config, error) {
	if f.certPath == "" || f.keyPath == "" || f.caPath == "" {
		return credentials.Config{}, errors.New(
			"--nodeagent-tls-cert, --nodeagent-tls-key and --nodeagent-ca are required; mTLS is mandatory")
	}
	return credentials.Config{
		Files: &credentials.FileSource{
			CertFile: f.certPath,
			KeyFile:  f.keyPath,
			CAFile:   f.caPath,
		},
	}, nil
}

// nodeAgentClientCredentials resolves the transport credentials the
// snapshot dialer presents to node-agents. The operator is the client
// on this hop, so what it must get right is whose node-agent it is
// willing to talk to — the credential module owns that decision, and
// this function only names the surface it needs.
func nodeAgentClientCredentials(ctx context.Context, f nodeAgentCredentialFlags) (grpccreds.TransportCredentials, error) {
	cfg, err := f.credentialConfig()
	if err != nil {
		return nil, err
	}
	provider, err := credentials.New(cfg)
	if err != nil {
		return nil, err
	}
	return provider.ClientCredentials(ctx)
}

// runStartupPrereqCheck performs the one-shot cluster prerequisite check and
// stores the result in state for /readyz to report. It logs a warning for
// each missing prerequisite and never propagates errors — a missing
// RuntimeClass or an unreachable API server must not prevent Setec from
// starting, because the operator's role at that point is to surface the
// problem to the cluster administrator via Events, not to crash-loop.
func runStartupPrereqCheck(
	cfg *rest.Config,
	runtimeCfg *runtimepkg.RuntimeConfig,
	nodeSelectorLabel string,
	state *readyzState,
) {
	ctx, cancel := context.WithTimeout(context.Background(), prereqCheckTimeout)
	defer cancel()

	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		setupLog.Info("startup prerequisite check skipped: unable to build API client",
			"error", err.Error(),
		)
		// Store an empty result so /readyz transitions out of the
		// "check pending" state — the operator is healthy; the check
		// simply could not run.
		state.result.Store(&prereq.CheckResult{})
		return
	}

	// Build the per-backend class-name map for the multi-backend prereq check.
	classNames := make(map[string]string, len(runtimeCfg.Runtimes))
	for name, bc := range runtimeCfg.Runtimes {
		if bc.Enabled {
			classNames[name] = bc.RuntimeClassName
		}
	}

	result, err := prereq.CheckMulti(ctx, c, runtimeCfg.EnabledBackends(), classNames, nodeSelectorLabel)
	if err != nil {
		setupLog.Info("startup prerequisite check encountered an API error",
			"error", err.Error(),
		)
		// Still store a result so /readyz reports `prereq_check_complete:true`
		// and consumers see kata_runtime_available:false.
		state.result.Store(&result)
		return
	}

	for _, w := range result.Warnings {
		setupLog.Info("prerequisite warning", "warning", w)
	}

	state.result.Store(&result)
}

// newProbeServer returns a manager.Runnable that serves /healthz and /readyz
// on addr. /healthz is an unconditional 200 (the process is up). /readyz is a
// 200 carrying the JSON-encoded readyzBody so operators and probes can see
// the prereq-check outcome without parsing Events. Separating the probe
// server from controller-runtime's built-in handler is what allows the
// structured body; the built-in handler supports only plain-text verbose
// output.
func newProbeServer(addr string, state *readyzState) manager.Runnable {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		body := readyzBody{}
		if r := state.result.Load(); r != nil {
			body.KataRuntimeAvailable = r.RuntimeClassPresent
			body.KataCapableNodes = r.KataCapableNodes
			body.PrereqCheckComplete = true
			body.Warnings = r.Warnings
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(body)
	})

	return manager.RunnableFunc(func(ctx context.Context) error {
		srv := &http.Server{
			Addr:              addr,
			Handler:           mux,
			ReadHeaderTimeout: 5 * time.Second,
		}

		// Shut the server down gracefully when the manager's context
		// is cancelled (SIGTERM). A short shutdown timeout keeps the
		// Pod's terminationGracePeriodSeconds budget intact.
		shutdownDone := make(chan struct{})
		go func() {
			<-ctx.Done()
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = srv.Shutdown(shutdownCtx)
			close(shutdownDone)
		}()

		setupLog.Info("starting health probe server", "address", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		<-shutdownDone
		return nil
	})
}
