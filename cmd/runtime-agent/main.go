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

// Command runtime-agent is the Setec per-node runtime capability probe DaemonSet
// binary. It probes each configured isolation backend (kata-fc, kata-qemu,
// gvisor, runc), writes the results as Node labels and a SetecRuntimes
// condition, emits Prometheus metrics, and re-probes on the configured interval.
//
// In "static" NodeCapabilitiesMode the binary exits immediately with code 0;
// the operator uses staticCapabilities from the RuntimeConfig directly.
//
// All business logic lives in cmd/runtime-agent/run.go and
// internal/runtimeagent/ and is unit-tested without real system calls.
// main.go handles only flag parsing, signal handling, client construction,
// and HTTP server lifecycle.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
	"github.com/zeroroot-ai/setec/internal/metrics"
	internalruntime "github.com/zeroroot-ai/setec/internal/runtime"
	"github.com/zeroroot-ai/setec/internal/runtimeagent/probe"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(setecv1alpha1.AddToScheme(scheme))
}

func main() {
	var (
		runtimesConfig string
		nodeName       string
		kubeconfig     string
		metricsAddr    string
	)

	flag.StringVar(&runtimesConfig, "runtimes-config", "",
		"Path to the RuntimeConfig YAML file. Required.")
	flag.StringVar(&nodeName, "node-name", os.Getenv("NODE_NAME"),
		"Kubernetes Node this pod runs on. Defaults to $NODE_NAME.")
	flag.StringVar(&kubeconfig, "kubeconfig", "",
		"Path to a kubeconfig file. Empty means in-cluster config.")
	flag.StringVar(&metricsAddr, "metrics-addr", ":8080",
		"Address for the Prometheus /metrics and /healthz HTTP endpoint.")
	flag.Parse()

	// --- Validate required flags ----------------------------------------

	if runtimesConfig == "" {
		fmt.Fprintln(os.Stderr, "runtime-agent: --runtimes-config is required")
		os.Exit(1)
	}
	if nodeName == "" {
		fmt.Fprintln(os.Stderr, "runtime-agent: --node-name is required (or set $NODE_NAME)")
		os.Exit(1)
	}

	// --- Load and validate RuntimeConfig --------------------------------

	cfg, err := internalruntime.LoadFromFile(runtimesConfig)
	if err != nil {
		fmt.Fprintf(os.Stderr, "runtime-agent: load config: %v\n", err)
		os.Exit(1)
	}

	// --- Handle static mode early exit ----------------------------------

	if cfg.Defaults.Runtime.NodeCapabilitiesMode == "static" {
		fmt.Fprintln(os.Stderr, "runtime-agent: static mode, exiting")
		os.Exit(0)
	}

	fmt.Fprintf(os.Stderr, "runtime-agent: starting on node=%q runtimes-config=%q\n", nodeName, runtimesConfig)

	// --- Build Kubernetes client ----------------------------------------

	k8sCfg, err := buildKubeConfig(kubeconfig)
	if err != nil {
		fmt.Fprintf(os.Stderr, "runtime-agent: build kubeconfig: %v\n", err)
		os.Exit(1)
	}
	c, err := ctrlclient.New(k8sCfg, ctrlclient.Options{Scheme: scheme})
	if err != nil {
		fmt.Fprintf(os.Stderr, "runtime-agent: build client: %v\n", err)
		os.Exit(1)
	}

	// --- Build probes ---------------------------------------------------

	// AllowTCG is sourced from BackendConfig.Params["allowTcg"]. Because
	// BackendConfig does not yet carry a Params map (not extended in this
	// task), AllowTCG defaults to false. See summary for extension note.
	allProbes := probe.AllProbes(probe.Config{
		FSRoot:   "/",
		LookPath: exec.LookPath,
		AllowTCG: false,
	})
	filteredProbes := filterProbes(allProbes, cfg)

	// --- Prometheus metrics ---------------------------------------------

	reg := prometheus.NewRegistry()
	col := metrics.NewCollectorsWith(reg)

	// --- Signal context ------------------------------------------------

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// --- HTTP server (metrics + healthz) --------------------------------

	go serveHTTP(ctx, metricsAddr, reg)

	// --- Probe loop -----------------------------------------------------

	deps := Dependencies{
		Client:     c,
		Probes:     filteredProbes,
		NodeName:   nodeName,
		Interval:   cfg.Defaults.Runtime.ProbeInterval,
		Collectors: col,
	}

	Run(ctx, deps)

	// Graceful drain: give the HTTP server up to 4s to close after ctx
	// cancels so shutdown completes well inside the 5s SIGTERM budget.
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer shutdownCancel()
	<-shutdownCtx.Done()
}

// buildKubeConfig returns a *rest.Config derived from the provided kubeconfig
// path, or from the in-cluster environment when path is empty.
func buildKubeConfig(path string) (*rest.Config, error) {
	if path == "" {
		// ctrl.GetConfigOrDie panics; use the non-fatal variant.
		cfg, err := ctrl.GetConfig()
		if err != nil {
			return nil, fmt.Errorf("in-cluster config: %w", err)
		}
		return cfg, nil
	}
	cfg, err := clientcmd.BuildConfigFromFlags("", path)
	if err != nil {
		return nil, fmt.Errorf("kubeconfig %q: %w", path, err)
	}
	return cfg, nil
}

// serveHTTP runs the Prometheus /metrics and /healthz HTTP endpoint until ctx
// is cancelled. It performs a graceful shutdown with a short deadline so the
// overall shutdown stays within the 5s SIGTERM budget.
func serveHTTP(ctx context.Context, addr string, reg *prometheus.Registry) {
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

	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutCtx); err != nil {
			fmt.Fprintf(os.Stderr, "runtime-agent: http shutdown: %v\n", err)
		}
	}()

	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintf(os.Stderr, "runtime-agent: http serve: %v\n", err)
		os.Exit(1)
	}
}
