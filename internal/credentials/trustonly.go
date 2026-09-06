// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package credentials

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"

	grpccreds "google.golang.org/grpc/credentials"
)

// minTrustOnlyTLSVersion is the floor for the one-way surface. It is
// deliberately one notch below the mTLS floor.
//
// The peer on this surface is a third-party endpoint outside setec's
// control — today an OTLP collector, which is frequently a vendor
// gateway or a TLS-terminating proxy the operator does not own.
// Requiring TLS 1.3 of it would take telemetry away from operators who
// cannot change their collector, for a hop that carries spans rather
// than the authority to run a microVM. The mTLS floor stays at 1.3 and
// is not negotiable; this one is a separate, narrower promise, and it
// is stated here rather than buried in a caller so that raising it is
// a one-line reviewable change.
const minTrustOnlyTLSVersion = tls.VersionTLS12

// TrustOnly configures a one-way TLS surface: setec verifies the peer
// but presents no identity of its own.
//
// This is emphatically not mTLS, and nothing on this surface should be
// trusted to have proved who setec is. It exists for endpoints that
// are not part of setec's mTLS mesh and that setec only consumes — the
// telemetry exporter. A surface where the peer must know who setec is
// belongs on Provider, which requires an identity by construction.
type TrustOnly struct {
	// CAFile is a PEM bundle of trust anchors used to verify the peer.
	// Empty means the host's root store, which is the right default
	// for a collector fronted by a publicly-issued certificate.
	//
	// A CAFile that is set but unreadable or unparseable is an error
	// rather than a fall back to the host roots: an operator who
	// mounted a bundle asked for that bundle, and silently widening
	// the trust set is the failure this package exists to prevent.
	CAFile string
}

// TrustOnlyCredentials returns transport credentials for a one-way TLS
// surface described by cfg.
//
// It is a function rather than a Provider method because there is no
// identity to acquire, hold, or rotate — asking a Provider for these
// would mean a Provider that might have no identity, which would
// weaken every mTLS caller's guarantee for the benefit of the one
// surface that does not need it.
func TrustOnlyCredentials(cfg TrustOnly) (grpccreds.TransportCredentials, error) {
	tlsCfg := &tls.Config{MinVersion: minTrustOnlyTLSVersion}
	if cfg.CAFile != "" {
		pem, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, fmt.Errorf("read CA file %q: %w", cfg.CAFile, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("CA file %q contains no usable certificates", cfg.CAFile)
		}
		tlsCfg.RootCAs = pool
	}
	return grpccreds.NewTLS(tlsCfg), nil
}
