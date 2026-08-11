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

package credentials_test

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

	"github.com/zeroroot-ai/setec/internal/credentials"
)

// ---------------------------------------------------------------------
// Configuration errors. These are the silent-failure cases: a component
// that starts with no credential source, or with half a source, must
// not reach a listener.
// ---------------------------------------------------------------------

func TestNew_NoSourceConfigured(t *testing.T) {
	t.Parallel()
	_, err := credentials.New(credentials.Config{})
	if err == nil {
		t.Fatal("New with no source: want error, got nil")
	}
	if !strings.Contains(err.Error(), "no credential source") {
		t.Fatalf("error = %q, want it to name the missing source", err)
	}
}

func TestNew_IncompleteFileSource(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		src      credentials.FileSource
		wantWord string
	}{
		"no cert": {credentials.FileSource{KeyFile: "k.pem", CAFile: "ca.pem"}, "certificate"},
		"no key":  {credentials.FileSource{CertFile: "c.pem", CAFile: "ca.pem"}, "key"},
		"no ca":   {credentials.FileSource{CertFile: "c.pem", KeyFile: "k.pem"}, "CA"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := credentials.New(credentials.Config{Files: &tc.src})
			if err == nil {
				t.Fatalf("New(%+v): want error, got nil", tc.src)
			}
			if !strings.Contains(err.Error(), tc.wantWord) {
				t.Fatalf("error = %q, want it to mention %q", err, tc.wantWord)
			}
		})
	}
}

// ---------------------------------------------------------------------
// File-acquisition errors. Every one names the offending path, because
// the operator fixing it is looking at a volume mount, not at source.
// ---------------------------------------------------------------------

func TestServerCredentials_MissingCertFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ca := newCA(t)
	ca.writeBundle(t, filepath.Join(dir, "ca.pem"))

	p := mustProvider(t, credentials.FileSource{
		CertFile: filepath.Join(dir, "absent.crt"),
		KeyFile:  filepath.Join(dir, "absent.key"),
		CAFile:   filepath.Join(dir, "ca.pem"),
	})
	_, err := p.ServerCredentials(t.Context())
	if err == nil {
		t.Fatal("want error for absent keypair, got nil")
	}
	if !strings.Contains(err.Error(), "absent.crt") {
		t.Fatalf("error = %q, want it to name the missing certificate file", err)
	}
}

func TestServerCredentials_MissingCAFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ca := newCA(t)
	certPath, keyPath := ca.issue(t, dir, "server", serverLeaf)

	p := mustProvider(t, credentials.FileSource{
		CertFile: certPath,
		KeyFile:  keyPath,
		CAFile:   filepath.Join(dir, "absent-ca.pem"),
	})
	_, err := p.ServerCredentials(t.Context())
	if err == nil {
		t.Fatal("want error for absent CA bundle, got nil")
	}
	if !strings.Contains(err.Error(), "absent-ca.pem") {
		t.Fatalf("error = %q, want it to name the missing CA file", err)
	}
}

func TestServerCredentials_CAFileIsNotPEM(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ca := newCA(t)
	certPath, keyPath := ca.issue(t, dir, "server", serverLeaf)
	junk := filepath.Join(dir, "junk-ca.pem")
	if err := os.WriteFile(junk, []byte("not a certificate\n"), 0o600); err != nil {
		t.Fatalf("write junk CA: %v", err)
	}

	p := mustProvider(t, credentials.FileSource{CertFile: certPath, KeyFile: keyPath, CAFile: junk})
	_, err := p.ServerCredentials(t.Context())
	if err == nil {
		t.Fatal("want error for a CA bundle with no certificates, got nil")
	}
	if !strings.Contains(err.Error(), "junk-ca.pem") {
		t.Fatalf("error = %q, want it to name the unusable CA file", err)
	}
}

func TestClientCredentials_MissingCertFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ca := newCA(t)
	ca.writeBundle(t, filepath.Join(dir, "ca.pem"))

	p := mustProvider(t, credentials.FileSource{
		CertFile: filepath.Join(dir, "absent.crt"),
		KeyFile:  filepath.Join(dir, "absent.key"),
		CAFile:   filepath.Join(dir, "ca.pem"),
	})
	_, err := p.ClientCredentials(t.Context())
	if err == nil {
		t.Fatal("want error for absent keypair, got nil")
	}
	if !strings.Contains(err.Error(), "absent.crt") {
		t.Fatalf("error = %q, want it to name the missing certificate file", err)
	}
}

// ---------------------------------------------------------------------
// Handshake behaviour. These assert what a peer observes — the
// connection is accepted or refused — not the shape of the tls.Config.
// Every refusal is paired with the acceptance case it is measured
// against, so "nothing connected" cannot satisfy the suite.
// ---------------------------------------------------------------------

func TestServerCredentials_AcceptsPeerFromTrustedCA(t *testing.T) {
	t.Parallel()
	ca := newCA(t)
	addr := serveHealth(t, ca)

	if err := dialHealth(t, addr, clientCredsFor(t, ca, ca)); err != nil {
		t.Fatalf("handshake with a peer from the trusted CA: %v", err)
	}
}

func TestServerCredentials_RefusesPeerFromUntrustedCA(t *testing.T) {
	t.Parallel()
	ca := newCA(t)
	foreign := newCA(t)
	addr := serveHealth(t, ca)

	// Client identity signed by a CA the server does not trust; the
	// client still trusts the server's CA, so only the client-auth
	// direction is under test.
	if err := dialHealth(t, addr, clientCredsFor(t, foreign, ca)); err == nil {
		t.Fatal("handshake with a peer from an untrusted CA: want refusal, got success")
	}
}

func TestServerCredentials_RefusesPeerWithNoCertificate(t *testing.T) {
	t.Parallel()
	ca := newCA(t)
	addr := serveHealth(t, ca)

	// No client certificate at all. This is the case that separates
	// RequireAndVerifyClientCert from VerifyClientCertIfGiven.
	anonymous := grpccreds.NewTLS(&tls.Config{
		MinVersion: tls.VersionTLS13,
		RootCAs:    ca.pool(t),
	})
	if err := dialHealth(t, addr, anonymous); err == nil {
		t.Fatal("handshake with no client certificate: want refusal, got success")
	}
}

func TestServerCredentials_RefusesPlaintextPeer(t *testing.T) {
	t.Parallel()
	ca := newCA(t)
	addr := serveHealth(t, ca)

	if err := dialHealth(t, addr, insecure.NewCredentials()); err == nil {
		t.Fatal("plaintext dial against an mTLS listener: want refusal, got success")
	}
}

func TestServerCredentials_RefusesPeerBelowTLS13(t *testing.T) {
	t.Parallel()
	ca := newCA(t)
	addr := serveHealth(t, ca)

	dir := t.TempDir()
	certPath, keyPath := ca.issue(t, dir, "client", clientLeaf)
	keypair, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		t.Fatalf("load client keypair: %v", err)
	}
	// A peer that is otherwise entirely acceptable, capped at TLS 1.2.
	// It must be refused, which is what pins MinVersion.
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

func TestClientCredentials_RefusesServerFromUntrustedCA(t *testing.T) {
	t.Parallel()
	ca := newCA(t)
	foreign := newCA(t)

	// Server identity from a CA the client does not trust. The server
	// trusts the client, so only the server-verification direction is
	// under test.
	addr := serveHealthWith(t, foreign, ca)

	dir := t.TempDir()
	certPath, keyPath := ca.issue(t, dir, "client", clientLeaf)
	p := mustProvider(t, credentials.FileSource{
		CertFile: certPath,
		KeyFile:  keyPath,
		CAFile:   ca.writeBundle(t, filepath.Join(dir, "ca.pem")),
	})
	creds, err := p.ClientCredentials(t.Context())
	if err != nil {
		t.Fatalf("ClientCredentials: %v", err)
	}
	if err := dialHealth(t, addr, creds); err == nil {
		t.Fatal("dial to a server from an untrusted CA: want refusal, got success")
	}
}

func TestClientCredentials_AcceptsServerFromTrustedCA(t *testing.T) {
	t.Parallel()
	ca := newCA(t)
	addr := serveHealth(t, ca)

	dir := t.TempDir()
	certPath, keyPath := ca.issue(t, dir, "client", clientLeaf)
	p := mustProvider(t, credentials.FileSource{
		CertFile: certPath,
		KeyFile:  keyPath,
		CAFile:   ca.writeBundle(t, filepath.Join(dir, "ca.pem")),
	})
	creds, err := p.ClientCredentials(t.Context())
	if err != nil {
		t.Fatalf("ClientCredentials: %v", err)
	}
	if err := dialHealth(t, addr, creds); err != nil {
		t.Fatalf("dial to a server from the trusted CA: %v", err)
	}
}

// ---------------------------------------------------------------------
// Helpers.
// ---------------------------------------------------------------------

func mustProvider(t *testing.T, src credentials.FileSource) *credentials.Provider {
	t.Helper()
	p, err := credentials.New(credentials.Config{Files: &src})
	if err != nil {
		t.Fatalf("New(%+v): %v", src, err)
	}
	return p
}

// serveHealth starts an mTLS gRPC health server whose identity and
// trust anchors both come from ca.
func serveHealth(t *testing.T, ca *testCA) string {
	t.Helper()
	return serveHealthWith(t, ca, ca)
}

// serveHealthWith starts an mTLS gRPC health server presenting an
// identity issued by identityCA and trusting clients issued by trustCA.
func serveHealthWith(t *testing.T, identityCA, trustCA *testCA) string {
	t.Helper()
	dir := t.TempDir()
	certPath, keyPath := identityCA.issue(t, dir, "server", serverLeaf)
	p := mustProvider(t, credentials.FileSource{
		CertFile: certPath,
		KeyFile:  keyPath,
		CAFile:   trustCA.writeBundle(t, filepath.Join(dir, "trust.pem")),
	})
	creds, err := p.ServerCredentials(t.Context())
	if err != nil {
		t.Fatalf("ServerCredentials: %v", err)
	}

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

// clientCredsFor builds client credentials presenting an identity from
// identityCA and trusting servers from trustCA. It deliberately does
// not go through the Provider: several tests need a peer the Provider
// would refuse to build.
func clientCredsFor(t *testing.T, identityCA, trustCA *testCA) grpccreds.TransportCredentials {
	t.Helper()
	dir := t.TempDir()
	certPath, keyPath := identityCA.issue(t, dir, "client", clientLeaf)
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
// The handshake is lazy in gRPC, so the RPC is what forces it.
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
		Subject:               pkix.Name{CommonName: "setec-test-ca"},
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

// issue writes a CA-signed leaf keypair into dir and returns the two
// paths.
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
