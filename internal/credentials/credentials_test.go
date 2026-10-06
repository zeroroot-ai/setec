// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

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
	"testing"
	"time"

	"google.golang.org/grpc"
	grpccreds "google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

// ---------------------------------------------------------------------
// Handshake behavior of the server credentials. Each refusal is paired
// with the acceptance case of spiffe_test.go, which uses the same
// server, so "nothing connected" cannot satisfy the suite.
// ---------------------------------------------------------------------

func TestServerCredentials_RefusesPeerWithNoCertificate(t *testing.T) {
	t.Parallel()
	ca := newCA(t)
	api := startWorkloadAPI(t, ca)
	addr := serveHealthSPIFFE(t, api.addr, callerID)

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
	api := startWorkloadAPI(t, ca)
	addr := serveHealthSPIFFE(t, api.addr, callerID)

	if err := dialHealth(t, addr, insecure.NewCredentials()); err == nil {
		t.Fatal("plaintext dial against an mTLS listener: want refusal, got success")
	}
}

func TestServerCredentials_RefusesPeerBelowTLS13(t *testing.T) {
	t.Parallel()
	ca := newCA(t)
	api := startWorkloadAPI(t, ca)
	addr := serveHealthSPIFFE(t, api.addr, callerID)

	// A peer that is otherwise entirely acceptable, capped at TLS 1.2.
	// It must be refused, which is what pins MinVersion.
	leaf, key := ca.issueSPIFFE(t, callerID)
	legacy := grpccreds.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{leaf.Raw}, PrivateKey: key, Leaf: leaf}},
		MinVersion:   tls.VersionTLS12,
		MaxVersion:   tls.VersionTLS12,
		RootCAs:      ca.pool(t),
	})
	if err := dialHealth(t, addr, legacy); err == nil {
		t.Fatal("handshake capped at TLS 1.2: want refusal, got success")
	}
}

// ---------------------------------------------------------------------
// Helpers.
// ---------------------------------------------------------------------

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
