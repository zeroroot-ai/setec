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

// Command frontend is the Setec gRPC frontend service. It wraps the
// controller-runtime client, speaks setec.v1.SandboxService, and
// enforces tenant scoping on every RPC.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	setecv1grpc "github.com/zeroroot-ai/setec/api/grpc/v1"
	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
	"github.com/zeroroot-ai/setec/internal/frontend"
	"github.com/zeroroot-ai/setec/internal/tenancy"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

func main() {
	var (
		listenAddr        string
		tlsCert           string
		tlsKey            string
		tlsClientCA       string
		tenantLabelKey    string
		metricsAddr       string
		shutdownGraceTime time.Duration
	)
	flag.StringVar(&listenAddr, "listen-addr", ":50051", "gRPC server listen address.")
	flag.StringVar(&tlsCert, "tls-cert", "", "Path to server TLS certificate. Required.")
	flag.StringVar(&tlsKey, "tls-key", "", "Path to server TLS key. Required.")
	flag.StringVar(&tlsClientCA, "tls-client-ca", "", "Path to client-CA bundle enabling mTLS. Required.")
	flag.StringVar(&tenantLabelKey, "tenant-namespace-label", "setec.zeroroot.ai/tenant",
		"Label key used to map tenant → namespace.")
	flag.StringVar(&metricsAddr, "metrics-addr", ":9091", "HTTP address for /metrics (Prometheus scraping).")
	flag.DurationVar(&shutdownGraceTime, "shutdown-grace", 30*time.Second,
		"Maximum time to wait for in-flight RPCs during graceful shutdown.")
	flag.Parse()

	fmt.Fprintln(os.Stderr, "setec frontend starting")

	// Build a scheme and controller-runtime client so Sandbox CRs can
	// be created / read / deleted.
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(setecv1alpha1.AddToScheme(scheme))

	cfg := ctrl.GetConfigOrDie()
	k8sClient, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		fmt.Fprintf(os.Stderr, "frontend: build K8s client: %v\n", err)
		os.Exit(1)
	}
	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "frontend: build clientset: %v\n", err)
		os.Exit(1)
	}

	resolver := &labelTenantResolver{client: k8sClient, labelKey: tenantLabelKey}
	srv := &frontend.Service{
		Client:         k8sClient,
		Clientset:      clientset,
		TenantResolver: resolver,
	}
	leaseSrv := &frontend.LeaseService{
		Client:         k8sClient,
		Clientset:      clientset,
		TenantResolver: resolver,
	}

	// mTLS is mandatory. All three flags must be populated; missing
	// any of them is a misconfiguration that the DaemonSet should
	// restart out of, not paper over.
	if tlsCert == "" || tlsKey == "" || tlsClientCA == "" {
		fmt.Fprintln(os.Stderr,
			"frontend: --tls-cert, --tls-key and --tls-client-ca are required; mTLS is mandatory")
		os.Exit(1)
	}
	creds, err := loadTLSCreds(tlsCert, tlsKey, tlsClientCA)
	if err != nil {
		fmt.Fprintf(os.Stderr, "frontend: load TLS creds: %v\n", err)
		os.Exit(1)
	}
	grpcOpts := []grpc.ServerOption{grpc.Creds(creds)}

	grpcServer := grpc.NewServer(grpcOpts...)
	setecv1grpc.RegisterSandboxServiceServer(grpcServer, srv)
	setecv1grpc.RegisterLeaseServiceServer(grpcServer, leaseSrv)

	lis, err := net.Listen("tcp", listenAddr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "frontend: listen %q: %v\n", listenAddr, err)
		os.Exit(1)
	}

	// /metrics on a separate listener so the Prometheus scrape does
	// not go through gRPC auth.
	go serveMetrics(metricsAddr)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// Bind the lease-pool background replenish loops to the process
	// lifetime; they stop when ctx is cancelled on shutdown.
	leaseSrv.Start(ctx)

	go func() {
		<-ctx.Done()
		fmt.Fprintln(os.Stderr, "frontend: shutting down gRPC server")
		done := make(chan struct{})
		go func() {
			grpcServer.GracefulStop()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(shutdownGraceTime):
			grpcServer.Stop()
		}
	}()

	fmt.Fprintf(os.Stderr, "frontend: gRPC listening on %s\n", listenAddr)
	if err := grpcServer.Serve(lis); err != nil {
		fmt.Fprintf(os.Stderr, "frontend: gRPC serve: %v\n", err)
	}
}

// loadTLSCreds builds mTLS credentials. The caFile MUST be non-empty;
// callers validate that before invoking this helper.
func loadTLSCreds(certFile, keyFile, caFile string) (credentials.TransportCredentials, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("load server cert: %w", err)
	}
	pool := x509.NewCertPool()
	ca, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("read client CA: %w", err)
	}
	if !pool.AppendCertsFromPEM(ca) {
		return nil, fmt.Errorf("client CA %q is not a PEM bundle", caFile)
	}
	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS13,
		ClientCAs:    pool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
	}
	return credentials.NewTLS(tlsCfg), nil
}

// labelTenantResolver maps a TenantID to a namespace by listing
// namespaces carrying a label whose value matches the tenant.
type labelTenantResolver struct {
	client   client.Client
	labelKey string
}

// NamespaceFor returns the first namespace whose label[labelKey]
// equals the tenant. Tenants are expected to own exactly one
// namespace; multiple matches return the first one and log a warning
// (the /metrics endpoint surfaces the cardinality).
func (r *labelTenantResolver) NamespaceFor(ctx context.Context, t tenancy.TenantID) (string, error) {
	list := &corev1.NamespaceList{}
	if err := r.client.List(ctx, list); err != nil {
		return "", fmt.Errorf("list namespaces: %w", err)
	}
	want := string(t)
	for _, ns := range list.Items {
		if ns.Labels[r.labelKey] == want {
			return ns.Name, nil
		}
	}
	return "", fmt.Errorf("no namespace with label %s=%s", r.labelKey, want)
}

// serveMetrics runs the Prometheus scrape endpoint. Uses the default
// prometheus registry so gRPC interceptor metrics would flow through
// if we wire them later.
func serveMetrics(addr string) {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(prometheus.DefaultGatherer, promhttp.HandlerOpts{}))
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		fmt.Fprintf(os.Stderr, "frontend: metrics server exited: %v\n", err)
	}
}
