// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

// Command frontend is the Setec gRPC frontend service. It wraps the
// controller-runtime client, speaks setec.v1.SandboxService, and
// enforces tenant scoping on every RPC.
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
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	setecv1grpc "github.com/zeroroot-ai/setec/api/grpc/v1"
	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
	"github.com/zeroroot-ai/setec/internal/credentials"
	"github.com/zeroroot-ai/setec/internal/frontend"

	"google.golang.org/grpc"
)

func main() {
	var (
		listenAddr        string
		creds             credentialFlags
		clients           repeatedString
		grants            repeatedString
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
	flag.Var(&clients, "client",
		"An enrolled client cluster, as <name>=<spiffe-id>, e.g. "+
			"saas=spiffe://example.org/ns/gibson/sa/gibson-daemon. Repeat for each Gibson cluster. "+
			"Required: the frontend refuses every caller that is not enrolled. In SPIFFE mode "+
			"these IDs are also the credential allow-list.")
	flag.Var(&grants, "pair-namespace-grant",
		"A RoleBinding that each new pair namespace gets, as <cluster-role>=<sa-namespace>/<sa-name>. "+
			"Repeat for each: the operator needs its Pod-write role and the frontend its exec role there.")
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

	// Each Gibson cluster is a named client. The pair of the client and the
	// tenant of the request has one namespace, which the frontend makes on
	// the first call of the pair (docs/design/isolation.md).
	enrollment, err := frontend.ParseEnrollment(clients)
	if err != nil {
		fmt.Fprintf(os.Stderr, "frontend: %v\n", err)
		os.Exit(1)
	}
	roleGrants, err := parseGrants(grants)
	if err != nil {
		fmt.Fprintf(os.Stderr, "frontend: %v\n", err)
		os.Exit(1)
	}
	resolver := &frontend.NamespaceProvisioner{Client: k8sClient, Grants: roleGrants}
	fmt.Fprintf(os.Stderr, "frontend: %d enrolled clients\n", len(enrollment.SPIFFEIDs()))
	srv := &frontend.Service{
		Client:    k8sClient,
		Clientset: clientset,
		// Exec opens pods/exec streams, which need the REST config
		// itself: the connection is an HTTP upgrade the typed clientset
		// does not model.
		RESTConfig: cfg,
		Enrollment: enrollment,
		Resolver:   resolver,
	}
	leaseSrv := &frontend.LeaseService{
		Client:     k8sClient,
		Clientset:  clientset,
		Enrollment: enrollment,
		Resolver:   resolver,
	}

	// mTLS is mandatory and the credential mode is explicit. Half a
	// mode, both modes, or neither is a misconfiguration the Deployment
	// should restart out of, not paper over; credentials.New is what
	// decides that, so there is one answer and not one per component.
	credConfig, credMode := creds.config(enrollment.SPIFFEIDs())
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
	// lifetime; they stop when ctx is canceled on shutdown.
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
	tlsCert      string
	tlsKey       string
	tlsClientCA  string
	spiffeSocket string
}

// config maps the flags onto a credentials.Config and names the mode
// they selected. In SPIFFE mode the allow-list is the SPIFFE ID of each
// enrolled client, so one list names who may call.
//
// It deliberately validates nothing. A source is *selected* by any of
// its flags being set, not by all of them; whether the selection is
// coherent — both modes, neither, or half of one — is
// credentials.New's decision, so that every setec component gets the
// same answer and the same message. Selecting on "any flag set" is what
// makes a typo in one flag name a startup error naming the missing
// piece rather than a silent switch to the other mode.
func (f credentialFlags) config(enrolledIDs []string) (credentials.Config, string) {
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
	if f.spiffeSocket != "" {
		cfg.SPIFFE = &credentials.SPIFFESource{
			SocketPath:    f.spiffeSocket,
			AuthorizedIDs: enrolledIDs,
		}
		mode = spiffeMode
	}
	if cfg.Files != nil && cfg.SPIFFE != nil {
		mode = conflictingMode
	}
	return cfg, mode
}

// repeatedString collects a flag given more than once. The enrolled
// clients are a list, and repeating the flag keeps
// each entry visible on its own line in a manifest rather than buried
// in a delimited string.
type repeatedString []string

func (r *repeatedString) String() string { return strings.Join(*r, ",") }

func (r *repeatedString) Set(v string) error {
	*r = append(*r, v)
	return nil
}

// parseGrants reads the --pair-namespace-grant entries. At least one is
// required: without the Pod-write grant of the operator, no Sandbox of a
// new pair could run.
func parseGrants(entries []string) ([]frontend.RoleGrant, error) {
	if len(entries) == 0 {
		return nil, errors.New("no --pair-namespace-grant is set; " +
			"a new pair namespace would hold no Pod-write grant for the operator")
	}
	out := make([]frontend.RoleGrant, 0, len(entries))
	for _, e := range entries {
		role, sa, ok := strings.Cut(e, "=")
		ns, name, ok2 := strings.Cut(sa, "/")
		if !ok || !ok2 || role == "" || ns == "" || name == "" {
			return nil, fmt.Errorf("--pair-namespace-grant %q is not <cluster-role>=<sa-namespace>/<sa-name>", e)
		}
		out = append(out, frontend.RoleGrant{
			ClusterRole:    role,
			ServiceAccount: types.NamespacedName{Namespace: ns, Name: name},
		})
	}
	return out, nil
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
