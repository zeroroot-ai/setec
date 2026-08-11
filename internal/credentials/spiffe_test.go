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

	"github.com/zeroroot-ai/setec/internal/credentials"
)

const (
	callerID  = "spiffe://zeroroot.ai/ns/gibson/sa/gibson-daemon"
	serviceID = "spiffe://zeroroot.ai/ns/setec/sa/setec-frontend"
)

// ---------------------------------------------------------------------
// Mode selection. These are the silent-failure cases: SPIFFE mode is
// only worth having if it cannot be half-configured into something
// weaker than the file mode it replaces.
// ---------------------------------------------------------------------

func TestNew_BothSourcesConfigured(t *testing.T) {
	t.Parallel()
	_, err := credentials.New(credentials.Config{
		Files:  &credentials.FileSource{CertFile: "c.pem", KeyFile: "k.pem", CAFile: "ca.pem"},
		SPIFFE: &credentials.SPIFFESource{SocketPath: "/run/api.sock", AuthorizedIDs: []string{callerID}},
	})
	if err == nil {
		t.Fatal("New with both sources: want error, got nil")
	}
	if !strings.Contains(err.Error(), "exactly one") {
		t.Fatalf("error = %q, want it to name the conflict", err)
	}
}

func TestNew_IncompleteSPIFFESource(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		src      credentials.SPIFFESource
		wantWord string
	}{
		"no socket": {
			credentials.SPIFFESource{AuthorizedIDs: []string{callerID}},
			"socket path is empty",
		},
		"no allow-list": {
			credentials.SPIFFESource{SocketPath: "/run/api.sock"},
			"allow-list is empty",
		},
		"empty allow-list": {
			credentials.SPIFFESource{SocketPath: "/run/api.sock", AuthorizedIDs: []string{}},
			"allow-list is empty",
		},
		"malformed allow-list entry": {
			credentials.SPIFFESource{SocketPath: "/run/api.sock", AuthorizedIDs: []string{"gibson-daemon"}},
			"gibson-daemon",
		},
		"trust-domain-only allow-list entry": {
			credentials.SPIFFESource{SocketPath: "/run/api.sock", AuthorizedIDs: []string{"spiffe://zeroroot.ai"}},
			"names a trust domain and no workload",
		},
		"malformed socket address": {
			credentials.SPIFFESource{SocketPath: "http://spire/api", AuthorizedIDs: []string{callerID}},
			"http://spire/api",
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := credentials.New(credentials.Config{SPIFFE: &tc.src})
			if err == nil {
				t.Fatalf("New(%+v): want error, got nil", tc.src)
			}
			if !strings.Contains(err.Error(), tc.wantWord) {
				t.Fatalf("error = %q, want it to mention %q", err, tc.wantWord)
			}
		})
	}
}

func TestNew_CompleteSPIFFESource(t *testing.T) {
	t.Parallel()
	// The acceptance case for every refusal above: a source naming a
	// socket and one full SPIFFE ID is accepted, and accepted without
	// touching the socket, which is what lets a component decide its
	// configuration is coherent before it decides it can boot.
	if _, err := credentials.New(credentials.Config{SPIFFE: &credentials.SPIFFESource{
		SocketPath:    filepath.Join(t.TempDir(), "absent.sock"),
		AuthorizedIDs: []string{callerID},
	}}); err != nil {
		t.Fatalf("New with a complete SPIFFE source: %v", err)
	}
}

// ---------------------------------------------------------------------
// Acquisition. An unreachable Workload API is a boot failure and never
// a quiet downgrade to whatever files happen to be lying around.
// ---------------------------------------------------------------------

func TestServerCredentials_SPIFFEUnreachableSocket(t *testing.T) {
	t.Parallel()
	socket := filepath.Join(shortTempDir(t), "absent.sock")
	p := mustSPIFFEProvider(t, socket, callerID)

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	_, err := p.ServerCredentials(ctx)
	if err == nil {
		t.Fatal("ServerCredentials against an absent Workload API socket: want error, got nil")
	}
	if !strings.Contains(err.Error(), socket) {
		t.Fatalf("error = %q, want it to name the unreachable socket", err)
	}
}

func TestServerCredentials_SPIFFEReachableSocket(t *testing.T) {
	t.Parallel()
	// The acceptance case for the refusal above: the same call against
	// a socket that answers returns credentials.
	api := startWorkloadAPI(t, newCA(t))
	p := mustSPIFFEProvider(t, api.addr, callerID)

	if _, err := p.ServerCredentials(t.Context()); err != nil {
		t.Fatalf("ServerCredentials against a reachable Workload API: %v", err)
	}
}

func TestClientCredentials_SPIFFEModeAvailable(t *testing.T) {
	t.Parallel()
	// This test replaced one asserting that SPIFFE-mode client
	// credentials were refused. That refusal was setec#172's explicit
	// placeholder for this slice, named setec#174 in its message; it
	// went away when the replacement it was waiting for arrived. No
	// other test changed.
	api := startWorkloadAPI(t, newCA(t))
	p := mustSPIFFEProvider(t, api.addr, callerID)

	if _, err := p.ClientCredentials(t.Context()); err != nil {
		t.Fatalf("ClientCredentials against a reachable Workload API: %v", err)
	}
}

// ---------------------------------------------------------------------
// Peer authorization. Every peer below is signed by the CA the server
// trusts, so what separates them is identity and nothing else. A test
// that swapped in an untrusted CA would prove authentication and pass
// against a server with no allow-list at all.
// ---------------------------------------------------------------------

func TestSPIFFEServerCredentials_AcceptsAuthorizedPeer(t *testing.T) {
	t.Parallel()
	ca := newCA(t)
	api := startWorkloadAPI(t, ca)
	addr := serveHealthSPIFFE(t, api.addr, callerID)

	if err := dialHealth(t, addr, spiffeClientCreds(t, ca, callerID, ca)); err != nil {
		t.Fatalf("handshake from an authorized SPIFFE ID: %v", err)
	}
}

func TestSPIFFEServerCredentials_RefusesUnauthorizedSPIFFEID(t *testing.T) {
	t.Parallel()
	ca := newCA(t)
	api := startWorkloadAPI(t, ca)
	addr := serveHealthSPIFFE(t, api.addr, callerID)

	// Same trust domain, same CA, different workload. This is the case
	// the whole slice exists for: the certificate is entirely valid and
	// the peer is still refused.
	other := "spiffe://zeroroot.ai/ns/gibson/sa/some-other-workload"
	if err := dialHealth(t, addr, spiffeClientCreds(t, ca, other, ca)); err == nil {
		t.Fatal("handshake from an unauthorized SPIFFE ID: want refusal, got success")
	}
}

func TestSPIFFEServerCredentials_RefusesForeignTrustDomain(t *testing.T) {
	t.Parallel()
	ca := newCA(t)
	api := startWorkloadAPI(t, ca)
	addr := serveHealthSPIFFE(t, api.addr, callerID)

	// Identical path below a different trust domain, signed by the CA
	// the server trusts. Matching on the path alone would accept it.
	foreign := "spiffe://evil.example/ns/gibson/sa/gibson-daemon"
	if err := dialHealth(t, addr, spiffeClientCreds(t, ca, foreign, ca)); err == nil {
		t.Fatal("handshake from a foreign trust domain: want refusal, got success")
	}
}

func TestSPIFFEServerCredentials_RefusesPeerWithoutSPIFFEID(t *testing.T) {
	t.Parallel()
	ca := newCA(t)
	api := startWorkloadAPI(t, ca)
	addr := serveHealthSPIFFE(t, api.addr, callerID)

	// A perfectly ordinary cert-manager-shaped client certificate from
	// the trusted CA. In file mode this peer is accepted; in SPIFFE
	// mode it has no identity to authorize.
	dir := t.TempDir()
	certPath, keyPath := ca.issue(t, dir, "client", clientLeaf)
	keypair, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		t.Fatalf("load client keypair: %v", err)
	}
	creds := grpccreds.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{keypair},
		MinVersion:   tls.VersionTLS13,
		RootCAs:      ca.pool(t),
	})
	if err := dialHealth(t, addr, creds); err == nil {
		t.Fatal("handshake from a peer with no SPIFFE ID: want refusal, got success")
	}
}

func TestSPIFFEServerCredentials_RefusesAuthorizedIDFromUntrustedCA(t *testing.T) {
	t.Parallel()
	ca := newCA(t)
	foreign := newCA(t)
	api := startWorkloadAPI(t, ca)
	addr := serveHealthSPIFFE(t, api.addr, callerID)

	// The allow-listed identity, self-asserted by an issuer the server
	// does not trust. Authorization does not displace authentication.
	if err := dialHealth(t, addr, spiffeClientCreds(t, foreign, callerID, ca)); err == nil {
		t.Fatal("handshake from an untrusted CA claiming an authorized ID: want refusal, got success")
	}
}

// ---------------------------------------------------------------------
// Rotation. The SVID and the bundle are re-read for every handshake, so
// what SPIRE hands out after boot is what reaches the wire.
// ---------------------------------------------------------------------

func TestSPIFFEServerCredentials_RotatesInProcess(t *testing.T) {
	t.Parallel()
	first := newCA(t)
	second := newCA(t)
	api := startWorkloadAPI(t, first)
	addr := serveHealthSPIFFE(t, api.addr, callerID)

	// Before rotation the second CA is a stranger in both directions.
	if err := dialHealth(t, addr, spiffeClientCreds(t, second, callerID, second)); err == nil {
		t.Fatal("handshake against the pre-rotation server: want refusal, got success")
	}
	if err := dialHealth(t, addr, spiffeClientCreds(t, first, callerID, first)); err != nil {
		t.Fatalf("handshake against the pre-rotation server: %v", err)
	}

	api.setSVID(t, second, serviceID)

	// After rotation the running server presents and trusts the second
	// CA, with no restart. Rotation reaches the source asynchronously,
	// so the assertion is "within a bounded time", not "on the next
	// connection".
	eventually(t, 10*time.Second, func() error {
		return dialHealth(t, addr, spiffeClientCreds(t, second, callerID, second))
	})
	if err := dialHealth(t, addr, spiffeClientCreds(t, first, callerID, first)); err == nil {
		t.Fatal("handshake with the retired CA after rotation: want refusal, got success")
	}
}

func TestSPIFFESource_RotationFailureSurfaces(t *testing.T) {
	t.Parallel()
	api := startWorkloadAPI(t, newCA(t))

	reported := make(chan error, 8)
	p, err := credentials.New(credentials.Config{SPIFFE: &credentials.SPIFFESource{
		SocketPath:    api.addr,
		AuthorizedIDs: []string{callerID},
		OnRotationError: func(err error) {
			select {
			case reported <- err:
			default:
			}
		},
	}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := p.ServerCredentials(t.Context()); err != nil {
		t.Fatalf("ServerCredentials: %v", err)
	}

	// While the Workload API is healthy nothing is reported, so the
	// assertion below cannot be satisfied by a source that complains
	// unconditionally.
	select {
	case err := <-reported:
		t.Fatalf("healthy Workload API reported %v", err)
	case <-time.After(500 * time.Millisecond):
	}

	api.stop()

	// Losing the agent must say so. Left unreported it would first
	// become visible when the last SVID expired, long afterwards and
	// looking like a certificate problem.
	select {
	case <-reported:
	case <-time.After(15 * time.Second):
		t.Fatal("Workload API went away and nothing was reported")
	}
}

// ---------------------------------------------------------------------
// Helpers.
// ---------------------------------------------------------------------

func mustSPIFFEProvider(t *testing.T, socket string, authorized ...string) *credentials.Provider {
	t.Helper()
	p, err := credentials.New(credentials.Config{SPIFFE: &credentials.SPIFFESource{
		SocketPath:      socket,
		AuthorizedIDs:   authorized,
		OnRotationError: testReporter(t),
	}})
	if err != nil {
		t.Fatalf("New(SPIFFE %s): %v", socket, err)
	}
	return p
}

// serveHealthSPIFFE starts an mTLS gRPC health server whose credentials
// come from the Workload API at socket and which authorizes the given
// SPIFFE IDs.
func serveHealthSPIFFE(t *testing.T, socket string, authorized ...string) string {
	t.Helper()
	p := mustSPIFFEProvider(t, socket, authorized...)
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

// spiffeClientCreds builds client credentials presenting an X509-SVID
// for id issued by identityCA, and trusting servers from trustCA. It
// does not go through the Provider: the point of most of these tests is
// a peer the Provider would never build.
func spiffeClientCreds(t *testing.T, identityCA *testCA, id string, trustCA *testCA) grpccreds.TransportCredentials {
	t.Helper()
	leaf, key := identityCA.issueSPIFFE(t, id)
	return grpccreds.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{leaf.Raw}, PrivateKey: key, Leaf: leaf}},
		MinVersion:   tls.VersionTLS13,
		RootCAs:      trustCA.pool(t),
	})
}

// issueSPIFFE signs a leaf carrying id as its sole URI SAN. It also
// carries a loopback IP SAN so a test client can verify the server the
// ordinary way; a real X509-SVID has no such SAN, which is exactly why
// a SPIFFE-aware client verifies the SPIFFE ID instead.
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
		Subject:      pkix.Name{CommonName: ""},
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

// fakeWorkloadAPI serves the SPIFFE Workload API over a unix socket.
//
// The Workload API is a socket protocol, so the SPIFFE path is testable
// without a SPIRE deployment — and a test that needed one would not run
// anywhere this repo's CI runs.
type fakeWorkloadAPI struct {
	workload.UnimplementedSpiffeWorkloadAPIServer

	addr string

	mu       sync.Mutex
	response *workload.X509SVIDResponse
	updated  map[chan struct{}]struct{}

	srv      *grpc.Server
	stopOnce sync.Once
}

func startWorkloadAPI(t *testing.T, ca *testCA) *fakeWorkloadAPI {
	t.Helper()
	api := &fakeWorkloadAPI{updated: map[chan struct{}]struct{}{}}
	api.setSVID(t, ca, serviceID)

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

// setSVID replaces the served SVID and bundle, and wakes every open
// stream so the change propagates the way a SPIRE rotation would.
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
	for ch := range f.updated {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
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
			return err
		}
		select {
		case <-ch:
		case <-stream.Context().Done():
			return stream.Context().Err()
		}
	}
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

// testReporter logs Workload API failures against the test, and stops
// doing so once the test is over: the watcher outlives it and would
// otherwise log into a finished test.
func testReporter(t *testing.T) func(error) {
	t.Helper()
	var (
		mu   sync.Mutex
		done bool
	)
	t.Cleanup(func() {
		mu.Lock()
		done = true
		mu.Unlock()
	})
	return func(err error) {
		mu.Lock()
		defer mu.Unlock()
		if !done {
			t.Logf("workload API: %v", err)
		}
	}
}

// eventually retries fn until it returns nil or the budget runs out.
func eventually(t *testing.T, budget time.Duration, fn func() error) {
	t.Helper()
	deadline := time.Now().Add(budget)
	var err error
	for time.Now().Before(deadline) {
		if err = fn(); err == nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("condition never held within %s: %v", budget, err)
}
