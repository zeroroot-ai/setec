// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package diskbuilder

import "errors"

// SignaturePolicy names who must have signed an image: a keyless signer
// (the OIDC issuer and the identity of its certificate), or the holder of
// a public key. Exactly one of the two is set.
type SignaturePolicy struct {
	// Issuer is the OIDC issuer of a keyless signature, for example
	// https://token.actions.githubusercontent.com.
	Issuer string
	// Identity is the identity of a keyless signature, for example the
	// URI of the release workflow of the image owner.
	Identity string
	// PublicKey is a PEM public key, for an image signed with a key.
	PublicKey []byte
}

// Validate reports a policy that is neither keyless nor a key, or both.
func (p SignaturePolicy) Validate() error {
	keyless := p.Issuer != "" || p.Identity != ""
	switch {
	case keyless && len(p.PublicKey) > 0:
		return errors.New("a signature policy is keyless or a public key, not both")
	case keyless && (p.Issuer == "" || p.Identity == ""):
		return errors.New("a keyless signature policy needs the issuer and the identity")
	case !keyless && len(p.PublicKey) == 0:
		return errors.New("a signature policy needs a keyless issuer and identity, or a public key")
	}
	return nil
}
