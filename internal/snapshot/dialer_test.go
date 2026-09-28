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
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	grpccreds "google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

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
	port, _ := servePool(t, "127.0.0.1", ca, ca)

	d := NewGRPCDialer(fixedPod("node-1", "127.0.0.1"), authorityPattern(port), operatorCredentials(t, ca, ca))
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
	port, _ := servePool(t, "127.0.0.1", foreign, ca)

	d := NewGRPCDialer(fixedPod("node-1", "127.0.0.1"), authorityPattern(port), operatorCredentials(t, ca, ca))
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
	port, _ := servePool(t, "127.0.0.1", ca, foreign)

	d := NewGRPCDialer(fixedPod("node-1", "127.0.0.1"), authorityPattern(port), operatorCredentials(t, ca, ca))
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
	port, _ := servePlaintextPool(t, "127.0.0.1")

	d := NewGRPCDialer(fixedPod("node-1", "127.0.0.1"), authorityPattern(port), operatorCredentials(t, ca, ca))
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
	d := NewGRPCDialer(fixedPod("node-1", "127.0.0.1"), "%s.setec-node-agent.setec-system.svc:50052", nil)
	t.Cleanup(func() { _ = d.Close() })

	_, err := d.Dial(t.Context(), "node-1")
	if err == nil {
		t.Fatal("Dial with no credentials: want error, got nil")
	}
	if !strings.Contains(err.Error(), "mTLS is mandatory") {
		t.Fatalf("error = %q, want it to say mTLS is mandatory", err)
	}
}

// TestGRPCDialer_RequiresAResolver guards the analogous mistake on the
// other required collaborator: a dialer built with no way to find the
// node-agent Pod must refuse rather than dial nothing.
func TestGRPCDialer_RequiresAResolver(t *testing.T) {
	t.Parallel()
	ca := newCA(t)
	d := NewGRPCDialer(nil, "%s.setec-node-agent.setec-system.svc:50052", operatorCredentials(t, ca, ca))
	t.Cleanup(func() { _ = d.Close() })

	_, err := d.Dial(t.Context(), "node-1")
	if err == nil {
		t.Fatal("Dial with no Resolver: want error, got nil")
	}
	if !strings.Contains(err.Error(), "Resolver is required") {
		t.Fatalf("error = %q, want it to say a Resolver is required", err)
	}
}

func TestGRPCDialer_RejectsAnEmptyNodeName(t *testing.T) {
	t.Parallel()
	ca := newCA(t)
	d := NewGRPCDialer(fixedPod("node-1", "127.0.0.1"), "%s.setec-node-agent.setec-system.svc:50052", operatorCredentials(t, ca, ca))
	t.Cleanup(func() { _ = d.Close() })

	if _, err := d.Dial(t.Context(), ""); err == nil {
		t.Fatal("Dial with an empty node name: want error, got nil")
	}
}

// TestGRPCDialer_PropagatesResolverFailure is the "no pod" case at the
// dialer level: when the resolver cannot find a node-agent Pod on the
// node (setec#92's original symptom, absent this fix's cause), Dial
// must surface that rather than fail some other, more confusing way.
func TestGRPCDialer_PropagatesResolverFailure(t *testing.T) {
	t.Parallel()
	ca := newCA(t)
	resolveErr := errors.New("podresolver: no Running and Ready node-agent pod found on node \"node-1\"")
	d := NewGRPCDialer(&fakeResolver{err: resolveErr}, "%s.setec-node-agent.setec-system.svc:50052", operatorCredentials(t, ca, ca))
	t.Cleanup(func() { _ = d.Close() })

	_, err := d.Dial(t.Context(), "node-1")
	if err == nil {
		t.Fatal("Dial with a failing Resolver: want error, got nil")
	}
	if !errors.Is(err, resolveErr) {
		t.Fatalf("error = %v, want it to wrap %v", err, resolveErr)
	}
}

// TestGRPCDialer_RejectsAPodWithNoIP guards against dialing an empty
// address: a Pod between being scheduled and having its IP assigned is
// not yet something the operator can dial.
func TestGRPCDialer_RejectsAPodWithNoIP(t *testing.T) {
	t.Parallel()
	ca := newCA(t)
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{UID: types.UID("no-ip")}}
	d := NewGRPCDialer(&fakeResolver{pod: pod}, "%s.setec-node-agent.setec-system.svc:50052", operatorCredentials(t, ca, ca))
	t.Cleanup(func() { _ = d.Close() })

	_, err := d.Dial(t.Context(), "node-1")
	if err == nil {
		t.Fatal("Dial against a Pod with no PodIP: want error, got nil")
	}
	if !strings.Contains(err.Error(), "no PodIP") {
		t.Fatalf("error = %q, want it to mention the missing PodIP", err)
	}
}

// TestGRPCDialer_RedialsAfterNodeAgentRestart is the connection-cache
// half of setec#92's fix: a node-agent restart gives the DaemonSet Pod
// on that node a new UID and a new IP. The old cached connection must
// be dropped rather than kept.
//
// The fixture proves this by killing the OLD node-agent's listener
// before asking the dialer for a connection to the (now restarted)
// node again. A cache keyed only on node name would still hand back
// the old, now-dead connection, and this test would fail with a
// transport error. A cache keyed on the Pod's identity notices the Pod
// changed, drops the old connection, and dials the new, live one.
func TestGRPCDialer_RedialsAfterNodeAgentRestart(t *testing.T) {
	t.Parallel()
	ca := newCA(t)

	// Two loopback addresses, one port: 127.0.0.x are all loopback on
	// Linux, so the "old" and "new" node-agent can each bind the same
	// port setec always dials (50052 in production) on a different
	// address, exactly as two different Pod IPs would.
	port, oldSrv := servePool(t, "127.0.0.2", ca, ca)
	servePoolOnFixedPort(t, "127.0.0.3", port, ca, ca)

	resolver := &fakeResolver{pod: podWithUID("old-uid", "127.0.0.2")}
	d := NewGRPCDialer(resolver, authorityPattern(port), operatorCredentials(t, ca, ca))
	t.Cleanup(func() { _ = d.Close() })

	client, err := d.Dial(t.Context(), "node-1")
	if err != nil {
		t.Fatalf("Dial (old node-agent): %v", err)
	}
	if err := queryPool(t, client); err != nil {
		t.Fatalf("QueryPool against the old node-agent: %v", err)
	}

	// The node-agent on node-1 restarts: its old Pod, and the listener
	// standing in for it, are both gone. Only a fresh connection to
	// the new Pod can succeed from here.
	oldSrv.Stop()
	resolver.set(podWithUID("new-uid", "127.0.0.3"))

	client, err = d.Dial(t.Context(), "node-1")
	if err != nil {
		t.Fatalf("Dial (new node-agent): %v", err)
	}
	if err := queryPool(t, client); err != nil {
		t.Fatalf("QueryPool against the new node-agent: want success (proves the stale "+
			"connection to the stopped old node-agent was dropped), got %v", err)
	}
}

// ---------------------------------------------------------------------
// Helpers.
// ---------------------------------------------------------------------

// authorityPattern builds a GRPCDialer.AuthorityPattern that renders
// to "localhost:<port>" no matter which node name gets substituted in.
// The "%.0s" verb consumes the node name argument and prints nothing.
// "localhost" is what the test leaf certificates carry as a DNS SAN,
// so a successful RPC through it proves the dialer verified the
// node-agent by that name. Per fixedPod and podWithUID, it does this
// while dialing a different, numeric loopback address entirely.
func authorityPattern(port int) string {
	return fmt.Sprintf("%%.0slocalhost:%d", port)
}

// fakeResolver is the NodeAgentPodResolver test double. A nil pod with
// a nil err is never a valid state to query. Callers must set one or
// the other.
type fakeResolver struct {
	mu  sync.Mutex
	pod *corev1.Pod
	err error
}

func (f *fakeResolver) ResolveNodeAgentPod(_ context.Context, _ string) (*corev1.Pod, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	return f.pod, nil
}

func (f *fakeResolver) set(pod *corev1.Pod) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pod = pod
}

// fixedPod is a fakeResolver that always resolves nodeName to a
// Running, Ready Pod at the given IP with a stable UID.
func fixedPod(nodeName, ip string) *fakeResolver {
	return &fakeResolver{pod: podWithUID(nodeName+"-uid", ip)}
}

// podWithUID returns a Running, Ready node-agent Pod fixture with the
// given UID and PodIP.
func podWithUID(uid, ip string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "node-agent-" + uid,
			Namespace: "setec-system",
			UID:       types.UID(uid),
			Labels:    map[string]string{NodeAgentComponentLabel: nodeAgentComponentValue},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			PodIP: ip,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
		},
	}
}

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

// servePool starts an mTLS NodeAgentService on addr:0, presenting an
// identity from identityCA and requiring a client certificate issued
// by trustCA. It mirrors what cmd/node-agent stands up. It returns the
// bound port and the *grpc.Server, so a test can Stop it early (e.g.
// to simulate a node-agent restart) as well as via the automatic
// t.Cleanup.
func servePool(t *testing.T, addr string, identityCA, trustCA *testCA) (int, *grpc.Server) {
	t.Helper()
	return servePoolOnFixedPort(t, addr, 0, identityCA, trustCA)
}

// servePoolOnFixedPort is servePool with an explicit port (0 means
// "any"), so two backends can share one logical port across different
// loopback addresses the way two Pod IPs would share a container port.
func servePoolOnFixedPort(t *testing.T, addr string, port int, identityCA, trustCA *testCA) (int, *grpc.Server) {
	t.Helper()
	dir := t.TempDir()
	certPath, keyPath := identityCA.issue(t, dir, fmt.Sprintf("node-agent-%s", addr), serverLeaf)
	provider, err := credentials.New(credentials.Config{
		Files: &credentials.FileSource{
			CertFile: certPath,
			KeyFile:  keyPath,
			CAFile:   trustCA.writeBundle(t, filepath.Join(dir, "trust-"+addr+".pem")),
		},
	})
	if err != nil {
		t.Fatalf("credentials.New: %v", err)
	}
	creds, err := provider.ServerCredentials(t.Context())
	if err != nil {
		t.Fatalf("ServerCredentials: %v", err)
	}
	return serve(t, net.JoinHostPort(addr, strconv.Itoa(port)), grpc.Creds(creds))
}

// servePlaintextPool stands up the same service with no TLS at all.
func servePlaintextPool(t *testing.T, addr string) (int, *grpc.Server) {
	t.Helper()
	return serve(t, net.JoinHostPort(addr, "0"), grpc.Creds(insecure.NewCredentials()))
}

// serve starts a NodeAgentService listening at addr and returns the
// bound port and the *grpc.Server (Stop is safe to call more than
// once, so callers may Stop it early and still rely on t.Cleanup).
func serve(t *testing.T, addr string, opt grpc.ServerOption) (int, *grpc.Server) {
	t.Helper()
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("listen on %s: %v", addr, err)
	}
	srv := grpc.NewServer(opt)
	setecgrpcv1.RegisterNodeAgentServiceServer(srv, stubNodeAgent{})
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	_, portStr, err := net.SplitHostPort(lis.Addr().String())
	if err != nil {
		t.Fatalf("split listener address %s: %v", lis.Addr(), err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parse listener port %s: %v", portStr, err)
	}
	return port, srv
}

// queryPool forces the lazy gRPC handshake by issuing one RPC and
// returns whatever it produced. The response body carries nothing the
// tests care about. The error is the observation.
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
// The server leaf always carries "localhost" as a DNS SAN. That is the
// authority every test dials, per authorityPattern, regardless of
// which numeric loopback address it is actually reached on.
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
