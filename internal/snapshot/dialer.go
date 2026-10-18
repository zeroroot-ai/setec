// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package snapshot

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"syscall"
	"time"

	"google.golang.org/grpc"
	grpccodes "google.golang.org/grpc/codes"
	"google.golang.org/grpc/connectivity"
	grpccreds "google.golang.org/grpc/credentials"
	grpcstatus "google.golang.org/grpc/status"
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

	// ReadyWait bounds the wait for the connection before each call
	// (nodeAgentReadyWait by default). 0 turns the wait off.
	ReadyWait time.Duration

	// Credentials are the client transport credentials used for
	// mTLS. They are required. The operator-to-node-agent channel is
	// always mTLS. They come from internal/credentials, which decides
	// where the operator's identity comes from and which node-agent
	// it accepts. The dialer only carries them to grpc.NewClient.
	Credentials grpccreds.TransportCredentials

	mu    sync.Mutex
	conns map[string]cachedConn
}

// nodeAgentReadyWait bounds how long a call waits for the connection to a
// node-agent to come up before it is sent. A node-agent container that
// restarts refuses connections for some seconds, and the Pod keeps its
// UID and IP, so the cached connection only has to reconnect. The kubelet
// restarts a container at once, and again after 10 seconds, so 15 seconds
// covers two restarts. Each controller has one worker, so the wait stays
// short: a node-agent that stays down holds the worker for 15 seconds a
// call, not longer. Without the
// wait, a call in that gap failed at once with Unavailable, and the
// coordinator marked a snapshot Failed that a retry seconds later would
// have taken (setec#313). The wait covers only the connection: a call
// that reached the node-agent is never sent again.
const nodeAgentReadyWait = 15 * time.Second

// cachedConn pairs a connection with the identity of the Pod it was
// dialed to. This lets a restarted node-agent (new Pod UID, new Pod
// IP) get detected, instead of a stale connection that still accepts
// TCP but talks to a Pod that no longer exists.
type cachedConn struct {
	podKey    string
	conn      *grpc.ClientConn
	handshake *handshakeRecorder
}

// NewGRPCDialer constructs a GRPCDialer with the provided pod
// resolver, authority pattern, and client credentials. The connection
// cache is lazily populated.
func NewGRPCDialer(resolver NodeAgentPodResolver, authorityPattern string, creds grpccreds.TransportCredentials) *GRPCDialer {
	return &GRPCDialer{
		Resolver:         resolver,
		AuthorityPattern: authorityPattern,
		Credentials:      creds,
		ReadyWait:        nodeAgentReadyWait,
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
		return nil, fmt.Errorf("grpcdialer: node-agent pod %s/%s on node %q has no PodIP: %w",
			pod.Namespace, pod.Name, nodeName, ErrNodeAgentUnavailable)
	}
	podKey := connCacheKey(pod)

	d.mu.Lock()
	defer d.mu.Unlock()

	if cached, ok := d.conns[nodeName]; ok {
		if cached.podKey == podKey {
			return wrapGRPC(cached.conn, d.ReadyWait, cached.handshake), nil
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

	handshake := &handshakeRecorder{TransportCredentials: d.Credentials, last: &lastError{}}
	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(handshake),
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
	d.conns[nodeName] = cachedConn{podKey: podKey, conn: conn, handshake: handshake}
	return wrapGRPC(conn, d.ReadyWait, handshake), nil
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

// lastError holds one error, safe for use by more than one goroutine.
type lastError struct {
	mu  sync.Mutex
	err error
}

func (l *lastError) set(err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.err = err
}

func (l *lastError) get() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.err
}

// handshakeRecorder wraps the transport credentials of one connection and
// keeps its last TLS fault (see tlsFault). A connection that never gets
// Ready because of a TLS fault (an untrusted CA, a wrong SPIFFE ID, a peer
// that is not TLS or sends bad TLS) is a fault of the configuration, not a
// short outage. The shim reads the kept fault to tell the two apart, and to
// give the cause (setec#315).
type handshakeRecorder struct {
	grpccreds.TransportCredentials

	last *lastError
}

// ClientHandshake runs the handshake of the wrapped credentials and keeps
// its error when it is a TLS fault. Only the first read that succeeds on a
// connection clears a kept fault (see alertConn). A refused dial or a
// handshake cut short keeps it. So a node agent that had a TLS fault and
// then restarts in place reads as that fault until a connection to it
// works. That is the safe side: a reset after an alert must not hide the
// fault.
func (h *handshakeRecorder) ClientHandshake(
	ctx context.Context, authority string, raw net.Conn,
) (net.Conn, grpccreds.AuthInfo, error) {
	conn, info, err := h.TransportCredentials.ClientHandshake(ctx, authority, raw)
	if err != nil {
		// Only a TLS fault is kept. A handshake cut short by the network
		// (a reset, an EOF, a timeout) is an outage, and keeps what was
		// there before.
		if tlsFault(err) {
			h.last.set(err)
		}
		return conn, info, err //nolint:wrapcheck // gRPC reads the handshake error of its credentials as is
	}
	// A kept error is cleared by the first read that succeeds, not here:
	// in TLS 1.3 a refusal of the client certificate comes after this
	// point (see alertConn).
	return &alertConn{Conn: conn, last: h.last}, info, nil
}

// tlsFault reports whether err of a TLS handshake is a fault of TLS: a
// certificate that does not verify, a peer that is not TLS, or an alert of
// the peer. Errors of the network or of the ctx are not.
func tlsFault(err error) bool {
	if op, ok := errors.AsType[*net.OpError](err); ok {
		// An alert of the peer, or an alert that this side sends because
		// the peer sent bad TLS ("unexpected message", "decode error").
		// Other operations ("read", "write", "dial") are of the network.
		return op.Op == remoteErrorOp || op.Op == localErrorOp
	}
	for _, transient := range []error{
		io.EOF, io.ErrUnexpectedEOF, net.ErrClosed, os.ErrDeadlineExceeded,
		context.Canceled, context.DeadlineExceeded, syscall.ECONNRESET, syscall.EPIPE,
	} {
		if errors.Is(err, transient) {
			return false
		}
	}
	return true
}

// remoteErrorOp is the operation of the net.OpError that crypto/tls returns
// for an alert of the peer, for example "remote error: tls: unknown
// certificate authority". It is not part of the API of crypto/tls.
// TestGRPCDialer_IsRefusedWhenItCannotProveWhoItIs fails if it changes.
const remoteErrorOp = "remote error"

// localErrorOp is the operation of the net.OpError that crypto/tls returns
// when this side sends an alert, for example "local error: tls: unexpected
// message". It is not part of the API of crypto/tls.
// TestGRPCDialer_RefusesAPeerThatSendsBadTLS fails if it changes.
const localErrorOp = "local error"

// alertConn keeps a TLS alert that the peer sends after the handshake. In
// TLS 1.3 the client ends its handshake before the server checks the client
// certificate, so a node agent that refuses the identity of the operator
// says so with an alert on the first read. The recorder keeps that alert
// like a failed handshake. The first read that succeeds clears the kept
// error. The wrap hides syscall.Conn of the TLS connection, so channelz
// shows no socket options for it. gRPC needs nothing else from it.
type alertConn struct {
	net.Conn

	last    *lastError
	cleared bool
}

func (c *alertConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	op, isOp := errors.AsType[*net.OpError](err)
	switch {
	case isOp && op.Op == remoteErrorOp:
		c.last.set(err)
	case err == nil && n > 0 && !c.cleared:
		c.cleared = true
		c.last.set(nil)
	}
	return n, err //nolint:wrapcheck // the reader of a net.Conn reads io.EOF and its peers as is
}

// Clone keeps the recorder: gRPC may clone the credentials of a
// connection, and the clone must report to the same place.
func (h *handshakeRecorder) Clone() grpccreds.TransportCredentials {
	return &handshakeRecorder{TransportCredentials: h.TransportCredentials.Clone(), last: h.last}
}

// grpcShim adapts the generated setecv1grpc.NodeAgentServiceClient
// (which takes variadic grpc.CallOption) to the narrower
// snapshot.NodeAgentClient used by the Coordinator. The Coordinator
// stays testable without importing gRPC. Before each call the shim waits,
// for at most readyWait, until the connection is up (see
// nodeAgentReadyWait). When the connection is still down after the wait,
// the shim does not send the call.
type grpcShim struct {
	conn      *grpc.ClientConn
	inner     setecgrpcv1.NodeAgentServiceClient
	readyWait time.Duration
	handshake *handshakeRecorder
}

func wrapGRPC(conn *grpc.ClientConn, readyWait time.Duration, handshake *handshakeRecorder) NodeAgentClient {
	return &grpcShim{
		conn: conn, inner: setecgrpcv1.NewNodeAgentServiceClient(conn),
		readyWait: readyWait, handshake: handshake,
	}
}

// ready waits until the connection is Ready or the wait ends. When ctx
// has a deadline, the wait takes at most half of the time left. With no
// wait (readyWait 0) it returns nil at once and the call goes out as gRPC
// decides. When the connection is not Ready after the wait, the call is not
// sent, and ready returns:
//   - the error of ctx, when ctx ended,
//   - an Unavailable status with the cause, when the last TLS handshake
//     failed: a fault of the configuration, which a retry does not mend,
//   - else an error that wraps ErrNodeAgentUnavailable and an Unavailable
//     status: the node agent got no call, so a caller may send it again.
func (s *grpcShim) ready(ctx context.Context) error {
	if s.readyWait <= 0 {
		return nil
	}
	st := s.conn.GetState()
	if st == connectivity.Ready {
		return nil
	}
	wait := s.readyWait
	if dl, ok := ctx.Deadline(); ok {
		wait = max(min(wait, time.Until(dl)/2), 0)
	}
	if wait > 0 {
		wctx, cancel := context.WithTimeout(ctx, wait)
		defer cancel()
		s.conn.Connect()
		for st = s.conn.GetState(); st != connectivity.Ready; st = s.conn.GetState() {
			if st == connectivity.Shutdown || !s.conn.WaitForStateChange(wctx, st) {
				break
			}
		}
	}
	switch {
	case st == connectivity.Ready:
		return nil
	case ctx.Err() != nil:
		return fmt.Errorf("grpcdialer: wait for the node agent: %w", ctx.Err())
	}
	if s.handshake != nil {
		if hsErr := s.handshake.last.get(); hsErr != nil {
			return grpcstatus.Error(grpccodes.Unavailable,
				"grpcdialer: the TLS handshake with the node agent failed: "+hsErr.Error())
		}
	}
	return fmt.Errorf("%w: %w", ErrNodeAgentUnavailable, grpcstatus.Errorf(grpccodes.Unavailable,
		"grpcdialer: the connection to the node agent is %s after a wait of %s; the call was not sent", st, wait))
}

func (s *grpcShim) CreateSnapshot(ctx context.Context, in *setecgrpcv1.CreateSnapshotRequest) (*setecgrpcv1.CreateSnapshotResponse, error) {
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	return s.inner.CreateSnapshot(ctx, in)
}
func (s *grpcShim) RestoreSandbox(ctx context.Context, in *setecgrpcv1.RestoreSandboxRequest) (*setecgrpcv1.RestoreSandboxResponse, error) {
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	return s.inner.RestoreSandbox(ctx, in)
}
func (s *grpcShim) PauseSandbox(ctx context.Context, in *setecgrpcv1.PauseSandboxRequest) (*setecgrpcv1.PauseSandboxResponse, error) {
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	return s.inner.PauseSandbox(ctx, in)
}
func (s *grpcShim) ResumeSandbox(ctx context.Context, in *setecgrpcv1.ResumeSandboxRequest) (*setecgrpcv1.ResumeSandboxResponse, error) {
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	return s.inner.ResumeSandbox(ctx, in)
}
func (s *grpcShim) DeleteSnapshot(ctx context.Context, in *setecgrpcv1.DeleteSnapshotRequest) (*setecgrpcv1.DeleteSnapshotResponse, error) {
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
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
