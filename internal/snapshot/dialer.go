// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package snapshot

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"

	"google.golang.org/grpc"
	grpccreds "google.golang.org/grpc/credentials"
	corev1 "k8s.io/api/core/v1"

	setecgrpcv1 "github.com/zeroroot-ai/setec/api/grpc/v1"
)

// GRPCDialer is the production NodeAgentDialer that opens mTLS
// connections to node-agent pods. It maintains a per-node connection
// cache so repeated reconciles reuse the same connection.
//
// The operator resolves the node-agent Pod on the target node through
// the Kubernetes API (Resolver) and dials that Pod's IP directly
// (setec#92). No DNS name makes a headless Service resolve to a
// single node-agent Pod. That needs the Pod to set hostname and
// subdomain, and the node-agent DaemonSet sets neither, so dialing by
// name has never worked.
//
// The dial target changes to the Pod IP. The gRPC :authority (and so
// the TLS ServerName used to verify the node-agent's certificate)
// still comes from AuthorityPattern, for example
// "<node>.<fullname>-node-agent.<namespace>.svc:50052". That string
// matches the wildcard SAN the chart's node-agent Certificate already
// carries, so verification keeps working even though DNS never looks
// the string up.
type GRPCDialer struct {
	// Resolver locates the node-agent Pod to dial for a given node.
	Resolver NodeAgentPodResolver

	// AuthorityPattern is a format string. It renders the gRPC
	// authority (and TLS ServerName) from a node name, for example
	// "%s.setec-node-agent.setec-system.svc:50052". %s is substituted
	// with the node name. DNS never resolves this string. gRPC sends
	// it as the :authority, and Go's TLS stack verifies the peer
	// certificate against it, but it is never a dial address.
	AuthorityPattern string

	// Credentials are the client transport credentials used for
	// mTLS. They are required. The operator-to-node-agent channel is
	// always mTLS. They come from internal/credentials, which decides
	// where the operator's identity comes from and which node-agent
	// it accepts. The dialer only carries them to grpc.NewClient.
	Credentials grpccreds.TransportCredentials

	mu    sync.Mutex
	conns map[string]cachedConn
}

// cachedConn pairs a connection with the identity of the Pod it was
// dialed to. This lets a restarted node-agent (new Pod UID, new Pod
// IP) get detected, instead of a stale connection that still accepts
// TCP but talks to a Pod that no longer exists.
type cachedConn struct {
	podKey string
	conn   *grpc.ClientConn
}

// NewGRPCDialer constructs a GRPCDialer with the provided pod
// resolver, authority pattern, and client credentials. The connection
// cache is lazily populated.
func NewGRPCDialer(resolver NodeAgentPodResolver, authorityPattern string, creds grpccreds.TransportCredentials) *GRPCDialer {
	return &GRPCDialer{
		Resolver:         resolver,
		AuthorityPattern: authorityPattern,
		Credentials:      creds,
		conns:            map[string]cachedConn{},
	}
}

// Dial returns a NodeAgentClient bound to a connection to the node-
// agent Pod currently running on the given node.
func (d *GRPCDialer) Dial(ctx context.Context, nodeName string) (NodeAgentClient, error) {
	if nodeName == "" {
		return nil, errors.New("grpcdialer: nodeName is required")
	}
	if d.Credentials == nil {
		return nil, errors.New("grpcdialer: Credentials are required. mTLS is mandatory")
	}
	if d.Resolver == nil {
		return nil, errors.New("grpcdialer: Resolver is required")
	}

	pod, err := d.Resolver.ResolveNodeAgentPod(ctx, nodeName)
	if err != nil {
		return nil, fmt.Errorf("grpcdialer: resolve node-agent pod on node %q: %w", nodeName, err)
	}
	if pod.Status.PodIP == "" {
		return nil, fmt.Errorf("grpcdialer: node-agent pod %s/%s on node %q has no PodIP",
			pod.Namespace, pod.Name, nodeName)
	}
	podKey := connCacheKey(pod)

	d.mu.Lock()
	defer d.mu.Unlock()

	if cached, ok := d.conns[nodeName]; ok {
		if cached.podKey == podKey {
			return wrapGRPC(cached.conn), nil
		}
		// The node-agent on this node restarted. A new Pod, with a
		// new UID and likely a new IP, now serves it. The cached
		// connection dials a Pod that is gone or about to be, so drop
		// it instead of reusing it.
		_ = cached.conn.Close()
		delete(d.conns, nodeName)
	}

	authority := fmt.Sprintf(d.AuthorityPattern, nodeName)
	// Only the authority's port gets dialed. Its host is never
	// resolved (see the type doc above). Below, the dialer reuses
	// that host as the gRPC :authority and TLS ServerName only.
	_, port, err := net.SplitHostPort(authority)
	if err != nil {
		return nil, fmt.Errorf("grpcdialer: authority %q (from pattern %q) is not host:port: %w",
			authority, d.AuthorityPattern, err)
	}
	target := net.JoinHostPort(pod.Status.PodIP, port)

	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(d.Credentials),
		// WithAuthority keeps the gRPC :authority, and so the TLS
		// ServerName the node-agent's certificate is verified
		// against, as the per-node name the chart's Certificate SAN
		// wildcards. This holds even though the dial target below is
		// a Pod IP.
		grpc.WithAuthority(authority),
	}

	conn, err := grpc.NewClient(target, opts...)
	if err != nil {
		return nil, fmt.Errorf("grpcdialer: dial %q (authority %q): %w", target, authority, err)
	}
	d.conns[nodeName] = cachedConn{podKey: podKey, conn: conn}
	return wrapGRPC(conn), nil
}

// connCacheKey identifies the Pod instance a connection was dialed to.
// It uses the Pod UID when set. This is always true in production, but
// a bare corev1.Pod built by a test may leave it empty, so it falls
// back to the Pod IP. This way a resolver that does not stamp UIDs
// still gets restart detection.
func connCacheKey(pod *corev1.Pod) string {
	if pod.UID != "" {
		return string(pod.UID)
	}
	return pod.Status.PodIP
}

// grpcShim adapts the generated setecv1grpc.NodeAgentServiceClient
// (which takes variadic grpc.CallOption) to the narrower
// snapshot.NodeAgentClient used by the Coordinator. The adapter drops
// the options, since Phase 3 callers never set them, which is the
// cleanest way to keep the Coordinator testable without importing
// gRPC.
type grpcShim struct {
	inner setecgrpcv1.NodeAgentServiceClient
}

func wrapGRPC(conn *grpc.ClientConn) NodeAgentClient {
	return &grpcShim{inner: setecgrpcv1.NewNodeAgentServiceClient(conn)}
}

func (s *grpcShim) CreateSnapshot(ctx context.Context, in *setecgrpcv1.CreateSnapshotRequest) (*setecgrpcv1.CreateSnapshotResponse, error) {
	return s.inner.CreateSnapshot(ctx, in)
}
func (s *grpcShim) RestoreSandbox(ctx context.Context, in *setecgrpcv1.RestoreSandboxRequest) (*setecgrpcv1.RestoreSandboxResponse, error) {
	return s.inner.RestoreSandbox(ctx, in)
}
func (s *grpcShim) PauseSandbox(ctx context.Context, in *setecgrpcv1.PauseSandboxRequest) (*setecgrpcv1.PauseSandboxResponse, error) {
	return s.inner.PauseSandbox(ctx, in)
}
func (s *grpcShim) ResumeSandbox(ctx context.Context, in *setecgrpcv1.ResumeSandboxRequest) (*setecgrpcv1.ResumeSandboxResponse, error) {
	return s.inner.ResumeSandbox(ctx, in)
}
func (s *grpcShim) DeleteSnapshot(ctx context.Context, in *setecgrpcv1.DeleteSnapshotRequest) (*setecgrpcv1.DeleteSnapshotResponse, error) {
	return s.inner.DeleteSnapshot(ctx, in)
}

// Close tears down every cached connection. It is safe to call more
// than once. It returns the first error encountered.
func (d *GRPCDialer) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	var firstErr error
	for _, c := range d.conns {
		if err := c.conn.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	d.conns = map[string]cachedConn{}
	return firstErr
}
