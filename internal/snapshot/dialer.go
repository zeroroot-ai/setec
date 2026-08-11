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

package snapshot

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"google.golang.org/grpc"
	grpccreds "google.golang.org/grpc/credentials"

	setecgrpcv1 "github.com/zeroroot-ai/setec/api/grpc/v1"
)

// GRPCDialer is the production NodeAgentDialer that opens mTLS
// connections to node-agent pods. It maintains a per-node connection
// cache so repeated reconciles reuse the same connection.
//
// Endpoint resolution is intentionally simple: the operator computes
// <nodeName>.<service>.<namespace>.svc.cluster.local:<port> so the
// Kubernetes DNS record produced by a headless Service selecting the
// node-agent DaemonSet routes to the right pod. The chart ships such
// a headless Service.
type GRPCDialer struct {
	// EndpointPattern is a format string rendering a dial target from
	// a node name (e.g. "%s.setec-node-agent.setec-system.svc:50052").
	EndpointPattern string

	// Credentials are the client transport credentials used for
	// mTLS. Required — the operator-to-node-agent channel is always
	// mTLS. They come from internal/credentials, which is what
	// decides where the operator's identity is sourced from and
	// whose node-agent it is willing to talk to; the dialer only
	// carries them to grpc.NewClient.
	Credentials grpccreds.TransportCredentials

	mu    sync.Mutex
	conns map[string]*grpc.ClientConn
}

// NewGRPCDialer constructs a GRPCDialer with the provided endpoint
// pattern and client credentials. The connection cache is lazily
// populated.
func NewGRPCDialer(pattern string, creds grpccreds.TransportCredentials) *GRPCDialer {
	return &GRPCDialer{
		EndpointPattern: pattern,
		Credentials:     creds,
		conns:           map[string]*grpc.ClientConn{},
	}
}

// Dial returns a NodeAgentClient bound to a connection to the given
// node.
func (d *GRPCDialer) Dial(_ context.Context, nodeName string) (NodeAgentClient, error) {
	if nodeName == "" {
		return nil, errors.New("grpcdialer: nodeName is required")
	}
	d.mu.Lock()
	defer d.mu.Unlock()

	if conn, ok := d.conns[nodeName]; ok {
		return wrapGRPC(conn), nil
	}

	target := fmt.Sprintf(d.EndpointPattern, nodeName)

	if d.Credentials == nil {
		return nil, errors.New("grpcdialer: Credentials are required; mTLS is mandatory")
	}
	opts := []grpc.DialOption{grpc.WithTransportCredentials(d.Credentials)}

	conn, err := grpc.NewClient(target, opts...)
	if err != nil {
		return nil, fmt.Errorf("grpcdialer: dial %q: %w", target, err)
	}
	d.conns[nodeName] = conn
	return wrapGRPC(conn), nil
}

// grpcShim adapts the generated setecv1grpc.NodeAgentServiceClient
// (which takes variadic grpc.CallOption) to the narrower
// snapshot.NodeAgentClient used by the Coordinator. The adapter drops
// the options — Phase 3 callers never set them — and is the cleanest
// way to keep the Coordinator testable without importing gRPC.
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
func (s *grpcShim) QueryPool(ctx context.Context, in *setecgrpcv1.QueryPoolRequest) (*setecgrpcv1.QueryPoolResponse, error) {
	return s.inner.QueryPool(ctx, in)
}
func (s *grpcShim) ClaimPoolEntry(ctx context.Context, in *setecgrpcv1.ClaimPoolEntryRequest) (*setecgrpcv1.ClaimPoolEntryResponse, error) {
	return s.inner.ClaimPoolEntry(ctx, in)
}
func (s *grpcShim) DeleteSnapshot(ctx context.Context, in *setecgrpcv1.DeleteSnapshotRequest) (*setecgrpcv1.DeleteSnapshotResponse, error) {
	return s.inner.DeleteSnapshot(ctx, in)
}

// Close tears down every cached connection. Safe to call multiple
// times; returns the first error encountered.
func (d *GRPCDialer) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	var firstErr error
	for _, c := range d.conns {
		if err := c.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	d.conns = map[string]*grpc.ClientConn{}
	return firstErr
}
