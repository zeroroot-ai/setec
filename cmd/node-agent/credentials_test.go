// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package main

import (
	"context"
	"crypto/tls"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	grpccreds "google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	"github.com/zeroroot-ai/setec/internal/credentials/spiffetest"
)

// The node-agent serves the operator over mTLS. Its credentials come
// from the SPIFFE Workload API, and each caller must hold an authorized
// SPIFFE ID. These tests run the flag-handling path of the binary
// against a fake Workload API. Every refusal is paired with the
// acceptance case it is measured against.

const (
	nodeAgentID = "spiffe://example.org/ns/setec/sa/setec-node-agent"
	callerID    = "spiffe://example.org/ns/setec/sa/setec"
)

func TestNodeAgentListener_AcceptsAnAuthorizedPeer(t *testing.T) {
	t.Parallel()
	ca := spiffetest.NewCA(t)
	addr := serveNodeAgent(t, ca)
	if err := dialHealth(t, addr, peer(t, ca, callerID, ca)); err != nil {
		t.Fatalf("handshake from an authorized SPIFFE ID: %v", err)
	}
}

func TestNodeAgentListener_Refuses(t *testing.T) {
	t.Parallel()
	ca := spiffetest.NewCA(t)
	foreign := spiffetest.NewCA(t)
	addr := serveNodeAgent(t, ca)

	leaf, key := ca.IssueSVID(t, callerID)
	legacy := grpccreds.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{leaf.Raw}, PrivateKey: key, Leaf: leaf}},
		MinVersion:   tls.VersionTLS12,
		MaxVersion:   tls.VersionTLS12,
		RootCAs:      ca.Pool(),
	})
	tests := map[string]grpccreds.TransportCredentials{
		// Same trust domain and CA, another workload.
		"an unauthorized SPIFFE ID": peer(t, ca, "spiffe://example.org/ns/setec/sa/other", ca),
		// The same path under another trust domain.
		"a foreign trust domain": peer(t, ca, "spiffe://evil.example/ns/setec/sa/setec", ca),
		// The authorized ID, from an issuer the node-agent does not trust.
		"an authorized ID from an untrusted CA": peer(t, foreign, callerID, ca),
		// No client certificate at all.
		"a peer with no certificate": grpccreds.NewTLS(&tls.Config{MinVersion: tls.VersionTLS13, RootCAs: ca.Pool()}),
		// An acceptable peer, capped at TLS 1.2.
		"a peer below TLS 1.3": legacy,
		"a plaintext peer":     insecure.NewCredentials(),
	}
	for name, creds := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if err := dialHealth(t, addr, creds); err == nil {
				t.Fatalf("handshake from %s: want refusal, got success", name)
			}
		})
	}
}

// TestServerCredentials_UnreachableSocketIsABootFailure pins the boot
// failure: a node-agent with no Workload API never serves.
func TestServerCredentials_UnreachableSocketIsABootFailure(t *testing.T) {
	t.Parallel()
	// The source waits 30s for an absent agent when the caller sets no
	// deadline of its own.
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	dir, err := os.MkdirTemp("", "setec-na")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	_, err = serverCredentials(ctx, credentialFlags{
		spiffeSocket:        "unix://" + filepath.Join(dir, "absent.sock"),
		spiffeAuthorizedIDs: repeatedString{callerID},
	})
	if err == nil || !strings.Contains(err.Error(), "absent.sock") {
		t.Fatalf("serverCredentials with an unreachable Workload API = %v, want an error that names the socket", err)
	}
}

// TestServerCredentials_RefusesAnIncompleteSource covers what an operator
// can leave out: the socket or the allow-list.
func TestServerCredentials_RefusesAnIncompleteSource(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		flags   credentialFlags
		wantErr string
	}{
		"no socket":     {credentialFlags{spiffeAuthorizedIDs: repeatedString{callerID}}, "socket path is empty"},
		"no allow-list": {credentialFlags{spiffeSocket: "unix:///run/spire/api.sock"}, "allow-list is empty"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := serverCredentials(t.Context(), tc.flags)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("serverCredentials = %v, want an error that mentions %q", err, tc.wantErr)
			}
		})
	}
}

// TestRepeatedString_CollectsEveryOccurrence pins the allow-list flag
// being repeatable. Keeping only the last occurrence would silently
// narrow the allow-list to one caller.
func TestRepeatedString_CollectsEveryOccurrence(t *testing.T) {
	t.Parallel()
	var ids repeatedString
	for _, id := range []string{callerID, "spiffe://example.org/ns/setec/sa/second"} {
		if err := ids.Set(id); err != nil {
			t.Fatalf("Set(%q): %v", id, err)
		}
	}
	src := credentialFlags{spiffeSocket: "unix:///s", spiffeAuthorizedIDs: ids}.source()
	if len(src.AuthorizedIDs) != 2 || src.SocketPath != "unix:///s" {
		t.Fatalf("source = %+v, want the socket and both IDs", src)
	}
}

// serveNodeAgent starts a gRPC health server with the credentials of the
// node-agent's own flag path. Its SVID and the trust bundle come from ca.
func serveNodeAgent(t *testing.T, ca *spiffetest.CA) string {
	t.Helper()
	api := spiffetest.Start(t, ca, nodeAgentID, ca)
	opt, err := serverCredentials(t.Context(), credentialFlags{
		spiffeSocket:        api.Addr,
		spiffeAuthorizedIDs: repeatedString{callerID},
	})
	if err != nil {
		t.Fatalf("serverCredentials: %v", err)
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer(opt)
	healthpb.RegisterHealthServer(srv, health.NewServer())
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return lis.Addr().String()
}

// peer builds client credentials that present an SVID for id from
// identityCA and trust servers from trustCA. It does not use the
// credentials package: most of these peers are ones it would never
// build.
func peer(t *testing.T, identityCA *spiffetest.CA, id string, trustCA *spiffetest.CA) grpccreds.TransportCredentials {
	t.Helper()
	leaf, key := identityCA.IssueSVID(t, id)
	return grpccreds.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{leaf.Raw}, PrivateKey: key, Leaf: leaf}},
		MinVersion:   tls.VersionTLS13,
		RootCAs:      trustCA.Pool(),
	})
}

// dialHealth performs one Check RPC and returns the resulting error.
// The gRPC handshake is lazy, so the RPC is what forces it.
func dialHealth(t *testing.T, addr string, creds grpccreds.TransportCredentials) error {
	t.Helper()
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(creds))
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	_, err = healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{}, grpc.WaitForReady(false))
	return err
}
