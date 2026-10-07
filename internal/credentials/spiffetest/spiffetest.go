// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

// Package spiffetest serves a fake SPIFFE Workload API for tests of a
// package that builds its credentials with the credentials package.
//
// The Workload API is a socket protocol, so the one credential source of
// setec is testable without a SPIRE deployment.
package spiffetest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/spiffe/go-spiffe/v2/proto/spiffe/workload"
	"google.golang.org/grpc"
)

// CA is a throwaway certificate authority for one test. It is the root
// of one trust domain.
type CA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	der  []byte
}

// NewCA makes a CA.
func NewCA(t *testing.T) *CA {
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
	return &CA{cert: cert, key: key, der: der}
}

// Pool is a certificate pool that holds the CA.
func (ca *CA) Pool() *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(ca.cert)
	return pool
}

// IssueSVID signs an X509-SVID that carries id as its URI SAN. It also
// carries the loopback address, so that a test peer built with plain
// crypto/tls can verify a server on 127.0.0.1. A real SVID has no such
// name, which is why the credentials package checks the SPIFFE ID
// instead.
func (ca *CA) IssueSVID(t *testing.T, id string) (*x509.Certificate, *ecdsa.PrivateKey) {
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
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
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

// WorkloadAPI is a fake Workload API on a unix socket. It serves one
// X509-SVID and the bundle of its trust domain.
type WorkloadAPI struct {
	workload.UnimplementedSpiffeWorkloadAPIServer

	// Addr is the endpoint address, for SPIFFESource.SocketPath.
	Addr string

	mu       sync.Mutex
	response *workload.X509SVIDResponse
	updated  map[chan struct{}]struct{}
}

// Start serves an SVID for id that issuer signed, with the CA bundle as
// the trust anchors of the trust domain. The server stops when the test
// ends.
func Start(t *testing.T, issuer *CA, id string, bundle *CA) *WorkloadAPI {
	t.Helper()
	api := &WorkloadAPI{updated: map[chan struct{}]struct{}{}}
	api.SetSVID(t, issuer, id, bundle)

	// A unix socket path has a hard length limit that the directory of
	// t.TempDir can pass, so the socket goes in a short directory.
	dir, err := os.MkdirTemp("", "setec-wl")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "agent.sock")
	lis, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("listen on %s: %v", socket, err)
	}
	api.Addr = "unix://" + socket
	srv := grpc.NewServer()
	workload.RegisterSpiffeWorkloadAPIServer(srv, api)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return api
}

// SetSVID replaces the served SVID and bundle, and wakes each open stream
// as a rotation of SPIRE does.
func (f *WorkloadAPI) SetSVID(t *testing.T, issuer *CA, id string, bundle *CA) {
	t.Helper()
	leaf, key := issuer.IssueSVID(t, id)
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal svid key: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.response = &workload.X509SVIDResponse{Svids: []*workload.X509SVID{{
		SpiffeId:    id,
		X509Svid:    leaf.Raw,
		X509SvidKey: keyDER,
		Bundle:      bundle.der,
	}}}
	for ch := range f.updated {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// FetchX509SVID streams the current response, and again after each
// change.
func (f *WorkloadAPI) FetchX509SVID(
	_ *workload.X509SVIDRequest,
	stream grpc.ServerStreamingServer[workload.X509SVIDResponse],
) error {
	ch := make(chan struct{}, 1)
	f.mu.Lock()
	f.updated[ch] = struct{}{}
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		delete(f.updated, ch)
		f.mu.Unlock()
	}()
	for {
		f.mu.Lock()
		response := f.response
		f.mu.Unlock()
		if err := stream.Send(response); err != nil {
			return err //nolint:wrapcheck // the gRPC status goes back to the client as is
		}
		select {
		case <-ch:
		case <-stream.Context().Done():
			return stream.Context().Err() //nolint:wrapcheck // the end of the stream, for the gRPC server
		}
	}
}
