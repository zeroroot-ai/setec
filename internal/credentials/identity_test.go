// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package credentials

import (
	"crypto/x509"
	"net/url"
	"testing"
)

func TestParseSPIFFEID(t *testing.T) {
	t.Parallel()
	got, err := ParseSPIFFEID("spiffe://example.org/ns/gibson/sa/daemon")
	if err != nil || got != "spiffe://example.org/ns/gibson/sa/daemon" {
		t.Fatalf("ParseSPIFFEID = %q, %v", got, err)
	}
	if _, err := ParseSPIFFEID("https://example.org/daemon"); err == nil {
		t.Fatal("an https URL is not a SPIFFE ID: want an error")
	}
}

func TestPeerSPIFFEID(t *testing.T) {
	t.Parallel()
	u, err := url.Parse("spiffe://example.org/ns/gibson/sa/daemon")
	if err != nil {
		t.Fatal(err)
	}
	got, err := PeerSPIFFEID(&x509.Certificate{URIs: []*url.URL{u}})
	if err != nil || got != u.String() {
		t.Fatalf("PeerSPIFFEID = %q, %v", got, err)
	}
	if _, err := PeerSPIFFEID(&x509.Certificate{}); err == nil {
		t.Fatal("a certificate with no URI SAN: want an error")
	}
	if _, err := PeerSPIFFEID(nil); err == nil {
		t.Fatal("no certificate: want an error")
	}
}
