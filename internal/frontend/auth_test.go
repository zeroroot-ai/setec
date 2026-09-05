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

package frontend

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

func makeCert(t *testing.T, dns []string) *x509.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     dns,
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

func TestTenantFromContext_NoPeer(t *testing.T) {
	t.Parallel()
	_, err := TenantFromContext(context.Background())
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("code = %s, want Unauthenticated", status.Code(err))
	}
}

func TestTenantFromContext_NotTLS(t *testing.T) {
	t.Parallel()
	p := &peer.Peer{AuthInfo: nil}
	ctx := peer.NewContext(context.Background(), p)
	_, err := TenantFromContext(ctx)
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("code = %s, want Unauthenticated", status.Code(err))
	}
}

func TestTenantFromContext_NoPeerCert(t *testing.T) {
	t.Parallel()
	p := &peer.Peer{AuthInfo: credentials.TLSInfo{State: tls.ConnectionState{}}}
	ctx := peer.NewContext(context.Background(), p)
	_, err := TenantFromContext(ctx)
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("code = %s, want Unauthenticated", status.Code(err))
	}
}

func TestTenantFromContext_HappyPath(t *testing.T) {
	t.Parallel()
	cert := makeCert(t, []string{"tenant-a.svc"})
	tid, err := TenantFromContext(ctxWithCert(cert))
	if err != nil {
		t.Fatalf("TenantFromContext: %v", err)
	}
	if tid != "tenant-a" {
		t.Fatalf("tenant = %q, want tenant-a", tid)
	}
}

func TestTenantFromContext_NoIdentityInCert(t *testing.T) {
	t.Parallel()
	cert := makeCert(t, nil)
	_, err := TenantFromContext(ctxWithCert(cert))
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("code = %s, want PermissionDenied", status.Code(err))
	}
}
