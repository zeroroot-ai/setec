// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package credentials_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"
	grpccreds "google.golang.org/grpc/credentials"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	"github.com/zeroroot-ai/setec/internal/credentials"
)

// Client-side authorization in SPIFFE mode.
//
// A client's job is not the server's job turned around. An X509-SVID
// carries no DNS name, so Go's usual answer to "is this the server I
// meant" — the hostname check — cannot run. It is *replaced* by the
// SPIFFE-ID check, and the failure to guard against is doing that
// replacement in the permissive direction: skipping hostname
// verification without putting anything in its place, which would
// accept any certificate at all.
//
// So the tests below come in two matched pairs. Identity: a server
// whose certificate is issued by the trusted authority but carries the
// wrong SPIFFE ID must be refused, and the expected one accepted.
// Authentication: a server carrying the *right* SPIFFE ID but signed by
// an authority outside the bundle must also be refused — that is the
// half that would silently disappear if InsecureSkipVerify had been set
// and nothing added back.

func TestSPIFFEClientCredentials_AcceptsExpectedServer(t *testing.T) {
	t.Parallel()
	ca := newCA(t)
	api := startWorkloadAPI(t, ca)
	// The client's allow-list names the server it means to reach.
	client := spiffeClient(t, api.addr, serviceID)
	addr := serveSPIFFEPeer(t, ca, serviceID)

	if err := dialHealth(t, addr, client); err != nil {
		t.Fatalf("dial to the expected server: %v", err)
	}
}

// TestSPIFFEClientCredentials_RefusesServerWithUnexpectedSPIFFEID is
// the load-bearing case. The server's certificate chains to the very
// bundle the client trusts; only its identity is not the one the client
// meant to reach. Chaining to the trust bundle is not sufficient, and
// this is what says so.
func TestSPIFFEClientCredentials_RefusesServerWithUnexpectedSPIFFEID(t *testing.T) {
	t.Parallel()
	ca := newCA(t)
	api := startWorkloadAPI(t, ca)
	client := spiffeClient(t, api.addr, serviceID)

	impostor := "spiffe://zeroroot.ai/ns/default/sa/not-the-node-agent"
	addr := serveSPIFFEPeer(t, ca, impostor)

	if err := dialHealth(t, addr, client); err == nil {
		t.Fatal("dial to a validly-signed server with an unexpected SPIFFE ID: want refusal, got success")
	}
}

// TestSPIFFEClientCredentials_RefusesUnexpectedTrustDomain pins that
// the client matches on the trust domain as well as the path. An
// identical path issued by somebody else's SPIRE is a different
// principal.
func TestSPIFFEClientCredentials_RefusesUnexpectedTrustDomain(t *testing.T) {
	t.Parallel()
	ca := newCA(t)
	api := startWorkloadAPI(t, ca)
	client := spiffeClient(t, api.addr, serviceID)

	foreign := "spiffe://evil.example/ns/setec/sa/setec-frontend"
	addr := serveSPIFFEPeer(t, ca, foreign)

	if err := dialHealth(t, addr, client); err == nil {
		t.Fatal("dial to the right path under a foreign trust domain: want refusal, got success")
	}
}

// TestSPIFFEClientCredentials_RefusesExpectedIDFromUntrustedAuthority
// is the other half of the pair. Replacing the hostname check must not
// mean dropping chain verification: a server that claims the right
// SPIFFE ID but was signed by an authority outside the bundle is an
// impostor, and only the chain check catches it.
func TestSPIFFEClientCredentials_RefusesExpectedIDFromUntrustedAuthority(t *testing.T) {
	t.Parallel()
	ca := newCA(t)
	foreign := newCA(t)
	api := startWorkloadAPI(t, ca)
	client := spiffeClient(t, api.addr, serviceID)

	// Right identity, wrong signature.
	addr := serveSPIFFEPeer(t, foreign, serviceID)

	if err := dialHealth(t, addr, client); err == nil {
		t.Fatal("dial to a server claiming the expected ID from an untrusted authority: want refusal, got success")
	}
}

// TestSPIFFEClientCredentials_RefusesPlaintextServer pins that the
// replacement verification did not turn the channel into something a
// plaintext listener can satisfy.
func TestSPIFFEClientCredentials_RefusesPlaintextServer(t *testing.T) {
	t.Parallel()
	ca := newCA(t)
	api := startWorkloadAPI(t, ca)
	client := spiffeClient(t, api.addr, serviceID)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer()
	healthpb.RegisterHealthServer(srv, health.NewServer())
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	if err := dialHealth(t, lis.Addr().String(), client); err == nil {
		t.Fatal("dial to a plaintext listener: want refusal, got success")
	}
}

// TestFileClientCredentials_StillChecksTheServerName pins that the
// replacement is scoped to sources whose certificates carry no name.
// File mode keeps Go's hostname check, and a certificate for the wrong
// name is refused even though it chains perfectly.
func TestFileClientCredentials_StillChecksTheServerName(t *testing.T) {
	t.Parallel()
	ca := newCA(t)
	dir := t.TempDir()

	// A server certificate valid for a name this client is not dialing.
	certPath, keyPath := ca.issueNamed(t, dir, "elsewhere", "elsewhere.invalid")
	keypair, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		t.Fatalf("load server keypair: %v", err)
	}
	addr := serveWith(t, grpccreds.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{keypair},
		MinVersion:   tls.VersionTLS13,
	}))

	clientCert, clientKey := ca.issue(t, dir, "client", clientLeaf)
	p, err := credentials.New(credentials.Config{Files: &credentials.FileSource{
		CertFile: clientCert,
		KeyFile:  clientKey,
		CAFile:   ca.writeBundle(t, dir+"/ca.pem"),
	}})
	if err != nil {
		t.Fatalf("credentials.New: %v", err)
	}
	creds, err := p.ClientCredentials(t.Context())
	if err != nil {
		t.Fatalf("ClientCredentials: %v", err)
	}
	if err := dialHealth(t, addr, creds); err == nil {
		t.Fatal("dial to a server certificate issued for another name: want refusal, got success")
	}
}

// ---------------------------------------------------------------------
// Helpers.
// ---------------------------------------------------------------------

// spiffeClient builds client credentials from a SPIFFE source whose
// allow-list names the servers worth talking to.
func spiffeClient(t *testing.T, socket string, expected ...string) grpccreds.TransportCredentials {
	t.Helper()
	creds, err := mustSPIFFEProvider(t, socket, expected...).ClientCredentials(t.Context())
	if err != nil {
		t.Fatalf("ClientCredentials: %v", err)
	}
	return creds
}

// serveSPIFFEPeer starts a TLS gRPC server presenting an X509-SVID for
// id signed by identityCA. It does not go through the Provider: most of
// these tests need a server the Provider would never build.
//
// It asks for no client certificate, so only the direction under
// test — the client's view of the server — can fail.
func serveSPIFFEPeer(t *testing.T, identityCA *testCA, id string) string {
	t.Helper()
	leaf, key := identityCA.issueNamelessSVID(t, id)
	return serveWith(t, grpccreds.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{leaf.Raw}, PrivateKey: key, Leaf: leaf}},
		MinVersion:   tls.VersionTLS13,
	}))
}

func serveWith(t *testing.T, creds grpccreds.TransportCredentials) string {
	t.Helper()
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

// issueNamed writes a CA-signed server keypair valid only for dnsName —
// no loopback IP SAN — so a client dialling 127.0.0.1 must reject it on
// the name. testCA.issue deliberately includes the loopback address,
// which is what makes every other handshake test work.
func (ca *testCA) issueNamed(t *testing.T, dir, name, dnsName string) (certPath, keyPath string) {
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
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{dnsName},
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

// issueNamelessSVID signs a leaf carrying id as its sole URI SAN and no
// DNS or IP SAN at all — what a real X509-SVID looks like.
//
// testCA.issueSPIFFE deliberately adds a loopback IP SAN so a
// non-SPIFFE test client can verify the server the ordinary way. On the
// client side that SAN would hide the whole point of this slice: with
// it present, Go's hostname check succeeds and a build that never
// replaced the hostname check would pass these tests anyway. Without
// it, only the replacement can complete a handshake.
func (ca *testCA) issueNamelessSVID(t *testing.T, id string) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	uri, err := url.Parse(id)
	if err != nil {
		t.Fatalf("parse SPIFFE ID %q: %v", id, err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("svid key: %v", err)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		URIs:         []*url.URL{uri},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatalf("svid cert: %v", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse svid: %v", err)
	}
	return leaf, key
}
