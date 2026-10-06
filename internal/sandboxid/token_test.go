// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package sandboxid

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"
)

func newKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

func claims(now time.Time) Claims {
	return Claims{Issuer: Issuer, Subject: "ns/sb/uid-1", Audience: "gibson", Tenant: "acme",
		Generation: 2, IssuedAt: now.Unix(), Expires: now.Add(Lifetime).Unix(), ID: "j1"}
}

// TestVerify_AcceptsItsOwnToken proves a round trip and the claims a
// verifier reads.
func TestVerify_AcceptsItsOwnToken(t *testing.T) {
	t.Parallel()
	pub, priv := newKey(t)
	now := time.Now()
	tok, err := Sign(priv, claims(now))
	if err != nil {
		t.Fatal(err)
	}
	if sub, err := Subject(tok); err != nil || sub != "ns/sb/uid-1" {
		t.Fatalf("Subject = %q, %v", sub, err)
	}
	c, err := Verify(tok, Expect{Key: pub, Subject: "ns/sb/uid-1", Audience: "gibson", Generation: 2, Now: now})
	if err != nil || c.Tenant != "acme" || c.ID != "j1" {
		t.Fatalf("Verify = %+v, %v", c, err)
	}
}

// TestVerify_RefusesEachForgery proves each refusal: another key (a fork
// signs with its own key), another subject, another audience, an old
// generation (a token from before a snapshot), an expired token, and a
// changed payload.
func TestVerify_RefusesEachForgery(t *testing.T) {
	t.Parallel()
	pub, priv := newKey(t)
	_, forkPriv := newKey(t)
	now := time.Now()
	good := Expect{Key: pub, Subject: "ns/sb/uid-1", Audience: "gibson", Generation: 2, Now: now}
	tok, _ := Sign(priv, claims(now))
	forkTok, _ := Sign(forkPriv, claims(now))

	other := claims(now)
	other.Subject = "ns/other/uid-2"
	otherTok, _ := Sign(priv, other)

	parts := strings.Split(tok, ".")
	tampered := parts[0] + "." + b64.EncodeToString([]byte(`{"iss":"setec","sub":"ns/sb/uid-1","aud":"gibson","setec.zeroroot.ai/generation":2,"iat":1,"exp":9999999999,"jti":"x"}`)) + "." + parts[2]

	cases := map[string]struct {
		token string
		want  Expect
		err   error
	}{
		"a fork key":          {forkTok, good, ErrInvalid},
		"another subject":     {otherTok, good, ErrInvalid},
		"another audience":    {tok, Expect{Key: pub, Subject: good.Subject, Audience: "x", Generation: 2, Now: now}, ErrInvalid},
		"an older generation": {tok, Expect{Key: pub, Subject: good.Subject, Audience: "gibson", Generation: 3, Now: now}, ErrGeneration},
		"expired":             {tok, Expect{Key: pub, Subject: good.Subject, Audience: "gibson", Generation: 2, Now: now.Add(Lifetime + time.Second)}, ErrExpired},
		"a changed payload":   {tampered, good, ErrInvalid},
		"not a token":         {"abc", good, ErrInvalid},
	}
	for name, tc := range cases {
		if _, err := Verify(tc.token, tc.want); !errors.Is(err, tc.err) {
			t.Errorf("%s: Verify = %v, want %v", name, err, tc.err)
		}
	}
}

// TestKeyFromSeed_MatchesThePublicKey proves the seed and the public key
// forms of the identity Secret and the status.
func TestKeyFromSeed_MatchesThePublicKey(t *testing.T) {
	t.Parallel()
	seed := make([]byte, ed25519.SeedSize)
	seed[0] = 7
	key, err := KeyFromSeed(b64std(seed))
	if err != nil {
		t.Fatal(err)
	}
	pub, err := PublicKey(b64std(key.Public().(ed25519.PublicKey)))
	if err != nil || !pub.Equal(key.Public()) {
		t.Fatalf("PublicKey = %v, %v", pub, err)
	}
	if _, err := KeyFromSeed("short"); err == nil {
		t.Fatal("KeyFromSeed accepted a bad seed")
	}
}

func b64std(b []byte) string { return base64.StdEncoding.EncodeToString(b) }
