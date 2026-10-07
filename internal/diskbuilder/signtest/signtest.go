// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

// Package signtest signs a test image as cosign v3 does: a Sigstore bundle
// over the digest of the image manifest, attached as an OCI referrer of
// the image. It signs with a new key, so a test needs no Sigstore instance.
//
// The package builds the bundle itself, with the protobuf types of the
// bundle format. The signer of sigstore-go imports the PGP code of Rekor,
// which imports golang.org/x/crypto/openpgp (GO-2026-5932, no fixed
// version). A test helper must not carry that import into the module.
package signtest

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/types"
	protobundle "github.com/sigstore/protobuf-specs/gen/pb-go/bundle/v1"
	protocommon "github.com/sigstore/protobuf-specs/gen/pb-go/common/v1"
	"google.golang.org/protobuf/encoding/protojson"
)

const (
	bundleMediaType = "application/vnd.dev.sigstore.bundle.v0.3+json"
	predicateKey    = "dev.sigstore.bundle.predicateType"
	signPredicate   = "https://sigstore.dev/cosign/sign/v1"
)

// SignWithNewKey signs the image at ref with a new key and attaches the
// signature. It returns the PEM public key of the signer.
func SignWithNewKey(ctx context.Context, ref name.Digest, opts ...remote.Option) (string, error) {
	opts = append(opts, remote.WithContext(ctx))
	desc, err := remote.Get(ref, opts...)
	if err != nil {
		return "", fmt.Errorf("get %s: %w", ref, err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", fmt.Errorf("make a key: %w", err)
	}
	b, err := signBundle(key, desc.Manifest)
	if err != nil {
		return "", fmt.Errorf("sign %s: %w", ref, err)
	}
	raw, err := protojson.Marshal(b)
	if err != nil {
		return "", fmt.Errorf("encode the bundle: %w", err)
	}
	art := mutate.ConfigMediaType(mutate.MediaType(empty.Image, types.OCIManifestSchema1), bundleMediaType)
	art, err = mutate.AppendLayers(art, static.NewLayer(raw, bundleMediaType))
	if err != nil {
		return "", fmt.Errorf("build the signature: %w", err)
	}
	art, ok := mutate.Annotations(art, map[string]string{predicateKey: signPredicate}).(v1.Image)
	if !ok {
		return "", fmt.Errorf("annotate the signature of %s", ref)
	}
	art, ok = mutate.Subject(art, desc.Descriptor).(v1.Image)
	if !ok {
		return "", fmt.Errorf("attach the signature of %s", ref)
	}
	d, err := art.Digest()
	if err != nil {
		return "", fmt.Errorf("digest of the signature: %w", err)
	}
	if err := remote.Write(ref.Context().Digest(d.String()), art, opts...); err != nil {
		return "", fmt.Errorf("push the signature of %s: %w", ref, err)
	}
	pub, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return "", fmt.Errorf("the public key: %w", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pub})), nil
}

// signBundle signs data with key as cosign does with a key file: a v0.3
// bundle with a message signature over the SHA-256 digest of data, and the
// fingerprint of the public key as the hint of the verification material.
func signBundle(key *ecdsa.PrivateKey, data []byte) (*protobundle.Bundle, error) {
	pub, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("the public key: %w", err)
	}
	hint := sha256.Sum256(pub)
	digest := sha256.Sum256(data)
	sig, err := ecdsa.SignASN1(rand.Reader, key, digest[:])
	if err != nil {
		return nil, fmt.Errorf("sign the digest: %w", err)
	}
	return &protobundle.Bundle{
		MediaType: bundleMediaType,
		VerificationMaterial: &protobundle.VerificationMaterial{
			Content: &protobundle.VerificationMaterial_PublicKey{
				PublicKey: &protocommon.PublicKeyIdentifier{
					Hint: base64.StdEncoding.EncodeToString(hint[:]),
				},
			},
		},
		Content: &protobundle.Bundle_MessageSignature{
			MessageSignature: &protocommon.MessageSignature{
				MessageDigest: &protocommon.HashOutput{
					Algorithm: protocommon.HashAlgorithm_SHA2_256,
					Digest:    digest[:],
				},
				Signature: sig,
			},
		},
	}, nil
}
