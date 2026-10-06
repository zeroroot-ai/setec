// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

// Command node-agent is the Setec daemon on each fleet node. It serves
// the NodeAgentService that the operator calls to snapshot, restore,
// pause and resume the Firecracker machine of a launcher Pod on the node
// (internal/nodeagent/grpcserver), and a /metrics endpoint.
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

	setecgrpcv1 "github.com/zeroroot-ai/setec/api/grpc/v1"
	"github.com/zeroroot-ai/setec/internal/credentials"
	"github.com/zeroroot-ai/setec/internal/firecracker"
	"github.com/zeroroot-ai/setec/internal/nodeagent/grpcserver"
	"github.com/zeroroot-ai/setec/internal/nodeagent/launchersandbox"
	"github.com/zeroroot-ai/setec/internal/snapshot/storage"
)

// requireMode is the strict value of the entropy reseed mode flag.
const requireMode = "require"

// kvmDevicePath is the Linux device node of KVM. A node without it cannot
// host a launcher machine.
const kvmDevicePath = "/dev/kvm"

func main() {
	var (
		metricsAddr    string
		kubeletPodsDir string
		nodeName       string

		grpcListenAddr       string
		creds                credentialFlags
		snapshotRoot         string
		snapshotKeyFile      string
		snapshotDEKDir       string
		snapshotFillFraction float64
		entropyReseedMode    string

		// Session-checkpoint (S3-compatible) flags — setec#194.
		s3Endpoint  string
		s3Bucket    string
		s3Region    string
		s3Prefix    string
		s3PathStyle bool

		staleMultipartWindow time.Duration
	)
	flag.StringVar(&metricsAddr, "metrics-addr", ":9090",
		"Listen address for the Prometheus /metrics endpoint.")
	flag.StringVar(&nodeName, "node-name", os.Getenv("NODE_NAME"),
		"Name of the Kubernetes Node this agent runs on (defaults to $NODE_NAME).")
	flag.StringVar(&kubeletPodsDir, "kubelet-pods-dir", launchersandbox.DefaultPodsDir,
		"the Pod directory of the kubelet; the agent finds the work volume of a launcher Pod under it")
	flag.StringVar(&grpcListenAddr, "grpc-listen-addr", ":50052",
		"address the NodeAgentService gRPC server listens on. Empty disables the server.")
	flag.StringVar(&creds.tlsCert, "tls-cert", "",
		"path to the PEM-encoded server certificate for mTLS. Selects file credential mode, the default.")
	flag.StringVar(&creds.tlsKey, "tls-key", "",
		"path to the PEM-encoded server private key. Selects file credential mode, the default.")
	flag.StringVar(&creds.tlsClientCA, "tls-client-ca", "",
		"path to the PEM-encoded CA used to verify operator client certificates. "+
			"Selects file credential mode, the default.")
	flag.StringVar(&creds.spiffeSocket, "spiffe-socket", "",
		"SPIFFE Workload API socket, e.g. unix:///run/spire/agent-sockets/api.sock. "+
			"Selects SPIFFE credential mode; mutually exclusive with the --tls-* flags.")
	flag.Var(&creds.spiffeAuthorizedIDs, "spiffe-authorized-id",
		"Full SPIFFE ID allowed to call this node-agent, e.g. spiffe://example.org/ns/setec/sa/setec. "+
			"Repeat for each caller. Required in SPIFFE mode; there is no accept-everyone setting.")
	flag.StringVar(&snapshotRoot, "snapshot-root", "/var/lib/setec/snapshots",
		"root directory for persisted snapshot state files.")
	flag.StringVar(&snapshotKeyFile, "snapshot-key-file", "/var/lib/setec/keys/node.key",
		"Node-local key-encryption-key file snapshot DEKs are sealed with (created on "+
			"first use). Snapshots are ALWAYS encrypted at rest (docs/design/isolation.md); there is no opt-out.")
	flag.StringVar(&snapshotDEKDir, "snapshot-dek-dir", "/var/lib/setec/keys/dek",
		"Directory holding per-snapshot sealed data-encryption keys. Kept OUTSIDE the "+
			"snapshot root so artifact-tree copies carry no key material.")
	flag.Float64Var(&snapshotFillFraction, "snapshot-fill-threshold", 0.85,
		"refuse new snapshots when the snapshot-root filesystem's used fraction exceeds this value.")
	flag.StringVar(&s3Endpoint, "s3-endpoint", "",
		"Base URL of the S3-compatible object store session checkpoints are written to "+
			"(e.g. http://minio.minio.svc:9000 for MinIO). Empty uses the AWS default "+
			"endpoint resolution for --s3-region (real S3 on EKS).")
	flag.StringVar(&s3Bucket, "s3-bucket", "",
		"Bucket for session memory checkpoints. Empty disables the S3 checkpoint backend; "+
			"session suspend/resume-on-drain is then unavailable on this node (docs/design/storage.md).")
	flag.StringVar(&s3Region, "s3-region", "us-east-1",
		"Signing region for the S3-compatible store. MinIO accepts any non-empty value.")
	flag.StringVar(&s3Prefix, "s3-prefix", "",
		"Optional key prefix applied to every checkpoint object.")
	flag.BoolVar(&s3PathStyle, "s3-path-style", true,
		"Use path-style S3 addressing (required by MinIO and most self-hosted stores; "+
			"set false for real S3 virtual-hosted addressing).")
	flag.DurationVar(&staleMultipartWindow, "s3-stale-multipart-window", 6*time.Hour,
		"At startup, abort multipart checkpoint uploads under --s3-prefix older than this "+
			"(setec#297). An upload orphaned by an OOM or a node drain is billed forever and "+
			"is invisible to ListObjects. MUST exceed the longest suspend any agent sharing "+
			"the prefix will run, or the sweep aborts a live upload.")
	flag.StringVar(&entropyReseedMode, "entropy-reseed", requireMode,
		"Entropy reseed after a restore (setec#72). 'require' (default) fails a restore closed "+
			"unless the guest confirms fresh entropy; 'off' is an explicit opt-out, and the "+
			"operator gate then refuses each restore.")
	flag.Parse()

	if entropyReseedMode != requireMode && entropyReseedMode != "off" {
		fmt.Fprintf(os.Stderr, "node-agent: invalid --entropy-reseed %q (want \"require\" or \"off\")\n", entropyReseedMode)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "setec node-agent starting on node=%q\n", nodeName)
	if _, err := os.Stat(kvmDevicePath); errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(os.Stderr, "KVM device %q is missing; node cannot host Sandboxes. Exiting.\n", kvmDevicePath)
		os.Exit(1)
	}

	reg := prometheus.NewRegistry()
	go serveMetrics(metricsAddr, reg)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if grpcListenAddr == "" {
		fmt.Fprintln(os.Stderr, "node-agent: gRPC server disabled (--grpc-listen-addr empty)")
		<-ctx.Done()
		return
	}
	for _, dir := range []string{snapshotRoot, snapshotDEKDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			fmt.Fprintf(os.Stderr, "node-agent: mkdir %q: %v\n", dir, err)
			os.Exit(1)
		}
	}
	// Encryption at rest is unconditional (docs/design/isolation.md invariant 5):
	// the encrypted wrapper is the only backend ever wired.
	backend := &storage.EncryptedBackend{
		Inner: &storage.LocalDiskBackend{Root: snapshotRoot, FillThreshold: snapshotFillFraction},
		KEK:   &storage.FileKEKSource{Path: snapshotKeyFile},
		DEKs:  &storage.DirDEKStore{Dir: snapshotDEKDir},
	}
	sessionStorage := s3SessionStorage(ctx, s3Config{
		endpoint: s3Endpoint, bucket: s3Bucket, region: s3Region, prefix: s3Prefix,
		pathStyle: s3PathStyle, staleWindow: staleMultipartWindow,
	})
	srv := &grpcserver.Server{
		Storage:            backend,
		SessionStorage:     sessionStorage,
		FirecrackerFactory: func(sock string) firecracker.Client { return firecracker.NewClientFromSocket(sock) },
		Machines:           launchersandbox.Resolver{PodsDir: kubeletPodsDir},
		EntropyReseedOff:   entropyReseedMode != requireMode,
	}
	if srv.EntropyReseedOff {
		fmt.Fprintln(os.Stderr, "node-agent: entropy reseed on restore DISABLED (--entropy-reseed=off); "+
			"the operator gate refuses each restore")
	}
	go serveGRPC(ctx, grpcListenAddr, srv, grpcTLS(ctx, creds))
	<-ctx.Done()
	fmt.Fprintln(os.Stderr, "node-agent: shutdown signal received, exiting cleanly")
}

// s3Config is the S3-compatible store of session checkpoints.
type s3Config struct {
	endpoint, bucket, region, prefix string
	pathStyle                        bool
	staleWindow                      time.Duration
}

// s3SessionStorage builds the session-checkpoint backend (setec#194,
// docs/design/storage.md), or nil when no bucket is set. Checkpoints are
// node-independent, so a session can resume on another node; their DEKs
// are sealed with the per-session KEK that the operator forwards per call.
func s3SessionStorage(ctx context.Context, c s3Config) func(kek []byte) storage.StorageBackend {
	if c.bucket == "" {
		fmt.Fprintln(os.Stderr, "node-agent: s3 session-checkpoint backend disabled (--s3-bucket empty)")
		return nil
	}
	s3Backend, err := storage.NewS3Backend(ctx, storage.S3Config{
		Endpoint: c.endpoint, Bucket: c.bucket, Region: c.region, Prefix: c.prefix, UsePathStyle: c.pathStyle,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "node-agent: build s3 checkpoint backend: %v\n", err)
		os.Exit(1)
	}
	dekStore := s3Backend.DEKStore()
	fmt.Fprintf(os.Stderr, "node-agent: s3 session-checkpoint backend enabled (bucket=%q endpoint=%q)\n",
		c.bucket, c.endpoint)
	// Sweep multipart uploads orphaned by a previous life of this agent
	// (setec#297). Best effort: a store that denies the list must not stop
	// the agent.
	if aborted, err := s3Backend.AbortStaleMultipartUploads(ctx, c.staleWindow); err != nil {
		fmt.Fprintf(os.Stderr, "node-agent: sweep stale multipart uploads: %v\n", err)
	} else if aborted > 0 {
		fmt.Fprintf(os.Stderr, "node-agent: aborted %d stale multipart upload(s) older than %s\n", aborted, c.staleWindow)
	}
	return func(kek []byte) storage.StorageBackend {
		return &storage.EncryptedBackend{Inner: s3Backend, KEK: storage.StaticKEKSource(kek), DEKs: dekStore}
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

// serveGRPC binds a TCP listener, registers the NodeAgentService
// implementation, and blocks until the supplied context is canceled.
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
