// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

// Package signtest signs a test image as cosign v3 does: a Sigstore bundle
// over the digest of the image manifest, attached as an OCI referrer of
// the image. It signs with a new key, so a test needs no Sigstore instance.
package signtest

import (
	"context"
	"fmt"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"github.com/sigstore/sigstore-go/pkg/sign"
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
	kp, err := sign.NewEphemeralKeypair(nil)
	if err != nil {
		return "", fmt.Errorf("make a key: %w", err)
	}
	b, err := sign.Bundle(&sign.PlainData{Data: desc.Manifest}, kp, sign.BundleOptions{Context: ctx})
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
	pem, err := kp.GetPublicKeyPem()
	if err != nil {
		return "", fmt.Errorf("the public key: %w", err)
	}
	return pem, nil
}
