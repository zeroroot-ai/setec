// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package imagesig

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/sigstore/sigstore-go/pkg/testing/ca"

	"github.com/zeroroot-ai/setec/internal/diskbuilder/signtest"
)

const (
	testIssuer   = "https://token.actions.githubusercontent.com"
	testIdentity = "https://github.com/example/tools/.github/workflows/release.yml@refs/tags/v1"
)

// TestVerifyEntity_KeylessSignature covers a signed image and a signature
// of another identity. The virtual Sigstore issues a Fulcio certificate and
// a transparency log entry, as the public-good instance does.
func TestVerifyEntity_KeylessSignature(t *testing.T) {
	t.Parallel()
	vs, err := ca.NewVirtualSigstore()
	if err != nil {
		t.Fatal(err)
	}
	manifest := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json"}`)
	digest := sha256Hash(manifest)
	policy := SignaturePolicy{Issuer: testIssuer, Identity: testIdentity}

	signed, err := vs.Sign(testIdentity, testIssuer, manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyEntity(signed, digest, policy, vs); err != nil {
		t.Fatalf("a signature of the policy identity: %v", err)
	}

	other, err := vs.Sign("https://github.com/attacker/tools/.github/workflows/release.yml@refs/tags/v1", testIssuer, manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyEntity(other, digest, policy, vs); err == nil {
		t.Fatal("a signature of another identity verifies")
	}

	// A signature of the right identity over another manifest.
	if err := verifyEntity(signed, sha256Hash([]byte("another manifest")), policy, vs); err == nil {
		t.Fatal("a signature of another digest verifies")
	}
}

// TestVerifyImage_Unsigned covers an image with no signature referrer.
func TestVerifyImage_Unsigned(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(registry.New())
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	tag, _ := name.NewTag(u.Host + "/tools/scanner:v1")
	img := testImage(t)
	if err := remote.Write(tag, img); err != nil {
		t.Fatal(err)
	}
	dg, _ := img.Digest()
	ref := u.Host + "/tools/scanner@" + dg.String()

	err := VerifyImage(t.Context(), ref, SignaturePolicy{Issuer: testIssuer, Identity: testIdentity}, nil)
	if !errors.Is(err, ErrUnsigned) {
		t.Fatalf("VerifyImage of an unsigned image = %v, want ErrUnsigned", err)
	}
	if err := VerifyImage(t.Context(), u.Host+"/tools/scanner:v1", SignaturePolicy{Issuer: testIssuer, Identity: testIdentity}, nil); err == nil {
		t.Fatal("VerifyImage accepts an image by tag")
	}
}

// TestSignaturePolicy_Validate refuses a policy that is both or neither.
func TestSignaturePolicy_Validate(t *testing.T) {
	t.Parallel()
	for _, p := range []SignaturePolicy{
		{},
		{Issuer: testIssuer},
		{Issuer: testIssuer, Identity: testIdentity, PublicKey: []byte("k")},
	} {
		if p.Validate() == nil {
			t.Fatalf("policy %+v validates", p)
		}
	}
}

// sha256Hash is the sha256 digest of b.
func sha256Hash(b []byte) v1.Hash {
	h, _, err := v1.SHA256(bytes.NewReader(b))
	if err != nil {
		panic(err)
	}
	return h
}

// TestVerifyImage_KeySignature runs the whole path of a key signature:
// the signature is an OCI referrer of the image, VerifyImage finds it, and
// only the key of the signer verifies it.
func TestVerifyImage_KeySignature(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(registry.New())
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	tag, _ := name.NewTag(u.Host + "/tools/signed:v1")
	img := testImage(t)
	if err := remote.Write(tag, img); err != nil {
		t.Fatal(err)
	}
	dg, _ := img.Digest()
	ref, _ := name.NewDigest(u.Host + "/tools/signed@" + dg.String())
	pub, err := signtest.SignWithNewKey(t.Context(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyImage(t.Context(), ref.String(), SignaturePolicy{PublicKey: []byte(pub)}, nil); err != nil {
		t.Fatalf("VerifyImage with the key of the signer: %v", err)
	}
	stranger := strangerKey(t)
	if err := VerifyImage(t.Context(), ref.String(), SignaturePolicy{PublicKey: []byte(stranger)}, nil); !errors.Is(err, ErrNotVerified) {
		t.Fatalf("VerifyImage with another key = %v, want ErrNotVerified", err)
	}
}

// strangerKey is a PEM public key that signed nothing.
func strangerKey(t *testing.T) string {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&k.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
}

// testImage is a small random image.
func testImage(t *testing.T) v1.Image {
	t.Helper()
	img, err := random.Image(256, 2)
	if err != nil {
		t.Fatal(err)
	}
	return img
}
