// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

//go:build e2e

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

/*
Operator<->node-agent mTLS material for the E2E suite.

# Why the suite mints these itself (setec#320, setec#296)

With snapshots.enabled=true and credentials.mode=file the chart mounts three
Secrets across two workloads:

	operator  (deployment.yaml)  snapshots.mTLS.operatorCertSecret + caSecret
	node-agent (daemonset.yaml)  snapshots.mTLS.nodeAgentCertSecret + caSecret

None of the mounts is optional, and the chart creates only the two LEAF
Secrets — and only when snapshots.mTLS.certManager.enabled is on. `caSecret`
(setec-nodeagent-ca) is mounted by both workloads and produced by no values
combination at all, so the operator Pod never starts and the install dies on
the rollout wait:

	INSTALLATION FAILED: resource Deployment/... not ready.
	status: InProgress, message: Available: 0/2

That is setec#320, and it is a CHART defect that the chart has to fix (a CA
Certificate plus a namespaced Issuer both leaves are issued from, so the two
sides share a trust root — issuing both leaves straight from a *selfsigned*
ClusterIssuer, which is what the chart does today, makes each leaf its own
root and the channel cannot verify even once caSecret exists).

This file does NOT fix that. It is the suite's own way around it, and it
exists because the chart fix cannot be exercised from here: adding an Issuer
to the chart needs `create issuers.cert-manager.io` on the ARC runner's
ServiceAccount, which it does not have and which only a zeroroot-ai/gitops
change can grant. What the runner CAN do is create Secrets in the namespace it
owns — so the suite mints one CA, issues both leaves from it, and installs
with certManager.enabled=false. Same trust root, no cert-manager, no new RBAC.

This is the pattern webhookcert_test.go already uses for the admission
webhook's serving cert, for the same reason.
*/

package e2e

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"sigs.k8s.io/yaml"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/zeroroot-ai/setec/internal/credentials"
)

// Secret names the chart mounts. They are chart VALUES with these defaults
// (charts/setec/values.yaml, snapshots.mTLS), and the suite keeps the
// defaults rather than overriding them, so a rename on either side shows up
// as a mount failure rather than as a silently different Secret.
const (
	nodeAgentCASecret     = "setec-nodeagent-ca"
	nodeAgentServerSecret = "setec-nodeagent-server-tls"
	operatorClientSecret  = "setec-nodeagent-client-tls"
)

// caCertKey is the key the chart's mounts read the trust root from:
// `--nodeagent-ca=/etc/setec/nodeagent-ca/ca.crt` on the operator and
// `--tls-client-ca=/etc/setec/nodeagent-ca/ca.crt` on the node-agent. It
// matches what cert-manager writes into a Certificate's Secret, so the
// hand-minted Secret is drop-in for the cert-manager path.
const caCertKey = "ca.crt"

// operatorClientCN is the Subject CN the node-agent authorizes the operator
// by. It matches the chart's own operator-client Certificate; a different CN
// authenticates fine and then fails authorization, which surfaces as an
// opaque snapshot failure rather than as a TLS error.
const operatorClientCN = "setec-operator"

// nodeAgentMTLS is one CA plus the two leaves issued from it.
type nodeAgentMTLS struct {
	caPEM []byte

	serverCertPEM []byte
	serverKeyPEM  []byte

	clientCertPEM []byte
	clientKeyPEM  []byte
}

// generateNodeAgentMTLS mints a CA and the operator/node-agent leaf pair.
//
// The server leaf's SANs mirror the chart's own nodeagent-server Certificate
// exactly, because the operator dials per-node DNS names off the headless
// Service — `--nodeagent-endpoint-pattern=%s.<fullname>-node-agent.<ns>.svc:50052`
// (charts/setec/templates/deployment.yaml) — so the WILDCARD entry is the one
// that actually gets verified at dial time and the bare Service name is there
// for parity with the chart.
func generateNodeAgentMTLS(fullname, namespace string) (*nodeAgentMTLS, error) {
	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("node-agent CA key: %w", err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "setec-e2e-nodeagent-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, fmt.Errorf("node-agent CA cert: %w", err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		return nil, fmt.Errorf("parse node-agent CA: %w", err)
	}

	svc := fmt.Sprintf("%s-node-agent.%s.svc", fullname, namespace)
	serverCert, serverKey, err := issueLeaf(caCert, caKey, 2, svc,
		[]string{
			svc,
			svc + ".cluster.local",
			"*." + svc,
			"*." + svc + ".cluster.local",
		},
		// server AND client auth: the chart's own Certificate declares both,
		// and the node-agent presents this cert when the operator verifies it.
		[]x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	)
	if err != nil {
		return nil, fmt.Errorf("node-agent server leaf: %w", err)
	}

	clientCert, clientKey, err := issueLeaf(caCert, caKey, 3, operatorClientCN, nil,
		[]x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth})
	if err != nil {
		return nil, fmt.Errorf("operator client leaf: %w", err)
	}

	return &nodeAgentMTLS{
		caPEM:         pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		serverCertPEM: serverCert,
		serverKeyPEM:  serverKey,
		clientCertPEM: clientCert,
		clientKeyPEM:  clientKey,
	}, nil
}

// issueLeaf signs one leaf certificate with the given CA and returns the PEM
// cert and PKCS#8 PEM key.
//
// PKCS#8 ("PRIVATE KEY") rather than PKCS#1: Go's tls.LoadX509KeyPair accepts
// both, but the PKCS#1 form has already cost this repo one silent handshake
// failure on the webhook path (see webhookcert_test.go), so both cert helpers
// emit the same form.
func issueLeaf(
	ca *x509.Certificate,
	caKey *rsa.PrivateKey,
	serial int64,
	commonName string,
	dnsNames []string,
	usages []x509.ExtKeyUsage,
) (certPEM, keyPEM []byte, err error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, fmt.Errorf("key: %w", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(serial),
		Subject:      pkix.Name{CommonName: commonName},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  usages,
		DNSNames:     dnsNames,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		return nil, nil, fmt.Errorf("sign: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
		nil
}

// verifyNodeAgentMTLS proves the three pieces form a working channel by
// performing a REAL mTLS handshake between them — through
// internal/credentials, the same package cmd/setec and cmd/node-agent build
// their transport credentials from.
//
// Going through the production credential module rather than hand-rolling an
// x509.Verify is the point. A hand-rolled check answers "do these chain?";
// this answers "would the operator and the node-agent actually talk?", which
// is the question the install is betting on, and it picks up the TLS floor,
// the peer-authorization hook and the hostname policy that module sets and
// that a bespoke check would silently omit. It is also why this file needs no
// credguard exemption: it builds no credential of its own.
//
// The dialled name is a PER-NODE one (`node-1.<svc>`) rather than the bare
// Service name, because `--nodeagent-endpoint-pattern=%s.<fullname>-node-agent.<ns>.svc:50052`
// (charts/setec/templates/deployment.yaml) substitutes the node name. The
// wildcard SAN is what carries the channel; a certificate with only the bare
// Service name passes a naive check and fails every real dial.
func verifyNodeAgentMTLS(m *nodeAgentMTLS, fullname, namespace string) error {
	dir, err := os.MkdirTemp("", "setec-e2e-nodeagent-mtls")
	if err != nil {
		return fmt.Errorf("temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	write := func(name string, data []byte) (string, error) {
		path := filepath.Join(dir, name)
		// 0600: these are private keys, briefly, on the runner's disk.
		if err := os.WriteFile(path, data, 0o600); err != nil {
			return "", fmt.Errorf("write %s: %w", name, err)
		}
		return path, nil
	}
	caPath, err := write("ca.crt", m.caPEM)
	if err != nil {
		return err
	}
	srvCert, err := write("server.crt", m.serverCertPEM)
	if err != nil {
		return err
	}
	srvKey, err := write("server.key", m.serverKeyPEM)
	if err != nil {
		return err
	}
	cliCert, err := write("client.crt", m.clientCertPEM)
	if err != nil {
		return err
	}
	cliKey, err := write("client.key", m.clientKeyPEM)
	if err != nil {
		return err
	}

	// One CA file for both ends, which is the property the chart gets wrong:
	// issuing both leaves straight from a selfsigned ClusterIssuer makes each
	// leaf its own root and no single CAFile can verify both (setec#320).
	serverProvider, err := credentials.New(credentials.Config{Files: &credentials.FileSource{
		CertFile: srvCert, KeyFile: srvKey, CAFile: caPath,
	}})
	if err != nil {
		return fmt.Errorf("node-agent server credentials: %w", err)
	}
	clientProvider, err := credentials.New(credentials.Config{Files: &credentials.FileSource{
		CertFile: cliCert, KeyFile: cliKey, CAFile: caPath,
	}})
	if err != nil {
		return fmt.Errorf("operator client credentials: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	serverCreds, err := serverProvider.ServerCredentials(ctx)
	if err != nil {
		return fmt.Errorf("node-agent server credentials: %w", err)
	}
	clientCreds, err := clientProvider.ClientCredentials(ctx)
	if err != nil {
		return fmt.Errorf("operator client credentials: %w", err)
	}

	serverConn, clientConn := net.Pipe()
	defer func() { _ = serverConn.Close() }()
	defer func() { _ = clientConn.Close() }()

	type result struct{ err error }
	serverDone := make(chan result, 1)
	go func() {
		_, _, err := serverCreds.ServerHandshake(serverConn)
		serverDone <- result{err}
	}()

	authority := fmt.Sprintf("node-1.%s-node-agent.%s.svc:50052", fullname, namespace)
	if _, _, err := clientCreds.ClientHandshake(ctx, authority, clientConn); err != nil {
		return fmt.Errorf("operator could not complete an mTLS handshake to %s: %w", authority, err)
	}
	select {
	case r := <-serverDone:
		if r.err != nil {
			return fmt.Errorf("node-agent rejected the operator's client certificate: %w", r.err)
		}
	case <-ctx.Done():
		return fmt.Errorf("node-agent side of the handshake did not complete: %w", ctx.Err())
	}

	// Authentication is not authorization. The node-agent decides which
	// caller it serves by Subject CN, so a correctly-signed certificate with
	// the wrong CN completes the handshake above and then fails at the first
	// snapshot RPC — as an opaque "checkpoint failed" from the operator.
	client, err := parseLeaf(m.clientCertPEM)
	if err != nil {
		return fmt.Errorf("client leaf: %w", err)
	}
	if client.Subject.CommonName != operatorClientCN {
		return fmt.Errorf("operator client leaf CN = %q, want %q (the node-agent authorizes on CN)",
			client.Subject.CommonName, operatorClientCN)
	}
	return nil
}

// parseLeaf decodes a single PEM certificate.
func parseLeaf(certPEM []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return nil, fmt.Errorf("no PEM block")
	}
	return x509.ParseCertificate(block.Bytes)
}

// createNodeAgentMTLSSecrets writes the three Secrets the chart mounts when
// snapshots are on. It must run BEFORE helm install: `helm --wait` blocks on
// the operator Deployment, and a Pod whose Secret volume does not resolve
// never becomes Ready.
//
// fullname is the chart's `setec.fullname`, which for every release this
// suite creates is the release name (the release name contains the chart
// name "setec", so the helper returns it unchanged).
func createNodeAgentMTLSSecrets(ctx context.Context, fullname, namespace string) error {
	m, err := generateNodeAgentMTLS(fullname, namespace)
	if err != nil {
		return err
	}
	// Prove the material chains BEFORE it is published. A CA and two leaves
	// that do not verify against each other install perfectly happily — the
	// Pods start, `helm --wait` returns, and the run fails tens of minutes
	// later inside a scenario, as an opaque "checkpoint failed" from the
	// operator with the real handshake error buried in the node-agent log.
	// Failing here instead costs one second and names the defect.
	if err := verifyNodeAgentMTLS(m, fullname, namespace); err != nil {
		return fmt.Errorf("generated node-agent mTLS material is unusable: %w", err)
	}

	secrets := []*corev1.Secret{
		{
			ObjectMeta: metav1.ObjectMeta{Name: nodeAgentCASecret, Namespace: namespace},
			// Opaque, not kubernetes.io/tls: this Secret carries only the
			// trust root, and the tls type would require a tls.key beside it.
			Type: corev1.SecretTypeOpaque,
			Data: map[string][]byte{caCertKey: m.caPEM},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: nodeAgentServerSecret, Namespace: namespace},
			Type:       corev1.SecretTypeTLS,
			Data: map[string][]byte{
				"tls.crt": m.serverCertPEM,
				"tls.key": m.serverKeyPEM,
				caCertKey: m.caPEM,
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: operatorClientSecret, Namespace: namespace},
			Type:       corev1.SecretTypeTLS,
			Data: map[string][]byte{
				"tls.crt": m.clientCertPEM,
				"tls.key": m.clientKeyPEM,
				caCertKey: m.caPEM,
			},
		},
	}
	for _, s := range secrets {
		if err := k8sClient.Create(ctx, s); err != nil && !apierrorsIsAlreadyExists(err) {
			return fmt.Errorf("create node-agent mTLS secret %q: %w", s.Name, err)
		}
	}
	return nil
}

// TestNodeAgentMTLS_LeavesChainToOneCA is the executable half of this file.
//
// createNodeAgentMTLSSecrets only runs when snapshots are on, which on a
// shared cluster is the rarer install shape — so without this the generator
// could rot for months and the first thing to notice would be the
// session-checkpoint job, on the one run a human paid an EC2 metal node and
// a Terraform apply for. It needs no cluster state of its own beyond the
// suite that is already up, so it costs nothing to run on every suites run.
//
// It asserts the property that actually matters and that the CHART gets
// wrong (setec#320): both leaves chain to the SAME root, and the server leaf
// covers the per-node name the operator dials — not merely that three
// Secrets exist.
func TestNodeAgentMTLS_LeavesChainToOneCA(t *testing.T) {
	const (
		fullname = "setec-e2e-mtls"
		ns       = "setec-e2e-mtls-ns"
	)
	m, err := generateNodeAgentMTLS(fullname, ns)
	if err != nil {
		t.Fatalf("generate node-agent mTLS material: %v", err)
	}
	if err := verifyNodeAgentMTLS(m, fullname, ns); err != nil {
		t.Fatalf("generated material does not form a usable channel: %v", err)
	}

	// A leaf signed by a DIFFERENT CA must be refused. Without this the test
	// above would still pass if Verify were somehow accepting anything, and
	// "each leaf is its own root" — the chart's actual bug — is exactly the
	// shape that a permissive check waves through.
	other, err := generateNodeAgentMTLS(fullname, ns)
	if err != nil {
		t.Fatalf("generate second CA: %v", err)
	}
	mixed := &nodeAgentMTLS{
		caPEM:         m.caPEM,
		serverCertPEM: other.serverCertPEM,
		serverKeyPEM:  other.serverKeyPEM,
		clientCertPEM: m.clientCertPEM,
		clientKeyPEM:  m.clientKeyPEM,
	}
	if err := verifyNodeAgentMTLS(mixed, fullname, ns); err == nil {
		t.Fatal("a server leaf from an unrelated CA verified against this CA; the check proves nothing")
	}
}

// TestNodeAgentMTLS_SecretNamesTrackTheChart pins the three constants above
// to the chart's own defaults.
//
// The suite deliberately does NOT override snapshots.mTLS.*Secret, so these
// names are a contract with charts/setec/values.yaml and nothing else checks
// it. Rename a default there and the suite keeps creating the OLD names: the
// install then hangs for the full helm --timeout on Pods whose Secret volumes
// never resolve, and reports "context deadline exceeded" — a ten-minute,
// entirely mute failure. This turns that into one line naming both sides.
func TestNodeAgentMTLS_SecretNamesTrackTheChart(t *testing.T) {
	out, err := exec.Command("helm", "show", "values", chartPath).Output()
	if err != nil {
		t.Fatalf("helm show values %s: %v", chartPath, err)
	}
	var values struct {
		Snapshots struct {
			MTLS struct {
				OperatorCertSecret  string `json:"operatorCertSecret"`
				NodeAgentCertSecret string `json:"nodeAgentCertSecret"`
				CASecret            string `json:"caSecret"`
			} `json:"mTLS"`
		} `json:"snapshots"`
	}
	if err := yaml.Unmarshal(out, &values); err != nil {
		t.Fatalf("parse chart values: %v", err)
	}
	for _, c := range []struct{ name, got, want string }{
		{"snapshots.mTLS.caSecret", values.Snapshots.MTLS.CASecret, nodeAgentCASecret},
		{"snapshots.mTLS.nodeAgentCertSecret", values.Snapshots.MTLS.NodeAgentCertSecret, nodeAgentServerSecret},
		{"snapshots.mTLS.operatorCertSecret", values.Snapshots.MTLS.OperatorCertSecret, operatorClientSecret},
	} {
		if c.got != c.want {
			t.Errorf("chart default %s = %q, but the suite creates %q — installChart would mint Secrets nothing mounts and helm --wait would time out with no explanation",
				c.name, c.got, c.want)
		}
	}
}
