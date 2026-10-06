// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package credentials

import (
	"crypto/x509"
	"errors"
	"fmt"

	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/svid/x509svid"
)

// ParseSPIFFEID checks that raw is a SPIFFE ID and returns its canonical
// form. Code outside this package reads SPIFFE IDs only through this
// function and PeerSPIFFEID, so the SPIFFE library stays in one package.
func ParseSPIFFEID(raw string) (string, error) {
	id, err := spiffeid.FromString(raw)
	if err != nil {
		return "", fmt.Errorf("credentials: %q is not a SPIFFE ID: %w", raw, err)
	}
	return id.String(), nil
}

// PeerSPIFFEID returns the SPIFFE ID in the URI SAN of a verified peer
// certificate, in canonical form.
func PeerSPIFFEID(cert *x509.Certificate) (string, error) {
	if cert == nil {
		return "", errors.New("credentials: no peer certificate")
	}
	id, err := x509svid.IDFromCert(cert)
	if err != nil {
		return "", fmt.Errorf("credentials: the peer certificate carries no SPIFFE ID: %w", err)
	}
	return id.String(), nil
}
