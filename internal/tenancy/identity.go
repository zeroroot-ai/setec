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

// Package tenancy owns the extraction of a TenantID from a Kubernetes
// namespace label or a TLS peer certificate. A TenantID is an opaque
// string; callers use it as a label value (on owned objects) and as an
// authorization check (on the gRPC frontend), so every returned value is
// validated to be safe as a DNS-1123 label.
//
// This package has no controller-runtime or client-go imports: the
// controller and frontend both compose it but all I/O stays outside.
package tenancy

import (
	"crypto/x509"
	"errors"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation"
)

// TenantID is an opaque identifier for a tenant. Callers MUST NOT depend on
// any particular encoding; the type exists to prevent accidental mixing of
// tenant IDs with arbitrary strings.
type TenantID string

// String renders the TenantID as a plain string. Use sparingly — prefer
// passing TenantID values by type so the compiler enforces tenant boundaries.
func (t TenantID) String() string { return string(t) }

// Sentinel errors. Callers classify failures via errors.Is to drive
// admission-layer messages or gRPC status codes (PERMISSION_DENIED vs.
// UNAUTHENTICATED vs. INVALID_ARGUMENT).
var (
	// ErrTenantLabelMissing is returned when a namespace has no value for
	// the configured tenant-label key, or the value is empty.
	ErrTenantLabelMissing = errors.New("tenancy: namespace is missing tenant label")

	// ErrTenantSANMissing is returned when a TLS peer certificate carries
	// no identifying SAN or CommonName from which to derive a TenantID.
	ErrTenantSANMissing = errors.New("tenancy: certificate carries no tenant identity")

	// ErrTenantInvalid is returned when the extracted tenant value does
	// not conform to DNS-1123 label syntax, which means it cannot safely
	// be used as a Kubernetes label value.
	ErrTenantInvalid = errors.New("tenancy: tenant identity is not a valid DNS label")
)

// spiffeScheme is the prefix SPIFFE IDs carry in a cert URI SAN. When
// present we prefer the SPIFFE trust-domain path over DNS SANs and CN,
// following the usual "most-specific identifier wins" convention.
const spiffeScheme = "spiffe://"

// FromNamespace extracts the tenant identity from a Kubernetes namespace
// by reading the given label key. Returns ErrTenantLabelMissing if the
// label is absent or empty, and ErrTenantInvalid if the value is not a
// valid DNS-1123 label.
//
// The function never returns sensitive data in its error messages —
// namespace names and label keys are the only fields echoed back, which is
// already public cluster metadata.
func FromNamespace(ns *corev1.Namespace, labelKey string) (TenantID, error) {
	if ns == nil {
		return "", fmt.Errorf("%w: namespace is nil", ErrTenantLabelMissing)
	}
	if labelKey == "" {
		return "", fmt.Errorf("%w: label key is empty", ErrTenantLabelMissing)
	}
	value, ok := ns.Labels[labelKey]
	if !ok || value == "" {
		return "", fmt.Errorf("%w: namespace %q has no %q label",
			ErrTenantLabelMissing, ns.Name, labelKey)
	}
	if errs := validation.IsDNS1123Label(value); len(errs) != 0 {
		return "", fmt.Errorf("%w: namespace %q label %q value fails validation",
			ErrTenantInvalid, ns.Name, labelKey)
	}
	return TenantID(value), nil
}

// FromCertificate extracts the tenant identity from a TLS peer certificate
// following a deterministic precedence:
//
//  1. First URI SAN with scheme "spiffe://" — the trust-domain-relative
//     path (everything after the host) is taken as the tenant.
//  2. First DNS SAN.
//  3. Subject.CommonName.
//
// If none of the above yield a non-empty string, ErrTenantSANMissing is
// returned. Whichever source wins, the value is validated against DNS-1123
// label syntax to guarantee it is safe as a Kubernetes label value.
//
// The error message never includes cert contents — only the position and
// source of failure — so logs do not leak subjects or DNs.
func FromCertificate(peerCert *x509.Certificate) (TenantID, error) {
	if peerCert == nil {
		return "", fmt.Errorf("%w: certificate is nil", ErrTenantSANMissing)
	}

	candidate := pickCertTenant(peerCert)
	if candidate == "" {
		return "", ErrTenantSANMissing
	}
	if errs := validation.IsDNS1123Label(candidate); len(errs) != 0 {
		return "", fmt.Errorf("%w: extracted identity is not a DNS label", ErrTenantInvalid)
	}
	return TenantID(candidate), nil
}

// pickCertTenant walks the precedence chain described on FromCertificate.
// It is factored out so tests of the precedence ordering can run without
// re-deriving the full FromCertificate signature.
func pickCertTenant(cert *x509.Certificate) string {
	// (1) SPIFFE URI SAN. A SPIFFE ID looks like
	//     spiffe://example.org/tenant-a; we take the first path segment
	//     below the trust domain as the tenant value.
	for _, u := range cert.URIs {
		if u == nil {
			continue
		}
		raw := u.String()
		if !strings.HasPrefix(raw, spiffeScheme) {
			continue
		}
		// After the scheme the first segment is the trust domain; the
		// rest is the workload path. We use the first non-empty path
		// segment as the tenant identifier.
		rest := strings.TrimPrefix(raw, spiffeScheme)
		if idx := strings.Index(rest, "/"); idx >= 0 {
			rest = rest[idx+1:]
		} else {
			rest = ""
		}
		if rest == "" {
			continue
		}
		// Take only the first path segment so "tenant-a/workload-1"
		// yields "tenant-a".
		if idx := strings.Index(rest, "/"); idx > 0 {
			rest = rest[:idx]
		}
		if rest != "" {
			return rest
		}
	}

	// (2) DNS SAN. We use the left-most label of the first DNS SAN.
	// A cert bearing "team-a.svc.cluster.local" yields "team-a".
	for _, dns := range cert.DNSNames {
		if dns == "" {
			continue
		}
		if idx := strings.Index(dns, "."); idx > 0 {
			return dns[:idx]
		}
		return dns
	}

	// (3) CommonName. Some legacy clients only populate CN. Use it as
	// last resort.
	if cn := strings.TrimSpace(cert.Subject.CommonName); cn != "" {
		if idx := strings.Index(cn, "."); idx > 0 {
			return cn[:idx]
		}
		return cn
	}

	return ""
}
