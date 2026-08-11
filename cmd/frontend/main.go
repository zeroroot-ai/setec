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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	setecv1grpc "github.com/zeroroot-ai/setec/api/grpc/v1"
	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
	"github.com/zeroroot-ai/setec/internal/credentials"
	"github.com/zeroroot-ai/setec/internal/frontend"
	"github.com/zeroroot-ai/setec/internal/tenancy"

	"google.golang.org/grpc"
)

func main() {
	var (
		listenAddr        string
		creds             credentialFlags
		tenantLabelKey    string
		metricsAddr       string
		shutdownGraceTime time.Duration
	)
	flag.StringVar(&listenAddr, "listen-addr", ":50051", "gRPC server listen address.")
	flag.StringVar(&creds.tlsCert, "tls-cert", "",
		"Path to server TLS certificate. Selects file credential mode, the default.")
	flag.StringVar(&creds.tlsKey, "tls-key", "",
		"Path to server TLS key. Selects file credential mode, the default.")
	flag.StringVar(&creds.tlsClientCA, "tls-client-ca", "",
		"Path to client-CA bundle enabling mTLS. Selects file credential mode, the default.")
	flag.StringVar(&creds.spiffeSocket, "spiffe-socket", "",
		"SPIFFE Workload API socket, e.g. unix:///run/spire/agent-sockets/api.sock. "+
			"Selects SPIFFE credential mode; mutually exclusive with the --tls-* flags.")
	flag.Var(&creds.spiffeAuthorizedIDs, "spiffe-authorized-id",
		"Full SPIFFE ID allowed to call this frontend, e.g. spiffe://zeroroot.ai/ns/gibson/sa/gibson. "+
			"Repeat for each caller. Required in SPIFFE mode; there is no accept-everyone setting.")
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

	// mTLS is mandatory and the credential mode is explicit. Half a
	// mode, both modes, or neither is a misconfiguration the Deployment
	// should restart out of, not paper over; credentials.New is what
	// decides that, so there is one answer and not one per component.
	credConfig, credMode := creds.config()
	provider, err := credentials.New(credConfig)
	if err != nil {
		fmt.Fprintf(os.Stderr, "frontend: credentials: %v\n", err)
		os.Exit(1)
	}
	// Acquiring the credentials here rather than lazily is what makes
	// an unreachable SPIFFE Workload API a boot failure. There is no
	// fallback to files.
	serverCreds, err := provider.ServerCredentials(context.Background())
	if err != nil {
		fmt.Fprintf(os.Stderr, "frontend: credentials (%s mode): %v\n", credMode, err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "frontend: credential mode: %s\n", credMode)
	grpcOpts := []grpc.ServerOption{grpc.Creds(serverCreds)}

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

// Credential mode names, used only in log and error output so an
// operator can tell from a pod's logs which posture it is running.
const (
	fileMode        = "file"
	spiffeMode      = "spiffe"
	conflictingMode = "conflicting"
	unsetMode       = "unset"
)

// credentialFlags carries the frontend's credential flags.
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
// It deliberately validates nothing. A source is *selected* by any of
// its flags being set, not by all of them; whether the selection is
// coherent — both modes, neither, or half of one — is
// credentials.New's decision, so that every setec component gets the
// same answer and the same message. Selecting on "any flag set" is what
// makes a typo in one flag name a startup error naming the missing
// piece rather than a silent switch to the other mode.
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
