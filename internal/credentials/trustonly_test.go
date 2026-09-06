// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package credentials_test

import (
	"crypto/tls"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc"
	grpccreds "google.golang.org/grpc/credentials"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	"github.com/zeroroot-ai/setec/internal/credentials"
)

// The one-way surface authenticates the peer and nothing else. Its
// tests therefore have to prove two opposite things: that it really
// does verify the peer (so it is not "TLS in name only"), and that it
// really does present no identity (so nobody mistakes it for mTLS).
//
// The sharpest pairing available here is one unchanged server dialled
// under two configurations: refused against the host root store,
// accepted against the bundle that signed it. Neither outcome is
// reachable by accident.

func TestTrustOnlyCredentials_RefusesServerNotInTheHostRootStore(t *testing.T) {
	t.Parallel()
	ca := newCA(t)
	addr := serveTrustOnly(t, ca, tls.NoClientCert)

	// Empty CAFile means the host root store, which has never heard
	// of this throwaway CA.
	creds, err := credentials.TrustOnlyCredentials(credentials.TrustOnly{})
	if err != nil {
		t.Fatalf("TrustOnlyCredentials: %v", err)
	}
	if err := dialHealth(t, addr, creds); err == nil {
		t.Fatal("dial to a privately-signed server with host roots: want refusal, got success")
	}
}

func TestTrustOnlyCredentials_AcceptsServerInTheConfiguredBundle(t *testing.T) {
	t.Parallel()
	ca := newCA(t)
	addr := serveTrustOnly(t, ca, tls.NoClientCert)

	// The same server as the refusal case above, dialled with the
	// bundle that signed it.
	creds, err := credentials.TrustOnlyCredentials(credentials.TrustOnly{
		CAFile: ca.writeBundle(t, filepath.Join(t.TempDir(), "ca.pem")),
	})
	if err != nil {
		t.Fatalf("TrustOnlyCredentials: %v", err)
	}
	if err := dialHealth(t, addr, creds); err != nil {
		t.Fatalf("dial to a server in the configured bundle: %v", err)
	}
}

func TestTrustOnlyCredentials_RefusesServerOutsideTheConfiguredBundle(t *testing.T) {
	t.Parallel()
	ca := newCA(t)
	foreign := newCA(t)
	addr := serveTrustOnly(t, foreign, tls.NoClientCert)

	creds, err := credentials.TrustOnlyCredentials(credentials.TrustOnly{
		CAFile: ca.writeBundle(t, filepath.Join(t.TempDir(), "ca.pem")),
	})
	if err != nil {
		t.Fatalf("TrustOnlyCredentials: %v", err)
	}
	if err := dialHealth(t, addr, creds); err == nil {
		t.Fatal("dial to a server outside the configured bundle: want refusal, got success")
	}
}

// TestTrustOnlyCredentials_PresentsNoClientIdentity pins the honest
// limit of this surface. A peer that demands a client certificate gets
// nothing, because there is no identity to give. If this ever starts
// passing, something has quietly turned the telemetry hop into mTLS
// and the type's documentation has become a lie.
func TestTrustOnlyCredentials_PresentsNoClientIdentity(t *testing.T) {
	t.Parallel()
	ca := newCA(t)
	addr := serveTrustOnly(t, ca, tls.RequireAnyClientCert)

	creds, err := credentials.TrustOnlyCredentials(credentials.TrustOnly{
		CAFile: ca.writeBundle(t, filepath.Join(t.TempDir(), "ca.pem")),
	})
	if err != nil {
		t.Fatalf("TrustOnlyCredentials: %v", err)
	}
	if err := dialHealth(t, addr, creds); err == nil {
		t.Fatal("dial to a peer demanding a client certificate: want refusal, got success")
	}
}

// ---------------------------------------------------------------------
// Configuration errors. A bundle the operator asked for and that is not
// usable must fail loudly rather than widen silently to the host roots.
// ---------------------------------------------------------------------

func TestTrustOnlyCredentials_MissingCAFile(t *testing.T) {
	t.Parallel()
	_, err := credentials.TrustOnlyCredentials(credentials.TrustOnly{
		CAFile: filepath.Join(t.TempDir(), "absent-ca.pem"),
	})
	if err == nil {
		t.Fatal("want error for an absent CA file, got nil")
	}
	if !strings.Contains(err.Error(), "absent-ca.pem") {
		t.Fatalf("error = %q, want it to name the missing CA file", err)
	}
}

func TestTrustOnlyCredentials_CAFileIsNotPEM(t *testing.T) {
	t.Parallel()
	junk := filepath.Join(t.TempDir(), "junk-ca.pem")
	if err := os.WriteFile(junk, []byte("not a certificate\n"), 0o600); err != nil {
		t.Fatalf("write junk CA: %v", err)
	}
	_, err := credentials.TrustOnlyCredentials(credentials.TrustOnly{CAFile: junk})
	if err == nil {
		t.Fatal("want error for a CA file with no certificates, got nil")
	}
	if !strings.Contains(err.Error(), "junk-ca.pem") {
		t.Fatalf("error = %q, want it to name the unusable CA file", err)
	}
}

// ---------------------------------------------------------------------
// Helpers.
// ---------------------------------------------------------------------

// serveTrustOnly starts a TLS gRPC health server presenting an identity
// issued by identityCA and applying clientAuth to its callers. It does
// not go through Provider: the point of these tests is a peer that is
// not part of setec's mTLS mesh.
func serveTrustOnly(t *testing.T, identityCA *testCA, clientAuth tls.ClientAuthType) string {
	t.Helper()
	dir := t.TempDir()
	certPath, keyPath := identityCA.issue(t, dir, "collector", serverLeaf)
	keypair, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		t.Fatalf("load server keypair: %v", err)
	}
	creds := grpccreds.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{keypair},
		MinVersion:   tls.VersionTLS12,
		ClientAuth:   clientAuth,
	})

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer(grpc.Creds(creds))
	healthpb.RegisterHealthServer(srv, health.NewServer())
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return lis.Addr().String()
}
