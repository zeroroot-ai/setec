// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package imagesig

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/static"
)

// simpleKey is a key that signs in the simple signing format, as a KMS key
// does for "cosign sign --key awskms://...".
type simpleKey struct {
	priv *ecdsa.PrivateKey
	pem  []byte
}

func newSimpleKey(t *testing.T) simpleKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&k.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return simpleKey{priv: k, pem: pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})}
}

// payload is the signed text of the simple signing format for the digest.
func payload(digest string) []byte {
	return []byte(fmt.Sprintf(`{"critical":{"identity":{"docker-reference":"example.test/tools/signed"},`+
		`"image":{"docker-manifest-digest":%q},"type":"cosign container image signature"},"optional":null}`, digest))
}

// pushSimpleSignature writes one signature to the ".sig" tag of ref. text is
// the payload, and signedText is what the key signs, so a test can sign one
// text and ship another.
func pushSimpleSignature(t *testing.T, ref name.Digest, key simpleKey, text, signedText []byte) {
	t.Helper()
	sum := sha256.Sum256(signedText)
	sig, err := ecdsa.SignASN1(rand.Reader, key.priv, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	layer := static.NewLayer(text, "application/vnd.dev.cosign.simplesigning.v1+json")
	img, err := mutate.Append(empty.Image, mutate.Addendum{
		Layer:       layer,
		Annotations: map[string]string{simpleSigningSignatureAnnotation: base64.StdEncoding.EncodeToString(sig)},
	})
	if err != nil {
		t.Fatal(err)
	}
	d, err := v1Hash(ref)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(simpleSigningTag(ref.Context(), d), img); err != nil {
		t.Fatal(err)
	}
}

// signedImage pushes a random image to a registry that serves h, or a plain
// registry for a nil handler, and returns its reference by digest.
func signedImage(t *testing.T, wrap func(http.Handler) http.Handler) name.Digest {
	t.Helper()
	var h http.Handler = registry.New()
	if wrap != nil {
		h = wrap(h)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	tag, _ := name.NewTag(u.Host + "/tools/signed:v1")
	img := testImage(t)
	if err := remote.Write(tag, img); err != nil {
		t.Fatal(err)
	}
	dg, _ := img.Digest()
	ref, err := name.NewDigest(u.Host + "/tools/signed@" + dg.String())
	if err != nil {
		t.Fatal(err)
	}
	return ref
}

// TestVerifyImage_SimpleSigning covers a key signature with no transparency log
// entry, in the format that "cosign sign --key" writes before cosign v3 and
// that a KMS key signs (setec#409).
func TestVerifyImage_SimpleSigning(t *testing.T) {
	t.Parallel()
	ref := signedImage(t, nil)
	key := newSimpleKey(t)
	text := payload(ref.DigestStr())
	pushSimpleSignature(t, ref, key, text, text)

	if err := VerifyImage(t.Context(), ref.String(), SignaturePolicy{PublicKey: key.pem}, nil); err != nil {
		t.Fatalf("VerifyImage with the key of the signer: %v", err)
	}
	// Another key does not verify.
	other := newSimpleKey(t)
	if err := VerifyImage(t.Context(), ref.String(), SignaturePolicy{PublicKey: other.pem}, nil); !errors.Is(err, ErrNotVerified) {
		t.Fatalf("VerifyImage with another key = %v, want ErrNotVerified", err)
	}
	// A keyless policy cannot read the format: it needs a certificate and a log
	// entry. The result names what it looked for.
	err := VerifyImage(t.Context(), ref.String(), SignaturePolicy{Issuer: testIssuer, Identity: testIdentity}, nil)
	if !errors.Is(err, ErrUnsigned) || strings.Contains(err.Error(), ".sig") {
		t.Fatalf("a keyless policy = %v, want ErrUnsigned that does not name the .sig tag", err)
	}
}

// TestVerifyImage_SimpleSigningRefusals: a payload for another image, a text
// that the key did not sign, and a payload of another type do not verify.
func TestVerifyImage_SimpleSigningRefusals(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		text   func(digest string) []byte
		signed func(digest string) []byte
	}{
		"another digest": {
			text:   func(string) []byte { return payload("sha256:" + strings.Repeat("0", 64)) },
			signed: func(string) []byte { return payload("sha256:" + strings.Repeat("0", 64)) },
		},
		"text the key did not sign": {
			text:   payload,
			signed: func(d string) []byte { return payload(d + " ") },
		},
		"another type": {
			text: func(d string) []byte {
				return []byte(`{"critical":{"image":{"docker-manifest-digest":"` + d + `"},"type":"other"}}`)
			},
			signed: func(d string) []byte {
				return []byte(`{"critical":{"image":{"docker-manifest-digest":"` + d + `"},"type":"other"}}`)
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ref := signedImage(t, nil)
			key := newSimpleKey(t)
			pushSimpleSignature(t, ref, key, tc.text(ref.DigestStr()), tc.signed(ref.DigestStr()))
			err := VerifyImage(t.Context(), ref.String(), SignaturePolicy{PublicKey: key.pem}, nil)
			if !errors.Is(err, ErrNotVerified) {
				t.Fatalf("VerifyImage = %v, want ErrNotVerified", err)
			}
		})
	}
}

// TestVerifyImage_UnsignedNamesWhatItLookedFor: with no signature at all, the
// result says that it looked for a bundle and for the ".sig" tag, so that the
// reader can tell a missing signature from an unsupported format.
func TestVerifyImage_UnsignedNamesWhatItLookedFor(t *testing.T) {
	t.Parallel()
	ref := signedImage(t, nil)
	err := VerifyImage(t.Context(), ref.String(), SignaturePolicy{PublicKey: newSimpleKey(t).pem}, nil)
	if !errors.Is(err, ErrUnsigned) {
		t.Fatalf("VerifyImage = %v, want ErrUnsigned", err)
	}
	for _, want := range []string{"Sigstore bundle", "sha256-" + ref.DigestStr()[len("sha256:"):] + ".sig"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the error %q does not name %q", err, want)
		}
	}
}

// TestVerifyImage_SignatureTagError: a registry that fails on the ".sig" tag is
// not an image with no signature.
func TestVerifyImage_SignatureTagError(t *testing.T) {
	t.Parallel()
	ref := signedImage(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, ".sig") && strings.Contains(r.URL.Path, "/manifests/") {
				http.Error(w, "boom", http.StatusInternalServerError)
				return
			}
			next.ServeHTTP(w, r)
		})
	})
	err := VerifyImage(t.Context(), ref.String(), SignaturePolicy{PublicKey: newSimpleKey(t).pem}, nil)
	if err == nil || errors.Is(err, ErrUnsigned) {
		t.Fatalf("VerifyImage = %v, want a registry error that is not ErrUnsigned", err)
	}
}

// v1Hash is the hash of the digest of ref.
func v1Hash(ref name.Digest) (v1.Hash, error) { return v1.NewHash(ref.DigestStr()) }
