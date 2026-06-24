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
	"crypto/tls"
	"crypto/x509"
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
	"google.golang.org/grpc/credentials"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	setecgrpcv1 "github.com/zeroroot-ai/setec/api/grpc/v1"
	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
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
		tlsCertPath          string
		tlsKeyPath           string
		tlsClientCAPath      string
		snapshotBackend      string
		snapshotRoot         string
		snapshotFillFraction float64
		kataSocketPattern    string
		poolReconcileTick    time.Duration
		orphanReapTick       time.Duration
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
	flag.StringVar(&tlsCertPath, "tls-cert", "",
		"Phase 3: path to the PEM-encoded server certificate for mTLS. Required when --grpc-listen-addr is non-empty.")
	flag.StringVar(&tlsKeyPath, "tls-key", "",
		"Phase 3: path to the PEM-encoded server private key. Required when --grpc-listen-addr is non-empty.")
	flag.StringVar(&tlsClientCAPath, "tls-client-ca", "",
		"Phase 3: path to the PEM-encoded CA used to verify operator client certificates."+
			" Required when --grpc-listen-addr is non-empty.")
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
	flag.Parse()

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
	reg.MustRegister(usedGauge, totalGauge, kataReady, prefetchErrors, orphansReaped, orphanReapErrors)
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
		}
		go serveGRPC(ctx, grpcListenAddr, srv, grpcTLS(tlsCertPath, tlsKeyPath, tlsClientCAPath))

		if poolReconcileTick > 0 {
			lister, err := newSandboxClassLister()
			if err != nil {
				fmt.Fprintf(os.Stderr, "node-agent: build SandboxClass lister: %v (pool reconcile disabled)\n", err)
			} else {
				reconciler := &pool.TickReconciler{
					Manager:  poolMgr,
					Lister:   lister,
					Interval: poolReconcileTick,
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

// grpcTLS returns the credentials option for the gRPC server. mTLS is
// mandatory: missing any of cert/key/client-ca causes the process to
// exit so the DaemonSet surfaces the misconfiguration via its restart
// count.
func grpcTLS(certPath, keyPath, clientCAPath string) grpc.ServerOption {
	if certPath == "" || keyPath == "" || clientCAPath == "" {
		fmt.Fprintln(os.Stderr,
			"node-agent: --tls-cert/--tls-key/--tls-client-ca are required; mTLS is mandatory")
		os.Exit(1)
	}
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "node-agent: load tls keypair: %v\n", err)
		os.Exit(1)
	}
	caBytes, err := os.ReadFile(clientCAPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "node-agent: read client-ca: %v\n", err)
		os.Exit(1)
	}
	certPool := x509.NewCertPool()
	if !certPool.AppendCertsFromPEM(caBytes) {
		fmt.Fprintf(os.Stderr, "node-agent: client-ca file contains no usable certificates\n")
		os.Exit(1)
	}
	return grpc.Creds(credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    certPool,
		MinVersion:   tls.VersionTLS13,
	}))
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
