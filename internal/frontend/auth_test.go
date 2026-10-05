// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package frontend

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"math/big"
	"net/url"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	setecv1grpc "github.com/zeroroot-ai/setec/api/grpc/v1"
	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
	"github.com/zeroroot-ai/setec/internal/tenancy"
)

const (
	idClusterA = "spiffe://a.example/ns/gibson/sa/gibson-daemon"
	idClusterB = "spiffe://b.example/ns/gibson/sa/gibson-daemon"
	idStranger = "spiffe://c.example/ns/gibson/sa/gibson-daemon"
)

// makeCert returns a self-signed certificate. A non-empty uri becomes its one
// URI SAN, as in an X.509 SVID.
func makeCert(t *testing.T, uri string) *x509.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     []string{"tenant-from-dns.svc"},
	}
	if uri != "" {
		u, err := url.Parse(uri)
		if err != nil {
			t.Fatalf("uri: %v", err)
		}
		tpl.URIs = []*url.URL{u}
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("cert: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return cert
}

func ctxWithCert(cert *x509.Certificate) context.Context {
	p := &peer.Peer{
		AuthInfo: credentials.TLSInfo{
			State: tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}},
		},
	}
	return peer.NewContext(context.Background(), p)
}

func testEnrollment(t *testing.T) *Enrollment {
	t.Helper()
	e, err := ParseEnrollment([]string{"cluster-a=" + idClusterA, "cluster-b=" + idClusterB})
	if err != nil {
		t.Fatalf("ParseEnrollment: %v", err)
	}
	return e
}

func TestParseEnrollment(t *testing.T) {
	t.Parallel()
	e := testEnrollment(t)
	if got := e.SPIFFEIDs(); len(got) != 2 || got[0] != idClusterA || got[1] != idClusterB {
		t.Fatalf("SPIFFEIDs = %v", got)
	}
	for _, tc := range []struct {
		name    string
		entries []string
	}{
		{"empty list", nil},
		{"no separator", []string{"cluster-a"}},
		{"empty name", []string{"=" + idClusterA}},
		{"name is not a DNS label", []string{"Cluster_A=" + idClusterA}},
		{"not a SPIFFE ID", []string{"cluster-a=https://a.example/x"}},
		{"name enrolled twice", []string{"cluster-a=" + idClusterA, "cluster-a=" + idClusterB}},
		{"ID enrolled twice", []string{"cluster-a=" + idClusterA, "cluster-b=" + idClusterA}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseEnrollment(tc.entries); err == nil {
				t.Fatalf("ParseEnrollment(%v) accepted the list", tc.entries)
			}
		})
	}
}

func TestClientFromContext(t *testing.T) {
	t.Parallel()
	e := testEnrollment(t)
	for _, tc := range []struct {
		name string
		ctx  context.Context
		want codes.Code
	}{
		{"no peer", context.Background(), codes.Unauthenticated},
		{"not TLS", peer.NewContext(context.Background(), &peer.Peer{}), codes.Unauthenticated},
		{"no certificate", peer.NewContext(context.Background(),
			&peer.Peer{AuthInfo: credentials.TLSInfo{State: tls.ConnectionState{}}}), codes.Unauthenticated},
		// A DNS name that once gave a tenant gives nothing now.
		{"no SPIFFE ID", ctxWithCert(makeCert(t, "")), codes.PermissionDenied},
		{"not enrolled", ctxWithCert(makeCert(t, idStranger)), codes.PermissionDenied},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := e.clientFromContext(tc.ctx); status.Code(err) != tc.want {
				t.Fatalf("code = %s, want %s (%v)", status.Code(err), tc.want, err)
			}
		})
	}
	got, err := e.clientFromContext(ctxWithCert(makeCert(t, idClusterB)))
	if err != nil || got != "cluster-b" {
		t.Fatalf("client = %q, %v; want cluster-b", got, err)
	}
}

// pairResolver maps each pair to a namespace named after it.
type pairResolver struct{}

func (pairResolver) NamespaceFor(_ context.Context, p tenancy.Pair) (string, error) {
	return "sbx-" + p.Client + "-" + string(p.Tenant), nil
}

func sandboxIn(ns, name string) *setecv1alpha1.Sandbox {
	return &setecv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, UID: "uid-1"},
		Status:     setecv1alpha1.SandboxStatus{Phase: setecv1alpha1.SandboxPhaseCompleted},
	}
}

// TestScope_EachPairReachesOnlyItsOwnSandbox is the done-when test of
// setec#168: a different cluster with the same tenant, and the same cluster
// with a different tenant, are both refused.
func TestScope_EachPairReachesOnlyItsOwnSandbox(t *testing.T) {
	t.Parallel()
	owner := sandboxIn("sbx-cluster-a-acme", "job")
	s := &Service{
		Client:     newClient(t, owner),
		Enrollment: testEnrollment(t),
		Resolver:   pairResolver{},
	}
	id := owner.Namespace + "/" + owner.Name + "/uid-1"
	ctxA := ctxWithCert(makeCert(t, idClusterA))
	ctxB := ctxWithCert(makeCert(t, idClusterB))

	if _, err := s.Wait(ctxA, &setecv1grpc.WaitRequest{SandboxId: id, Tenant: "acme"}); err != nil {
		t.Fatalf("the owner pair: Wait: %v", err)
	}
	for _, tc := range []struct {
		name   string
		ctx    context.Context
		tenant string
		want   codes.Code
	}{
		{"a different cluster with the same tenant", ctxB, "acme", codes.PermissionDenied},
		{"the same cluster with a different tenant", ctxA, "globex", codes.PermissionDenied},
		{"an empty tenant", ctxA, "", codes.InvalidArgument},
		{"a tenant that is not a DNS label", ctxA, "acme/x", codes.InvalidArgument},
		{"a caller that is not enrolled", ctxWithCert(makeCert(t, idStranger)), "acme", codes.PermissionDenied},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, waitErr := s.Wait(tc.ctx, &setecv1grpc.WaitRequest{SandboxId: id, Tenant: tc.tenant})
			_, attachErr := s.Attach(tc.ctx, &setecv1grpc.AttachRequest{SandboxId: id, Tenant: tc.tenant})
			_, killErr := s.Kill(tc.ctx, &setecv1grpc.KillRequest{SandboxId: id, Tenant: tc.tenant})
			for verb, err := range map[string]error{"Wait": waitErr, "Attach": attachErr, "Kill": killErr} {
				if status.Code(err) != tc.want {
					t.Errorf("%s: code = %s, want %s (%v)", verb, status.Code(err), tc.want, err)
				}
			}
		})
	}
	// Kill was refused each time, so the Sandbox of the owner still exists.
	if _, err := s.Wait(ctxA, &setecv1grpc.WaitRequest{SandboxId: id, Tenant: "acme"}); err != nil {
		t.Fatalf("the Sandbox of the owner is gone after refused calls: %v", err)
	}
}

// TestLaunch_RecordsThePair proves that Launch places the Sandbox in the
// namespace of the pair and writes the pair on it.
func TestLaunch_RecordsThePair(t *testing.T) {
	t.Parallel()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "sbx-cluster-b-acme"}}
	s := &Service{
		Client:     newClient(t, ns),
		Enrollment: testEnrollment(t),
		Resolver:   pairResolver{},
	}
	resp, err := s.Launch(ctxWithCert(makeCert(t, idClusterB)), &setecv1grpc.LaunchRequest{
		Image: "busybox", Command: []string{"true"}, Tenant: "acme",
	})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if resp.GetNamespace() != "sbx-cluster-b-acme" {
		t.Fatalf("namespace = %q, want sbx-cluster-b-acme", resp.GetNamespace())
	}
	sb := &setecv1alpha1.Sandbox{}
	if err := s.Client.Get(context.Background(),
		types.NamespacedName{Namespace: resp.GetNamespace(), Name: resp.GetName()}, sb); err != nil {
		t.Fatalf("get Sandbox: %v", err)
	}
	if sb.Labels[tenancy.ClientLabelKey] != "cluster-b" || sb.Labels[tenancy.TenantLabelKey] != "acme" {
		t.Fatalf("labels = %v, want client cluster-b and tenant acme", sb.Labels)
	}
}
