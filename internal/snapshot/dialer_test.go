// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package snapshot

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
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

	setecgrpcv1 "github.com/zeroroot-ai/setec/api/grpc/v1"
	"github.com/zeroroot-ai/setec/internal/credentials"
)

// The operator-to-node-agent hop is how a snapshot is taken of a
// running microVM, and it had no test at all. What matters on a client
// surface is the direction the server slices do not cover: the
// operator must satisfy itself about whom it is talking to before it
// hands over a sandbox id, and it must present an identity the
// node-agent can require.
//
// Every refusal below is paired with the acceptance case it is
// measured against.

func TestGRPCDialer_ReachesANodeAgentItTrusts(t *testing.T) {
	t.Parallel()
	ca := newCA(t)
	pattern := servePool(t, ca, ca)

	d := NewGRPCDialer(pattern, operatorCredentials(t, ca, ca))
	t.Cleanup(func() { _ = d.Close() })

	client, err := d.Dial(t.Context(), "node-1")
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	if err := queryPool(t, client); err != nil {
		t.Fatalf("QueryPool against a trusted node-agent: %v", err)
	}
}

func TestGRPCDialer_RefusesANodeAgentFromAnUntrustedCA(t *testing.T) {
	t.Parallel()
	ca := newCA(t)
	foreign := newCA(t)

	// The server's identity comes from a CA the operator does not
	// trust. It still trusts the operator, so only the direction
	// under test can fail.
	pattern := servePool(t, foreign, ca)

	d := NewGRPCDialer(pattern, operatorCredentials(t, ca, ca))
	t.Cleanup(func() { _ = d.Close() })

	client, err := d.Dial(t.Context(), "node-1")
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	if err := queryPool(t, client); err == nil {
		t.Fatal("QueryPool against a node-agent from an untrusted CA: want refusal, got success")
	}
}

// TestGRPCDialer_IsRefusedWhenItCannotProveWhoItIs is the mirror of the
// case above: the operator trusts the node-agent, but the node-agent
// does not trust the operator. It pins the fact that the credentials
// the dialer carries include an identity, not only trust anchors.
func TestGRPCDialer_IsRefusedWhenItCannotProveWhoItIs(t *testing.T) {
	t.Parallel()
	ca := newCA(t)
	foreign := newCA(t)
	pattern := servePool(t, ca, foreign)

	d := NewGRPCDialer(pattern, operatorCredentials(t, ca, ca))
	t.Cleanup(func() { _ = d.Close() })

	client, err := d.Dial(t.Context(), "node-1")
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	if err := queryPool(t, client); err == nil {
		t.Fatal("QueryPool with an identity the node-agent does not trust: want refusal, got success")
	}
}

func TestGRPCDialer_RefusesAPlaintextNodeAgent(t *testing.T) {
	t.Parallel()
	ca := newCA(t)
	pattern := servePlaintextPool(t)

	d := NewGRPCDialer(pattern, operatorCredentials(t, ca, ca))
	t.Cleanup(func() { _ = d.Close() })

	client, err := d.Dial(t.Context(), "node-1")
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	if err := queryPool(t, client); err == nil {
		t.Fatal("QueryPool against a plaintext listener: want refusal, got success")
	}
}

// TestGRPCDialer_RefusesToDialWithoutCredentials is the guard against
// the dialer being constructed by a future caller that forgot the
// credential module exists. A nil credential must never mean plaintext.
func TestGRPCDialer_RefusesToDialWithoutCredentials(t *testing.T) {
	t.Parallel()
	d := NewGRPCDialer("%s.setec-node-agent.setec-system.svc:50052", nil)
	t.Cleanup(func() { _ = d.Close() })

	_, err := d.Dial(t.Context(), "node-1")
	if err == nil {
		t.Fatal("Dial with no credentials: want error, got nil")
	}
	if !strings.Contains(err.Error(), "mTLS is mandatory") {
		t.Fatalf("error = %q, want it to say mTLS is mandatory", err)
	}
}

func TestGRPCDialer_RejectsAnEmptyNodeName(t *testing.T) {
	t.Parallel()
	ca := newCA(t)
	d := NewGRPCDialer("%s.setec-node-agent.setec-system.svc:50052", operatorCredentials(t, ca, ca))
	t.Cleanup(func() { _ = d.Close() })

	if _, err := d.Dial(t.Context(), ""); err == nil {
		t.Fatal("Dial with an empty node name: want error, got nil")
	}
}

// ---------------------------------------------------------------------
// Helpers.
// ---------------------------------------------------------------------

// operatorCredentials builds the operator's client credentials through
// the credential module, exactly as cmd/main.go does: an identity
// issued by identityCA, verifying node-agents against trustCA.
func operatorCredentials(t *testing.T, identityCA, trustCA *testCA) grpccreds.TransportCredentials {
	t.Helper()
	dir := t.TempDir()
	certPath, keyPath := identityCA.issue(t, dir, "operator", clientLeaf)
	provider, err := credentials.New(credentials.Config{
		Files: &credentials.FileSource{
			CertFile: certPath,
			KeyFile:  keyPath,
			CAFile:   trustCA.writeBundle(t, filepath.Join(dir, "trust.pem")),
		},
	})
	if err != nil {
		t.Fatalf("credentials.New: %v", err)
	}
	creds, err := provider.ClientCredentials(t.Context())
	if err != nil {
		t.Fatalf("ClientCredentials: %v", err)
	}
	return creds
}

// stubNodeAgent answers QueryPool so a successful call is unambiguous:
// anything other than a nil error means the connection never carried
// an RPC.
type stubNodeAgent struct {
	setecgrpcv1.UnimplementedNodeAgentServiceServer
}

func (stubNodeAgent) QueryPool(context.Context, *setecgrpcv1.QueryPoolRequest) (*setecgrpcv1.QueryPoolResponse, error) {
	return &setecgrpcv1.QueryPoolResponse{}, nil
}

// servePool starts an mTLS NodeAgentService presenting an identity from
// identityCA and requiring a client certificate issued by trustCA. It
// mirrors what cmd/node-agent stands up.
func servePool(t *testing.T, identityCA, trustCA *testCA) string {
	t.Helper()
	dir := t.TempDir()
	certPath, keyPath := identityCA.issue(t, dir, "node-agent", serverLeaf)
	provider, err := credentials.New(credentials.Config{
		Files: &credentials.FileSource{
			CertFile: certPath,
			KeyFile:  keyPath,
			CAFile:   trustCA.writeBundle(t, filepath.Join(dir, "trust.pem")),
		},
	})
	if err != nil {
		t.Fatalf("credentials.New: %v", err)
	}
	creds, err := provider.ServerCredentials(t.Context())
	if err != nil {
		t.Fatalf("ServerCredentials: %v", err)
	}
	return serve(t, grpc.Creds(creds))
}

// servePlaintextPool stands up the same service with no TLS at all.
func servePlaintextPool(t *testing.T) string {
	t.Helper()
	return serve(t, grpc.Creds(insecure.NewCredentials()))
}

// serve returns an EndpointPattern rather than a bare address. The
// dialer renders its target with fmt.Sprintf, so the pattern has to
// consume the node name; "%.0s" consumes it and prints nothing, which
// is how a test with one listener stands in for a DaemonSet.
func serve(t *testing.T, opt grpc.ServerOption) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer(opt)
	setecgrpcv1.RegisterNodeAgentServiceServer(srv, stubNodeAgent{})
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return "%.0s" + lis.Addr().String()
}

// queryPool forces the lazy gRPC handshake by issuing one RPC and
// returns whatever it produced. The response body carries nothing the
// tests care about; the error is the observation.
func queryPool(t *testing.T, client NodeAgentClient) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	_, err := client.QueryPool(ctx, &setecgrpcv1.QueryPoolRequest{})
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
		Subject:               pkix.Name{CommonName: "setec-snapshot-test-ca"},
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

func writePEM(t *testing.T, path, blockType string, der []byte) {
	t.Helper()
	buf := pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der})
	if err := os.WriteFile(path, buf, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
