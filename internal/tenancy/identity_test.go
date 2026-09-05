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

package tenancy

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"net/url"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// newTestCert constructs a self-signed certificate with the given SANs and
// subject CN. Each test case calls this helper to get a fresh x509 pointer
// without any real CA ceremony.
func newTestCert(t *testing.T, cn string, dnsNames []string, uris []string) *x509.Certificate {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	parsedURIs := make([]*url.URL, 0, len(uris))
	for _, u := range uris {
		parsed, err := url.Parse(u)
		if err != nil {
			t.Fatalf("parse URI %q: %v", u, err)
		}
		parsedURIs = append(parsedURIs, parsed)
	}

	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     dnsNames,
		URIs:         parsedURIs,
	}

	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse cert: %v", err)
	}
	return cert
}

func TestFromNamespace(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		ns        *corev1.Namespace
		labelKey  string
		want      TenantID
		wantErrIs error
	}{
		{
			name: "label set",
			ns: &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "tenant-a",
					Labels: map[string]string{"setec.zeroroot.ai/tenant": "tenant-a"},
				},
			},
			labelKey: "setec.zeroroot.ai/tenant",
			want:     "tenant-a",
		},
		{
			name: "label missing",
			ns: &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "nolabel",
					Labels: map[string]string{"other": "value"},
				},
			},
			labelKey:  "setec.zeroroot.ai/tenant",
			wantErrIs: ErrTenantLabelMissing,
		},
		{
			name: "label present but empty",
			ns: &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "empty",
					Labels: map[string]string{"setec.zeroroot.ai/tenant": ""},
				},
			},
			labelKey:  "setec.zeroroot.ai/tenant",
			wantErrIs: ErrTenantLabelMissing,
		},
		{
			name: "label value not a DNS label",
			ns: &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "bad",
					Labels: map[string]string{"setec.zeroroot.ai/tenant": "NOT_VALID"},
				},
			},
			labelKey:  "setec.zeroroot.ai/tenant",
			wantErrIs: ErrTenantInvalid,
		},
		{
			name:      "nil namespace",
			ns:        nil,
			labelKey:  "setec.zeroroot.ai/tenant",
			wantErrIs: ErrTenantLabelMissing,
		},
		{
			name: "empty label key",
			ns: &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "ns",
					Labels: map[string]string{"setec.zeroroot.ai/tenant": "tenant-a"},
				},
			},
			labelKey:  "",
			wantErrIs: ErrTenantLabelMissing,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := FromNamespace(tc.ns, tc.labelKey)
			if tc.wantErrIs != nil {
				if err == nil {
					t.Fatalf("expected error %v, got nil", tc.wantErrIs)
				}
				if !errors.Is(err, tc.wantErrIs) {
					t.Fatalf("err = %v, want errors.Is(%v)", err, tc.wantErrIs)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestFromCertificate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		cert     *x509.Certificate
		want     TenantID
		wantErr  bool
		errCheck error
	}{
		{
			name:    "nil cert",
			cert:    nil,
			wantErr: true, errCheck: ErrTenantSANMissing,
		},
		{
			name: "SPIFFE URI SAN wins over DNS and CN",
			cert: func() *x509.Certificate {
				return &x509.Certificate{} // placeholder; overwritten below
			}(),
			want: "tenant-a",
		},
		{
			name: "DNS SAN used when no SPIFFE",
			want: "team-a",
		},
		{
			name: "CN used when no SANs",
			want: "legacy",
		},
		{
			name:    "no identity at all",
			wantErr: true, errCheck: ErrTenantSANMissing,
		},
		{
			name:    "extracted identity not a DNS label",
			wantErr: true, errCheck: ErrTenantInvalid,
		},
	}

	// Per-case cert construction kept out of the table because x509 test
	// certs require runtime key generation; constructing them in a
	// separate switch keeps each case's intent clear.
	for i := range tests {
		switch tests[i].name {
		case "SPIFFE URI SAN wins over DNS and CN":
			tests[i].cert = newTestCert(t, "cn.example",
				[]string{"not-this.example.com"},
				[]string{"spiffe://example.org/tenant-a/workload-1"})
		case "DNS SAN used when no SPIFFE":
			tests[i].cert = newTestCert(t, "cn.example",
				[]string{"team-a.svc.cluster.local"}, nil)
		case "CN used when no SANs":
			tests[i].cert = newTestCert(t, "legacy.example.com", nil, nil)
		case "no identity at all":
			tests[i].cert = newTestCert(t, "", nil, nil)
		case "extracted identity not a DNS label":
			tests[i].cert = newTestCert(t, "NOT_VALID", nil, nil)
		}
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := FromCertificate(tc.cert)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got tenant %q", got)
				}
				if tc.errCheck != nil && !errors.Is(err, tc.errCheck) {
					t.Fatalf("err = %v, want errors.Is(%v)", err, tc.errCheck)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestFromCertificate_PrecedenceEdges covers SPIFFE shapes the precedence
// logic must handle gracefully without panicking or returning partial IDs.
func TestFromCertificate_PrecedenceEdges(t *testing.T) {
	t.Parallel()

	t.Run("SPIFFE trust domain without path falls through", func(t *testing.T) {
		// spiffe://example.org has no workload segment; should fall
		// through to DNS SAN or CN.
		cert := newTestCert(t, "",
			[]string{"team-b.svc"},
			[]string{"spiffe://example.org"})
		got, err := FromCertificate(cert)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "team-b" {
			t.Fatalf("got %q, want %q", got, "team-b")
		}
	})

	t.Run("SPIFFE single segment is used as tenant", func(t *testing.T) {
		cert := newTestCert(t, "", nil,
			[]string{"spiffe://example.org/just-tenant"})
		got, err := FromCertificate(cert)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "just-tenant" {
			t.Fatalf("got %q, want %q", got, "just-tenant")
		}
	})

	t.Run("DNS SAN with no dots uses whole label", func(t *testing.T) {
		cert := newTestCert(t, "", []string{"shortname"}, nil)
		got, err := FromCertificate(cert)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "shortname" {
			t.Fatalf("got %q, want %q", got, "shortname")
		}
	})

	t.Run("CN single label is used when no dots", func(t *testing.T) {
		cert := newTestCert(t, "legacy", nil, nil)
		got, err := FromCertificate(cert)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "legacy" {
			t.Fatalf("got %q, want %q", got, "legacy")
		}
	})
}

// TestTenantID_String exercises the String() helper so coverage reporting
// flags it as covered and future refactors don't accidentally break the
// plain-string rendering callers depend on.
func TestTenantID_String(t *testing.T) {
	t.Parallel()
	if TenantID("foo").String() != "foo" {
		t.Fatalf("String() mismatch")
	}
}

// TestPickCertTenant_DefensivePaths covers the nil-URI-in-slice and
// empty-DNS-name-in-slice guards; real x509 ParseCertificate never returns
// these but pickCertTenant must not panic if a caller assembles a cert
// manually (as test helpers sometimes do).
func TestPickCertTenant_DefensivePaths(t *testing.T) {
	t.Parallel()

	t.Run("nil entry in URIs slice is skipped", func(t *testing.T) {
		u, err := url.Parse("spiffe://example.org/tenant-c")
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		cert := &x509.Certificate{
			URIs:     []*url.URL{nil, u},
			DNSNames: []string{"fallback"},
		}
		got := pickCertTenant(cert)
		if got != "tenant-c" {
			t.Fatalf("got %q, want tenant-c", got)
		}
	})

	t.Run("empty string in DNSNames slice is skipped", func(t *testing.T) {
		cert := &x509.Certificate{
			DNSNames: []string{"", "team-d.example"},
		}
		got := pickCertTenant(cert)
		if got != "team-d" {
			t.Fatalf("got %q, want team-d", got)
		}
	})

	t.Run("non-SPIFFE URI is skipped", func(t *testing.T) {
		u, err := url.Parse("https://example.org/not-spiffe")
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		cert := &x509.Certificate{
			URIs:     []*url.URL{u},
			DNSNames: []string{"team-e.example"},
		}
		got := pickCertTenant(cert)
		if got != "team-e" {
			t.Fatalf("got %q, want team-e", got)
		}
	})
}
