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

// Command node-agent is the Setec node-level infrastructure daemon. It
// provisions and monitors the devicemapper thin-pool used by Kata
// Containers, optionally prefetches SandboxClass-referenced OCI images
// into the local containerd store, and exposes a /metrics HTTP endpoint
// for Prometheus scraping.
//
// The binary is deliberately minimal: all business logic lives in
// internal/nodeagent and is unit-tested without any real system calls.
// main.go handles flag parsing, signal handling, and the periodic
// sampler loop.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"google.golang.org/grpc"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	setecgrpcv1 "github.com/zeroroot-ai/setec/api/grpc/v1"
	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
	"github.com/zeroroot-ai/setec/internal/credentials"
	"github.com/zeroroot-ai/setec/internal/entropy"
	"github.com/zeroroot-ai/setec/internal/firecracker"
	"github.com/zeroroot-ai/setec/internal/nodeagent"
	"github.com/zeroroot-ai/setec/internal/nodeagent/grpcserver"
	"github.com/zeroroot-ai/setec/internal/nodeagent/pool"
	"github.com/zeroroot-ai/setec/internal/nodeagent/reaper"
	"github.com/zeroroot-ai/setec/internal/snapshot/storage"
)

const (
	// sampleInterval controls how often the agent polls thin-pool
	// status. 30s matches the cadence documented in the Helm chart
	// README and is fast enough to catch ENOSPC races without
	// flooding the event stream.
	sampleInterval = 30 * time.Second

	// kvmDevicePath is the Linux device node KVM-capable nodes expose.
	// Absence of this file means the node cannot host Sandboxes.
	kvmDevicePath = "/dev/kvm"
)

func main() {
	var (
		poolName            string
		dataDev             string
		metaDev             string
		fillThreshold       int
		metricsAddr         string
		containerdSocket    string
		containerdNamespace string
		containerdAuthFile  string
		nodeName            string
		prefetchImages      string

		// Phase 3 flags.
		grpcListenAddr       string
		creds                credentialFlags
		snapshotBackend      string
		snapshotRoot         string
		snapshotFillFraction float64
		kataSocketPattern    string
		poolReconcileTick    time.Duration
		orphanReapTick       time.Duration
		entropyReseedMode    string
	)
	flag.StringVar(&poolName, "thinpool-name", "setec-thinpool",
		"Name of the devicemapper thin-pool to manage.")
	flag.StringVar(&dataDev, "thinpool-data-device", "",
		"Block device for the thin-pool data volume (e.g. /dev/vdb). Required for Ensure.")
	flag.StringVar(&metaDev, "thinpool-metadata-device", "",
		"Block device for the thin-pool metadata volume (e.g. /dev/vdc). Required for Ensure.")
	flag.IntVar(&fillThreshold, "fill-threshold", 80,
		"Percent fill (0..100) above which the thin-pool is reported degraded.")
	flag.StringVar(&metricsAddr, "metrics-addr", ":9090",
		"Listen address for the Prometheus /metrics endpoint.")
	flag.StringVar(&nodeName, "node-name", os.Getenv("NODE_NAME"),
		"Name of the Kubernetes Node this agent runs on (defaults to $NODE_NAME).")
	flag.StringVar(&prefetchImages, "prefetch-images", "",
		"Space-separated OCI references to prefetch into the containerd content store.")
	flag.StringVar(&containerdSocket, "containerd-socket", "/run/containerd/containerd.sock",
		"Path to the containerd Unix socket used by the image puller.")
	flag.StringVar(&containerdNamespace, "containerd-namespace", "k8s.io",
		"Containerd namespace pulled images are placed into. Defaults to the one kubelet uses.")
	flag.StringVar(&containerdAuthFile, "containerd-auth-file", "",
		"Path to a Docker config.json used as the source of registry credentials. Empty means anonymous access.")

	// Phase 3 flags.
	flag.StringVar(&grpcListenAddr, "grpc-listen-addr", ":50052",
		"Phase 3: address the NodeAgentService gRPC server listens on. Empty disables the server.")
	flag.StringVar(&creds.tlsCert, "tls-cert", "",
		"Phase 3: path to the PEM-encoded server certificate for mTLS. "+
			"Selects file credential mode, the default.")
	flag.StringVar(&creds.tlsKey, "tls-key", "",
		"Phase 3: path to the PEM-encoded server private key. "+
			"Selects file credential mode, the default.")
	flag.StringVar(&creds.tlsClientCA, "tls-client-ca", "",
		"Phase 3: path to the PEM-encoded CA used to verify operator client certificates. "+
			"Selects file credential mode, the default.")
	flag.StringVar(&creds.spiffeSocket, "spiffe-socket", "",
		"SPIFFE Workload API socket, e.g. unix:///run/spire/agent-sockets/api.sock. "+
			"Selects SPIFFE credential mode; mutually exclusive with the --tls-* flags.")
	flag.Var(&creds.spiffeAuthorizedIDs, "spiffe-authorized-id",
		"Full SPIFFE ID allowed to call this node-agent, e.g. spiffe://zeroroot.ai/ns/setec/sa/setec. "+
			"Repeat for each caller. Required in SPIFFE mode; there is no accept-everyone setting.")
	flag.StringVar(&snapshotBackend, "snapshot-backend", "local-disk",
		"Phase 3: storage backend identifier. Only local-disk is supported in Phase 3.")
	flag.StringVar(&snapshotRoot, "snapshot-root", "/var/lib/setec/snapshots",
		"Phase 3: root directory for persisted snapshot state files.")
	flag.Float64Var(&snapshotFillFraction, "snapshot-fill-threshold", 0.85,
		"Phase 3: refuse new snapshots when the snapshot-root filesystem's used fraction exceeds this value.")
	flag.StringVar(&kataSocketPattern, "kata-socket-pattern", "/run/kata-containers/%s/firecracker.socket",
		"Phase 3: format string used to render the Firecracker API socket path for a given sandbox id.")
	flag.DurationVar(&poolReconcileTick, "pool-reconcile-interval", 30*time.Second,
		"Phase 3: interval between pre-warm pool reconciles. 0 disables the pool loop.")
	flag.DurationVar(&orphanReapTick, "orphan-reap-interval", time.Minute,
		"Interval between sweeps that force-remove orphaned NotReady kata sandboxes "+
			"(microVMs leaked by a failed teardown that still hold a containerd "+
			"name reservation). 0 disables the reaper.")
	flag.StringVar(&entropyReseedMode, "entropy-reseed", "require",
		"Active entropy reseed on snapshot restore (setec#72). 'require' (default) fails a "+
			"restore closed unless the in-guest setec-guest-agent acknowledges fresh entropy "+
			"over vsock; 'off' is an explicit opt-out that leaves only the passive virtio-rng "+
			"mechanism (guest images without setec-guest-agent need this).")
	flag.Parse()

	if entropyReseedMode != "require" && entropyReseedMode != "off" {
		fmt.Fprintf(os.Stderr, "node-agent: invalid --entropy-reseed %q (want \"require\" or \"off\")\n", entropyReseedMode)
		os.Exit(1)
	}

	fmt.Fprintf(os.Stderr, "setec node-agent starting on node=%q pool=%q\n", nodeName, poolName)

	if _, err := os.Stat(kvmDevicePath); errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(os.Stderr, "KVM device %q is missing; node cannot host Sandboxes. Exiting.\n", kvmDevicePath)
		os.Exit(1)
	}

	cfg := nodeagent.Config{
		PoolName:       poolName,
		DataDevice:     dataDev,
		MetadataDevice: metaDev,
		FillThreshold:  fillThreshold,
		SampleInterval: sampleInterval,
	}

	// Register Prometheus collectors with a private registry so we
	// serve exactly the metrics we define here and do not pick up the
	// default Go-runtime collectors twice.
	reg := prometheus.NewRegistry()
	usedGauge := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "setec_node_thinpool_used_bytes",
		Help: "Used bytes in the Setec-managed devicemapper thin-pool.",
	})
	totalGauge := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "setec_node_thinpool_total_bytes",
		Help: "Total bytes available in the Setec-managed devicemapper thin-pool.",
	})
	kataReady := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "setec_node_kata_runtime_ready",
		Help: "Whether the Kata runtime is ready on this node (0 or 1).",
	})
	prefetchErrors := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "setec_node_image_prefetch_errors_total",
		Help: "Total number of OCI image prefetch failures, labeled by error class.",
	}, []string{"reason"})
	orphansReaped := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "setec_node_orphan_sandboxes_reaped_total",
		Help: "Total orphaned kata sandboxes force-removed by the reaper, labeled by runtime handler.",
	}, []string{"handler"})
	orphanReapErrors := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "setec_node_orphan_reap_errors_total",
		Help: "Total errors encountered while listing or removing orphaned kata sandboxes.",
	})
	entropyReseeds := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "setec_node_entropy_reseed_total",
		Help: "Post-restore entropy reseed attempts by outcome (success/failure); failures fail the restore closed.",
	}, []string{"outcome"})
	poolFill := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "setec_prewarm_pool_entries",
		Help: "Number of pre-warmed pool entries currently paused on this node for a SandboxClass.",
	}, []string{"node", "sandbox_class"})
	poolClaims := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "setec_prewarm_pool_claims_total",
		Help: "Pool claim attempts by outcome (restored, miss, restore_failed).",
	}, []string{"outcome"})
	reg.MustRegister(usedGauge, totalGauge, kataReady, prefetchErrors, orphansReaped, orphanReapErrors, entropyReseeds, poolFill, poolClaims)
	// Presence of /dev/kvm is our local ready signal; deeper health
	// checks require the controller-side runtime class and are out
	// of scope for the node agent.
	kataReady.Set(1)

	// Start metrics server.
	go serveMetrics(metricsAddr, reg)

	manager := nodeagent.NewThinPoolManager(cfg)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// Build the production image puller up-front; a failed dial is
	// fatal so the DaemonSet reports the misconfiguration via its
	// restart count rather than running with a broken prefetch path.
	puller, err := nodeagent.NewContainerdPuller(containerdSocket, containerdNamespace, containerdAuthFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "node-agent: dial containerd %q: %v\n", containerdSocket, err)
		os.Exit(1)
	}
	defer func() {
		if cerr := puller.Close(); cerr != nil {
			fmt.Fprintf(os.Stderr, "node-agent: close containerd client: %v\n", cerr)
		}
	}()

	// Orphan-sandbox reaper: force-remove NotReady kata sandboxes whose
	// microVM leaked on a failed teardown ("Agent did not stop sandbox") and
	// still holds a containerd name reservation. Independent of the pool/gRPC
	// path — the leak can happen for any kata Sandbox. Uses the CRI service on
	// the same containerd socket.
	if orphanReapTick > 0 {
		criClient, err := reaper.NewCRIClient(containerdSocket)
		if err != nil {
			fmt.Fprintf(os.Stderr, "node-agent: build CRI client for reaper: %v (reaper disabled)\n", err)
		} else {
			defer func() { _ = criClient.Close() }()
			orphanReaper := &reaper.OrphanReaper{
				Client:   criClient,
				Interval: orphanReapTick,
				Metrics: reaper.Metrics{
					Reaped: func(handler string) { orphansReaped.WithLabelValues(handler).Inc() },
					Errors: orphanReapErrors.Inc,
				},
			}
			go orphanReaper.Run(ctx)
			fmt.Fprintf(os.Stderr, "node-agent: orphan-sandbox reaper started at %s interval\n", orphanReapTick)
		}
	} else {
		fmt.Fprintln(os.Stderr, "node-agent: orphan-sandbox reaper disabled (--orphan-reap-interval=0)")
	}

	// Phase 3: construct the storage backend, pool manager, and gRPC
	// server when the operator has enabled the feature (by setting
	// --grpc-listen-addr to a non-empty value, which is the default).
	if grpcListenAddr != "" {
		if snapshotBackend != "local-disk" {
			fmt.Fprintf(os.Stderr,
				"node-agent: unsupported snapshot backend %q; only local-disk is supported in Phase 3\n",
				snapshotBackend)
			os.Exit(1)
		}
		if err := os.MkdirAll(snapshotRoot, 0o700); err != nil {
			fmt.Fprintf(os.Stderr, "node-agent: mkdir %q: %v\n", snapshotRoot, err)
			os.Exit(1)
		}
		backend := &storage.LocalDiskBackend{
			Root:          snapshotRoot,
			FillThreshold: snapshotFillFraction,
		}
		ffactory := func(sock string) firecracker.Client {
			return firecracker.NewClientFromSocket(sock)
		}
		poolMgr := pool.New(backend, nodeagent.NewImageCache(puller), ffactory, nodeName)
		poolMgr.Launcher = pool.DefaultExecLauncher()
		if kataSocketPattern != "" {
			poolMgr.SocketPattern = kataSocketPattern
		}

		srv := &grpcserver.Server{
			Storage:            backend,
			FirecrackerFactory: ffactory,
			Pool:               poolMgr,
			TempDir:            snapshotRoot + "/tmp",
			ReseedObserver: func(outcome string) {
				entropyReseeds.WithLabelValues(outcome).Inc()
			},
			ClaimObserver: func(outcome string) {
				poolClaims.WithLabelValues(outcome).Inc()
			},
		}
		// Active entropy reseed on restore (setec#72). The default is
		// fail-closed: a restored sandbox is only reported successful
		// once the in-guest setec-guest-agent has acknowledged fresh
		// entropy over vsock. --entropy-reseed=off is the explicit,
		// auditable opt-out for guest images that do not bundle the
		// agent; it leaves only the passive virtio-rng mechanism.
		if entropyReseedMode == "require" {
			srv.Reseeder = entropy.NewVsockReseeder()
			fmt.Fprintln(os.Stderr, "node-agent: entropy reseed on restore ENFORCED (--entropy-reseed=require)")
		} else {
			fmt.Fprintln(os.Stderr,
				"node-agent: entropy reseed on restore DISABLED (--entropy-reseed=off); "+
					"restored snapshot clones rely on passive virtio-rng only")
		}
		go serveGRPC(ctx, grpcListenAddr, srv, grpcTLS(ctx, creds))

		if poolReconcileTick > 0 {
			lister, err := newSandboxClassLister()
			if err != nil {
				fmt.Fprintf(os.Stderr, "node-agent: build SandboxClass lister: %v (pool reconcile disabled)\n", err)
			} else {
				reconciler := &pool.TickReconciler{
					Manager:  poolMgr,
					Lister:   lister,
					Interval: poolReconcileTick,
					FillObserver: func(fills map[string]int) {
						// Reset first so classes whose pools were torn
						// down (class deleted or pool disabled) drop
						// their stale series instead of freezing at the
						// last non-zero value.
						poolFill.Reset()
						for class, n := range fills {
							poolFill.WithLabelValues(nodeName, class).Set(float64(n))
						}
					},
				}
				go reconciler.Run(ctx)
				fmt.Fprintf(os.Stderr, "node-agent: pool reconciler started at %s interval\n", poolReconcileTick)
			}
		} else {
			fmt.Fprintln(os.Stderr, "node-agent: pool reconciler disabled (--pool-reconcile-interval=0)")
		}
	}

	if dataDev != "" && metaDev != "" {
		if err := manager.Ensure(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "node-agent: thin-pool ensure failed: %v\n", err)
			// Continue so the /metrics endpoint stays up and the
			// operator can see the failure; do not exit.
		}
	} else {
		fmt.Fprintln(os.Stderr, "node-agent: thinpool-data-device / thinpool-metadata-device not set; skipping Ensure")
	}

	// Prefetch loop is single-shot on startup. A long-running loop is
	// unnecessary because SandboxClass changes drive future pulls.
	if prefetchImages != "" {
		refs := strings.Fields(prefetchImages)
		fmt.Fprintf(os.Stderr, "node-agent: prefetching %d images\n", len(refs))
		cache := nodeagent.NewImageCache(puller)
		if err := cache.Prefetch(ctx, refs); err != nil {
			fmt.Fprintf(os.Stderr, "node-agent: prefetch failed: %v\n", err)
			prefetchErrors.WithLabelValues(prefetchErrorReason(err)).Inc()
		}
	}

	// Monitor loop.
	ticker := time.NewTicker(sampleInterval)
	defer ticker.Stop()
	for {
		sample, err := manager.Sample(ctx)
		if err == nil {
			usedGauge.Set(float64(sample.Used * 512))
			totalGauge.Set(float64(sample.Total * 512))
			if sample.Degraded {
				fmt.Fprintf(os.Stderr,
					"node-agent: thin-pool %q degraded: %d%% used (threshold %d%%)\n",
					poolName, sample.FillPercent, fillThreshold)
			}
		}
		select {
		case <-ctx.Done():
			fmt.Fprintln(os.Stderr, "node-agent: shutdown signal received, exiting cleanly")
			return
		case <-ticker.C:
		}
	}
}

// serveMetrics runs the Prometheus HTTP endpoint. It exits the process
// on listener errors rather than trying to recover — the metrics
// endpoint is the only long-running HTTP surface this binary owns.
func serveMetrics(addr string, reg *prometheus.Registry) {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{Registry: reg}))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintf(os.Stderr, "node-agent: metrics server exited: %v\n", err)
		os.Exit(1)
	}
}

// prefetchErrorReason maps a ContainerdPuller error to the label value
// applied to setec_node_image_prefetch_errors_total. Unknown errors
// fall through to "pull_failed" so every increment carries a label.
func prefetchErrorReason(err error) string {
	switch {
	case errors.Is(err, nodeagent.ErrContainerdUnreachable):
		return "containerd_unreachable"
	case errors.Is(err, nodeagent.ErrImageNotFound):
		return "image_not_found"
	case errors.Is(err, nodeagent.ErrAuthRequired):
		return "auth_required"
	default:
		return "pull_failed"
	}
}

// Credential mode names, used only in log and error output so an
// operator can tell from a pod's logs which posture it is running.
// They match cmd/frontend's names because an operator comparing two
// pods' logs is comparing postures, not components.
const (
	fileMode        = "file"
	spiffeMode      = "spiffe"
	conflictingMode = "conflicting"
	unsetMode       = "unset"
)

// credentialFlags carries the node-agent's credential flags.
type credentialFlags struct {
	tlsCert             string
	tlsKey              string
	tlsClientCA         string
	spiffeSocket        string
	spiffeAuthorizedIDs repeatedString
}

// config maps the flags onto a credentials.Config and names the mode
// they selected.
//
// It deliberately validates nothing, and it is deliberately a copy of
// cmd/frontend's function rather than an approximation of it. A source
// is *selected* by any of its flags being set, not by all of them;
// whether the selection is coherent — both modes, neither, or half of
// one — is credentials.New's decision, so that every setec component
// gets the same answer and the same message. An operator must not be
// able to end up with the frontend on SPIFFE and the node-agent
// silently still on files, and the way to guarantee that is for
// neither component to hold an opinion of its own.
func (f credentialFlags) config() (credentials.Config, string) {
	var (
		cfg  credentials.Config
		mode = unsetMode
	)
	if f.tlsCert != "" || f.tlsKey != "" || f.tlsClientCA != "" {
		cfg.Files = &credentials.FileSource{
			CertFile: f.tlsCert,
			KeyFile:  f.tlsKey,
			CAFile:   f.tlsClientCA,
		}
		mode = fileMode
	}
	if f.spiffeSocket != "" || len(f.spiffeAuthorizedIDs) > 0 {
		cfg.SPIFFE = &credentials.SPIFFESource{
			SocketPath:    f.spiffeSocket,
			AuthorizedIDs: f.spiffeAuthorizedIDs,
		}
		mode = spiffeMode
	}
	if cfg.Files != nil && cfg.SPIFFE != nil {
		mode = conflictingMode
	}
	return cfg, mode
}

// repeatedString collects a flag given more than once. The credential
// allow-list is a list of full SPIFFE IDs, and repeating the flag keeps
// each entry visible on its own line in a manifest rather than buried
// in a delimited string.
type repeatedString []string

func (r *repeatedString) String() string { return strings.Join(*r, ",") }

func (r *repeatedString) Set(v string) error {
	*r = append(*r, v)
	return nil
}

// serverCredentials resolves the gRPC server option carrying this
// node-agent's mTLS credentials, and names the mode it used.
//
// Where the key material comes from, what the TLS floor is, whether a
// client certificate is required and verified, and which peers are
// authorized are all properties of internal/credentials — this
// function only says which surface it needs.
func serverCredentials(ctx context.Context, f credentialFlags) (grpc.ServerOption, string, error) {
	cfg, mode := f.config()
	provider, err := credentials.New(cfg)
	if err != nil {
		return nil, mode, err
	}
	// Acquiring the credentials here rather than lazily is what makes
	// an unreachable SPIFFE Workload API a boot failure. There is no
	// fallback to files.
	creds, err := provider.ServerCredentials(ctx)
	if err != nil {
		return nil, mode, err
	}
	return grpc.Creds(creds), mode, nil
}

// grpcTLS returns the credentials option for the gRPC server. mTLS is
// mandatory and the credential mode is explicit: half a mode, both
// modes, or neither causes the process to exit so the DaemonSet
// surfaces the misconfiguration via its restart count.
func grpcTLS(ctx context.Context, f credentialFlags) grpc.ServerOption {
	opt, mode, err := serverCredentials(ctx, f)
	if err != nil {
		fmt.Fprintf(os.Stderr, "node-agent: credentials (%s mode): %v\n", mode, err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "node-agent: credential mode: %s\n", mode)
	return opt
}

// newSandboxClassLister builds a controller-runtime client and
// returns a pool.SandboxClassLister that performs a cluster-wide
// List of SandboxClass resources on every invocation. The list is
// small (SandboxClass is a cluster-scoped singleton-ish CRD) so
// per-tick List is cheaper and simpler than running a full
// informer cache inside the node-agent.
func newSandboxClassLister() (func() []setecv1alpha1.SandboxClass, error) {
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(setecv1alpha1.AddToScheme(scheme))
	cfg, err := ctrl.GetConfig()
	if err != nil {
		return nil, fmt.Errorf("GetConfig: %w", err)
	}
	c, err := ctrlclient.New(cfg, ctrlclient.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("client.New: %w", err)
	}
	return func() []setecv1alpha1.SandboxClass {
		listCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		var list setecv1alpha1.SandboxClassList
		if err := c.List(listCtx, &list); err != nil {
			fmt.Fprintf(os.Stderr, "node-agent: list SandboxClass: %v\n", err)
			return nil
		}
		return list.Items
	}, nil
}

// serveGRPC binds a TCP listener, registers the NodeAgentService
// implementation, and blocks until the supplied context is cancelled.
// Errors during Serve cause the process to exit so the DaemonSet
// restarts the pod and re-reads any rotated secrets.
func serveGRPC(ctx context.Context, addr string, srv *grpcserver.Server, opts ...grpc.ServerOption) {
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "node-agent: grpc listen %q: %v\n", addr, err)
		os.Exit(1)
	}
	s := grpc.NewServer(opts...)
	setecgrpcv1.RegisterNodeAgentServiceServer(s, srv)
	fmt.Fprintf(os.Stderr, "node-agent: NodeAgentService listening on %s\n", addr)

	go func() {
		<-ctx.Done()
		s.GracefulStop()
	}()
	if err := s.Serve(lis); err != nil {
		fmt.Fprintf(os.Stderr, "node-agent: grpc serve: %v\n", err)
		os.Exit(1)
	}
}
