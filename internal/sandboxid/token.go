// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

// Package sandboxid is the identity of a sandbox (setec#235): a short-lived
// token that names the sandbox and its tenant, signed with a key of that
// sandbox. The launcher holds the key outside the machine, so no process in
// the machine can sign, and a fork gets its own key. A snapshot of a sandbox
// raises its identity generation: a token from before the snapshot, which a
// fork can find in its copy of the memory, no longer verifies.
//
// The token is a compact JWS (RFC 7515) with the EdDSA algorithm (RFC
// 8037), so a verifier can use any JOSE library.
package sandboxid

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Issuer is the iss claim of each token.
const Issuer = "setec"

// Lifetime is how long a token is valid. A process asks for a new token
// for each call, so the lifetime only bounds a token that leaks.
const Lifetime = 5 * time.Minute

// Claims are the claims of a token.
type Claims struct {
	Issuer string `json:"iss"`
	// Subject is the sandbox id, <namespace>/<name>/<uid>.
	Subject string `json:"sub"`
	// Audience names the verifier the token is for.
	Audience string `json:"aud"`
	// Client and Tenant are the owner pair of the namespace of the
	// sandbox. Each is empty when the namespace has no such label.
	Client string `json:"setec.zeroroot.ai/client,omitempty"`
	Tenant string `json:"setec.zeroroot.ai/tenant,omitempty"`
	// Generation is the identity generation of the sandbox at signing.
	Generation int64 `json:"setec.zeroroot.ai/generation"`
	IssuedAt   int64 `json:"iat"`
	Expires    int64 `json:"exp"`
	// ID is unique for each token.
	ID string `json:"jti"`
}

type header struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
	Kid string `json:"kid"`
}

// The errors of Verify. Each wraps ErrInvalid.
var (
	ErrInvalid    = errors.New("sandboxid: the identity token is not valid")
	ErrExpired    = fmt.Errorf("%w: it expired", ErrInvalid)
	ErrGeneration = fmt.Errorf("%w: it is from an earlier generation of the sandbox", ErrInvalid)
)

var b64 = base64.RawURLEncoding

// KeyID is the kid of a public key: the first 16 bytes of its SHA-256, in hex.
func KeyID(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:16])
}

// Sign returns the token of c, signed with key.
func Sign(key ed25519.PrivateKey, c Claims) (string, error) {
	pub, ok := key.Public().(ed25519.PublicKey)
	if !ok || len(key) != ed25519.PrivateKeySize {
		return "", errors.New("sandboxid: the signing key is not an ed25519 key")
	}
	h, err := json.Marshal(header{Alg: "EdDSA", Typ: "JWT", Kid: KeyID(pub)})
	if err != nil {
		return "", err
	}
	p, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	input := b64.EncodeToString(h) + "." + b64.EncodeToString(p)
	return input + "." + b64.EncodeToString(ed25519.Sign(key, []byte(input))), nil
}

// Subject returns the sub claim of a token without a check of its
// signature, so a verifier can find the key of the sandbox. Nothing else
// may trust the value before Verify.
func Subject(token string) (string, error) {
	_, c, _, _, err := split(token)
	if err != nil {
		return "", err
	}
	return c.Subject, nil
}

// Expect is what a verifier knows about the sandbox of a token.
type Expect struct {
	// Key is the public key of the sandbox.
	Key ed25519.PublicKey
	// Subject is the sandbox id.
	Subject string
	// Audience is the audience of the verifier.
	Audience string
	// Generation is the current identity generation of the sandbox.
	Generation int64
	// Now is the time of the check.
	Now time.Time
}

// Verify checks the signature, the issuer, the subject, the audience, the
// lifetime and the generation of a token, and returns its claims.
func Verify(token string, want Expect) (Claims, error) {
	h, c, input, sig, err := split(token)
	if err != nil {
		return Claims{}, err
	}
	if h.Alg != "EdDSA" || h.Kid != KeyID(want.Key) || len(want.Key) != ed25519.PublicKeySize {
		return Claims{}, fmt.Errorf("%w: it is not signed with the key of the sandbox", ErrInvalid)
	}
	if !ed25519.Verify(want.Key, []byte(input), sig) {
		return Claims{}, fmt.Errorf("%w: the signature does not verify", ErrInvalid)
	}
	switch {
	case c.Issuer != Issuer:
		return Claims{}, fmt.Errorf("%w: the issuer is %q", ErrInvalid, c.Issuer)
	case c.Subject != want.Subject:
		return Claims{}, fmt.Errorf("%w: it names another sandbox", ErrInvalid)
	case c.Audience != want.Audience:
		return Claims{}, fmt.Errorf("%w: it is for the audience %q", ErrInvalid, c.Audience)
	case c.ID == "":
		return Claims{}, fmt.Errorf("%w: it has no jti", ErrInvalid)
	}
	now := want.Now.Unix()
	if now >= c.Expires || c.IssuedAt > now+int64(time.Minute/time.Second) {
		return Claims{}, ErrExpired
	}
	if c.Generation != want.Generation {
		return Claims{}, ErrGeneration
	}
	return c, nil
}

func split(token string) (header, Claims, string, []byte, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return header{}, Claims{}, "", nil, fmt.Errorf("%w: it is not a compact JWS", ErrInvalid)
	}
	var h header
	var c Claims
	hb, err1 := b64.DecodeString(parts[0])
	pb, err2 := b64.DecodeString(parts[1])
	sig, err3 := b64.DecodeString(parts[2])
	if err := errors.Join(err1, err2, err3); err != nil {
		return header{}, Claims{}, "", nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if err := json.Unmarshal(hb, &h); err != nil {
		return header{}, Claims{}, "", nil, fmt.Errorf("%w: the header: %v", ErrInvalid, err)
	}
	if err := json.Unmarshal(pb, &c); err != nil {
		return header{}, Claims{}, "", nil, fmt.Errorf("%w: the claims: %v", ErrInvalid, err)
	}
	return h, c, parts[0] + "." + parts[1], sig, nil
}

// KeyFromSeed returns the key of a base64 ed25519 seed, as the identity
// Secret of a sandbox holds it.
func KeyFromSeed(seed string) (ed25519.PrivateKey, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(seed))
	if err != nil || len(raw) != ed25519.SeedSize {
		return nil, errors.New("sandboxid: the identity seed is not a base64 ed25519 seed")
	}
	return ed25519.NewKeyFromSeed(raw), nil
}

// PublicKey decodes the base64 public key that the status of a sandbox holds.
func PublicKey(s string) (ed25519.PublicKey, error) {
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return nil, errors.New("sandboxid: the identity public key is not a base64 ed25519 key")
	}
	return raw, nil
}
