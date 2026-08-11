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

package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
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
)

// The node-agent's gRPC surface is the operator's only way to drive a
// microVM on a node, and before this file it had no test at all. What
// is under test here is not that TLS works — internal/credentials owns
// that — but that the node-agent is wired to it, and that its refusal
// semantics are the frontend's. An operator must not be able to end up
// with one component demanding a verified client certificate and the
// other quietly not.
//
// Every refusal below is paired with the acceptance case it is measured
// against, so no assertion here is satisfiable by "nothing connected".

// ---------------------------------------------------------------------
// Flag translation. This is the silent-failure case: a typo in a flag
// name must not produce a listener with unintended credentials.
// ---------------------------------------------------------------------

func TestCredentialConfig_RejectsIncompleteFlags(t *testing.T) {
	t.Parallel()
	tests := map[string]credentialFlags{
		"no cert":      {keyPath: "k.pem", clientCAPath: "ca.pem"},
		"no key":       {certPath: "c.pem", clientCAPath: "ca.pem"},
		"no client ca": {certPath: "c.pem", keyPath: "k.pem"},
		"none at all":  {},
	}
	for name, flags := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := flags.credentialConfig()
			if err == nil {
				t.Fatalf("credentialConfig(%+v): want error, got nil", flags)
			}
			// The operator reading this message is looking at a
			// DaemonSet's argv, so the message names flags.
			for _, want := range []string{"--tls-cert", "--tls-key", "--tls-client-ca", "mTLS is mandatory"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error = %q, want it to mention %q", err, want)
				}
			}
		})
	}
}

func TestCredentialConfig_AcceptsCompleteFlags(t *testing.T) {
	t.Parallel()
	flags := credentialFlags{certPath: "c.pem", keyPath: "k.pem", clientCAPath: "ca.pem"}
	cfg, err := flags.credentialConfig()
	if err != nil {
		t.Fatalf("credentialConfig(%+v): %v", flags, err)
	}
	if cfg.Files == nil {
		t.Fatal("complete flags produced no file credential source")
	}
	if cfg.Files.CertFile != "c.pem" || cfg.Files.KeyFile != "k.pem" || cfg.Files.CAFile != "ca.pem" {
		t.Fatalf("flags reached the credential source scrambled: %+v", *cfg.Files)
	}
}

// ---------------------------------------------------------------------
// Credential acquisition. A bad mount is a boot failure whose message
// names the file, because the operator fixing it is looking at a
// volume, not at source.
// ---------------------------------------------------------------------

func TestServerCredentials_MissingKeypairNamesTheFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ca := newCA(t)

	_, err := serverCredentials(t.Context(), credentialFlags{
		certPath:     filepath.Join(dir, "absent.crt"),
		keyPath:      filepath.Join(dir, "absent.key"),
		clientCAPath: ca.writeBundle(t, filepath.Join(dir, "ca.pem")),
	})
	if err == nil {
		t.Fatal("want error for an absent keypair, got nil")
	}
	if !strings.Contains(err.Error(), "absent.crt") {
		t.Fatalf("error = %q, want it to name the missing certificate file", err)
	}
}

func TestServerCredentials_UnusableClientCANamesTheFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ca := newCA(t)
	certPath, keyPath := ca.issue(t, dir, "node-agent", serverLeaf)
	junk := filepath.Join(dir, "junk-ca.pem")
	if err := os.WriteFile(junk, []byte("not a certificate\n"), 0o600); err != nil {
		t.Fatalf("write junk CA: %v", err)
	}

	_, err := serverCredentials(t.Context(), credentialFlags{
		certPath: certPath, keyPath: keyPath, clientCAPath: junk,
	})
	if err == nil {
		t.Fatal("want error for a client-CA bundle with no certificates, got nil")
	}
	if !strings.Contains(err.Error(), "junk-ca.pem") {
		t.Fatalf("error = %q, want it to name the unusable CA file", err)
	}
}

// ---------------------------------------------------------------------
// What a peer observes on a node-agent listener. These are the
// posture-coherence assertions: the same four refusals the frontend
// makes, made here.
// ---------------------------------------------------------------------

func TestNodeAgentListener_AcceptsPeerFromTrustedCA(t *testing.T) {
	t.Parallel()
	ca := newCA(t)
	addr := serveNodeAgent(t, ca, ca)

	if err := dialHealth(t, addr, clientCredsFor(t, ca, ca)); err != nil {
		t.Fatalf("handshake with a peer from the trusted CA: %v", err)
	}
}

func TestNodeAgentListener_RefusesPeerFromUntrustedCA(t *testing.T) {
	t.Parallel()
	ca := newCA(t)
	foreign := newCA(t)
	addr := serveNodeAgent(t, ca, ca)

	// The client trusts the node-agent's CA, so only the client-auth
	// direction is under test.
	if err := dialHealth(t, addr, clientCredsFor(t, foreign, ca)); err == nil {
		t.Fatal("handshake with a peer from an untrusted CA: want refusal, got success")
	}
}

func TestNodeAgentListener_RefusesPeerWithNoCertificate(t *testing.T) {
	t.Parallel()
	ca := newCA(t)
	addr := serveNodeAgent(t, ca, ca)

	// No client certificate at all. This is the case that separates
	// RequireAndVerifyClientCert from VerifyClientCertIfGiven, and the
	// one that would let anything in the cluster drive a microVM.
	anonymous := grpccreds.NewTLS(&tls.Config{
		MinVersion: tls.VersionTLS13,
		RootCAs:    ca.pool(t),
	})
	if err := dialHealth(t, addr, anonymous); err == nil {
		t.Fatal("handshake with no client certificate: want refusal, got success")
	}
}

func TestNodeAgentListener_RefusesPeerBelowTLS13(t *testing.T) {
	t.Parallel()
	ca := newCA(t)
	addr := serveNodeAgent(t, ca, ca)

	dir := t.TempDir()
	certPath, keyPath := ca.issue(t, dir, "operator", clientLeaf)
	keypair, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		t.Fatalf("load client keypair: %v", err)
	}
	// A peer that is otherwise entirely acceptable, capped at TLS 1.2.
	// Refusing it is what pins the floor.
	legacy := grpccreds.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{keypair},
		MinVersion:   tls.VersionTLS12,
		MaxVersion:   tls.VersionTLS12,
		RootCAs:      ca.pool(t),
	})
	if err := dialHealth(t, addr, legacy); err == nil {
		t.Fatal("handshake capped at TLS 1.2: want refusal, got success")
	}
}

func TestNodeAgentListener_RefusesPlaintextPeer(t *testing.T) {
	t.Parallel()
	ca := newCA(t)
	addr := serveNodeAgent(t, ca, ca)

	if err := dialHealth(t, addr, insecure.NewCredentials()); err == nil {
		t.Fatal("plaintext dial against an mTLS listener: want refusal, got success")
	}
}

// ---------------------------------------------------------------------
// Helpers.
// ---------------------------------------------------------------------

// serveNodeAgent starts a gRPC server whose credentials come from the
// node-agent's own flag-handling path, presenting an identity issued by
// identityCA and verifying clients against trustCA. The registered
// service is irrelevant — the handshake is what is under test.
func serveNodeAgent(t *testing.T, identityCA, trustCA *testCA) string {
	t.Helper()
	dir := t.TempDir()
	certPath, keyPath := identityCA.issue(t, dir, "node-agent", serverLeaf)

	opt, err := serverCredentials(t.Context(), credentialFlags{
		certPath:     certPath,
		keyPath:      keyPath,
		clientCAPath: trustCA.writeBundle(t, filepath.Join(dir, "trust.pem")),
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

// clientCredsFor builds client credentials presenting an identity from
// identityCA and trusting servers from trustCA. It deliberately does
// not go through the credential module: several tests need a peer the
// module would refuse to build.
func clientCredsFor(t *testing.T, identityCA, trustCA *testCA) grpccreds.TransportCredentials {
	t.Helper()
	dir := t.TempDir()
	certPath, keyPath := identityCA.issue(t, dir, "operator", clientLeaf)
	keypair, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		t.Fatalf("load client keypair: %v", err)
	}
	return grpccreds.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{keypair},
		MinVersion:   tls.VersionTLS13,
		RootCAs:      trustCA.pool(t),
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
	_, err = healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{},
		grpc.WaitForReady(false))
	return err
}

// testCA is a throwaway certificate authority for one test.
type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	der  []byte
}

func newCA(t *testing.T) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("ca key: %v", err)
	}
	tpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "setec-node-agent-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("ca cert: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse ca: %v", err)
	}
	return &testCA{cert: cert, key: key, der: der}
}

type leafKind int

const (
	serverLeaf leafKind = iota
	clientLeaf
)

// issue writes a CA-signed leaf keypair into dir and returns the paths.
func (ca *testCA) issue(t *testing.T, dir, name string, kind leafKind) (certPath, keyPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("leaf key: %v", err)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: name},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	switch kind {
	case serverLeaf:
		tpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
		tpl.DNSNames = []string{"localhost"}
		tpl.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
	case clientLeaf:
		tpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatalf("leaf cert: %v", err)
	}

	certPath = filepath.Join(dir, name+".crt")
	keyPath = filepath.Join(dir, name+".key")
	writePEM(t, certPath, "CERTIFICATE", der)
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal leaf key: %v", err)
	}
	writePEM(t, keyPath, "EC PRIVATE KEY", keyDER)
	return certPath, keyPath
}

// writeBundle writes the CA certificate to path and returns path.
func (ca *testCA) writeBundle(t *testing.T, path string) string {
	t.Helper()
	writePEM(t, path, "CERTIFICATE", ca.der)
	return path
}

func (ca *testCA) pool(t *testing.T) *x509.CertPool {
	t.Helper()
	pool := x509.NewCertPool()
	pool.AddCert(ca.cert)
	return pool
}

func writePEM(t *testing.T, path, blockType string, der []byte) {
	t.Helper()
	buf := pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der})
	if err := os.WriteFile(path, buf, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
