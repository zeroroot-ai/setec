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
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spiffe/go-spiffe/v2/proto/spiffe/workload"
	"google.golang.org/grpc"
	grpccreds "google.golang.org/grpc/credentials"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

// SPIFFE mode on the node-agent's listener, observed the only way that
// means anything: by what a peer gets when it dials.
//
// The load-bearing case is a certificate *validly signed by the trusted
// authority* that carries the wrong SPIFFE ID. File mode accepts it —
// that is precisely the gap SPIFFE mode closes — so a test that only
// exercised an untrusted CA would pass against the node-agent as it was
// before this change and prove nothing.
//
// The Workload API is a socket protocol, so all of this runs against a
// fixture. Nothing here needs a live SPIRE server or a microVM
// (setec#161).

const (
	nodeAgentID = "spiffe://zeroroot.ai/ns/setec/sa/setec-node-agent"
	callerID    = "spiffe://zeroroot.ai/ns/setec/sa/setec"
)

func TestNodeAgentListener_SPIFFEAcceptsAuthorizedPeer(t *testing.T) {
	t.Parallel()
	ca := newCA(t)
	api := startWorkloadAPI(t, ca)
	addr := serveNodeAgentSPIFFE(t, api.addr, callerID)

	if err := dialHealth(t, addr, spiffePeer(t, ca, callerID, ca)); err != nil {
		t.Fatalf("handshake with an authorized SPIFFE ID: %v", err)
	}
}

// TestNodeAgentListener_SPIFFERefusesUnauthorizedSPIFFEID is the one
// that distinguishes authorization from authentication. The peer's
// certificate is issued by the very authority the node-agent trusts;
// only its identity is wrong.
func TestNodeAgentListener_SPIFFERefusesUnauthorizedSPIFFEID(t *testing.T) {
	t.Parallel()
	ca := newCA(t)
	api := startWorkloadAPI(t, ca)
	addr := serveNodeAgentSPIFFE(t, api.addr, callerID)

	intruder := "spiffe://zeroroot.ai/ns/default/sa/anything-else"
	if err := dialHealth(t, addr, spiffePeer(t, ca, intruder, ca)); err == nil {
		t.Fatal("handshake with a validly-signed but unauthorized SPIFFE ID: want refusal, got success")
	}
}

// TestNodeAgentListener_SPIFFERefusesForeignTrustDomain pins that the
// allow-list matches on the trust domain and not only the path. An
// identical path issued by somebody else's SPIRE is a different
// principal.
func TestNodeAgentListener_SPIFFERefusesForeignTrustDomain(t *testing.T) {
	t.Parallel()
	ca := newCA(t)
	api := startWorkloadAPI(t, ca)
	addr := serveNodeAgentSPIFFE(t, api.addr, callerID)

	foreignPath := "spiffe://evil.example/ns/setec/sa/setec"
	if err := dialHealth(t, addr, spiffePeer(t, ca, foreignPath, ca)); err == nil {
		t.Fatal("handshake with the right path under a foreign trust domain: want refusal, got success")
	}
}

// TestServerCredentials_SPIFFEUnreachableSocketIsABootFailure pins the
// absence of a fallback. A node-agent that cannot reach its SPIRE agent
// must fail to start, not quietly serve on whatever files happen to be
// mounted — a silent downgrade is the failure this work exists to
// prevent.
func TestServerCredentials_SPIFFEUnreachableSocketIsABootFailure(t *testing.T) {
	t.Parallel()
	// The source waits 30s for an absent agent when the caller sets no
	// deadline of its own. A boot failure is the assertion here, not
	// how long the source is willing to wait for one.
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	_, mode, err := serverCredentials(ctx, credentialFlags{
		spiffeSocket:        "unix://" + filepath.Join(shortTempDir(t), "absent.sock"),
		spiffeAuthorizedIDs: repeatedString{callerID},
	})
	if err == nil {
		t.Fatal("SPIFFE mode with an unreachable Workload API: want a startup error, got nil")
	}
	if mode != spiffeMode {
		t.Fatalf("mode = %q, want %q — the failure must name the mode the operator selected", mode, spiffeMode)
	}
	if !strings.Contains(err.Error(), "absent.sock") {
		t.Fatalf("error = %q, want it to name the socket it could not reach", err)
	}
}

// ---------------------------------------------------------------------
// Helpers.
// ---------------------------------------------------------------------

// serveNodeAgentSPIFFE starts a gRPC server whose credentials come from
// the node-agent's own flag-handling path in SPIFFE mode.
func serveNodeAgentSPIFFE(t *testing.T, socket string, authorized ...string) string {
	t.Helper()
	opt, mode, err := serverCredentials(t.Context(), credentialFlags{
		spiffeSocket:        socket,
		spiffeAuthorizedIDs: repeatedString(authorized),
	})
	if err != nil {
		t.Fatalf("serverCredentials (SPIFFE): %v", err)
	}
	if mode != spiffeMode {
		t.Fatalf("mode = %q, want %q", mode, spiffeMode)
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

// spiffePeer builds client credentials presenting an X509-SVID for id
// issued by identityCA, trusting servers from trustCA. It does not go
// through the credential module: most of these tests need a peer the
// module would never build.
func spiffePeer(t *testing.T, identityCA *testCA, id string, trustCA *testCA) grpccreds.TransportCredentials {
	t.Helper()
	leaf, key := identityCA.issueSPIFFE(t, id)
	return grpccreds.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{leaf.Raw}, PrivateKey: key, Leaf: leaf}},
		MinVersion:   tls.VersionTLS13,
		RootCAs:      trustCA.pool(t),
	})
}

// issueSPIFFE signs a leaf carrying id as its sole URI SAN. It also
// carries a loopback IP SAN so the test client can verify the server the
// ordinary way; a real X509-SVID has no such SAN, which is exactly why a
// SPIFFE-aware client verifies the SPIFFE ID instead.
func (ca *testCA) issueSPIFFE(t *testing.T, id string) (*x509.Certificate, *ecdsa.PrivateKey) {
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
		Subject:      pkix.Name{},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{
			x509.ExtKeyUsageServerAuth,
			x509.ExtKeyUsageClientAuth,
		},
		URIs:        []*url.URL{uri},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
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

// fakeWorkloadAPI serves the SPIFFE Workload API over a unix socket, so
// the SPIFFE path is exercisable without a SPIRE deployment.
type fakeWorkloadAPI struct {
	workload.UnimplementedSpiffeWorkloadAPIServer

	addr string

	mu       sync.Mutex
	response *workload.X509SVIDResponse

	srv      *grpc.Server
	stopOnce sync.Once
}

func startWorkloadAPI(t *testing.T, ca *testCA) *fakeWorkloadAPI {
	t.Helper()
	api := &fakeWorkloadAPI{}
	api.setSVID(t, ca, nodeAgentID)

	socket := filepath.Join(shortTempDir(t), "agent.sock")
	lis, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("listen on %s: %v", socket, err)
	}
	api.addr = "unix://" + socket
	api.srv = grpc.NewServer()
	workload.RegisterSpiffeWorkloadAPIServer(api.srv, api)
	go func() { _ = api.srv.Serve(lis) }()
	t.Cleanup(api.stop)
	return api
}

func (f *fakeWorkloadAPI) setSVID(t *testing.T, ca *testCA, id string) {
	t.Helper()
	leaf, key := ca.issueSPIFFE(t, id)
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
		Bundle:      ca.der,
	}}}
}

func (f *fakeWorkloadAPI) stop() {
	f.stopOnce.Do(func() {
		if f.srv != nil {
			f.srv.Stop()
		}
	})
}

func (f *fakeWorkloadAPI) FetchX509SVID(
	_ *workload.X509SVIDRequest,
	stream grpc.ServerStreamingServer[workload.X509SVIDResponse],
) error {
	f.mu.Lock()
	response := f.response
	f.mu.Unlock()
	if err := stream.Send(response); err != nil {
		return err
	}
	<-stream.Context().Done()
	return stream.Context().Err()
}

// shortTempDir returns a temporary directory outside the test's own
// name-derived path. A unix socket path has a hard length limit that
// t.TempDir's directory names can breach.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "setec-wl")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}
