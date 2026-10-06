// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

// Package imagesig checks the cosign signature of an image. Only the disk
// builder links it, so the operator carries no Sigstore code.
package imagesig

import (
	"context"
	"crypto"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/verify"
	"github.com/sigstore/sigstore/pkg/cryptoutils"
	"github.com/sigstore/sigstore/pkg/signature"

	"github.com/zeroroot-ai/setec/internal/diskbuilder"
	"github.com/zeroroot-ai/setec/internal/errwrap"
)

// The signature of a pool image. cosign v3 attaches the signature of an
// image as a Sigstore bundle, an OCI referrer of the image manifest. The
// bundle signs the digest of the manifest.
const (
	// bundleArtifactPrefix is the artifact type of a Sigstore bundle.
	bundleArtifactPrefix = "application/vnd.dev.sigstore.bundle"
	// bundlePredicateAnnotation names the kind of a bundle referrer.
	bundlePredicateAnnotation = "dev.sigstore.bundle.predicateType"
	// cosignSignPredicate is the predicate type of an image signature, as
	// opposed to an attestation such as an SBOM.
	cosignSignPredicate = "https://sigstore.dev/cosign/sign/v1"
)

var (
	// ErrUnsigned is the result for an image with no signature bundle.
	ErrUnsigned = errors.New("the image has no signature")
	// ErrNotVerified is the result for an image whose signatures all fail
	// the policy.
	ErrNotVerified = errors.New("no signature of the image matches the policy")
)

// SignaturePolicy is the signer that an image must have.
type SignaturePolicy = diskbuilder.SignaturePolicy

// VerifyImage checks that the image, by digest, has a Sigstore signature
// that matches p. trusted holds the Sigstore roots of a keyless policy, and
// is not used with a key. It returns ErrUnsigned or ErrNotVerified, wrapped
// with the cause, when the image does not verify.
func VerifyImage(ctx context.Context, image string, p SignaturePolicy, trusted root.TrustedMaterial, opts ...remote.Option) error {
	if err := p.Validate(); err != nil {
		return err
	}
	ref, err := name.NewDigest(image)
	if err != nil {
		return fmt.Errorf("the image must be by digest: %w", err)
	}
	digest, err := v1.NewHash(ref.DigestStr())
	if err != nil {
		return errwrap.Wrap(err, "v1.NewHash")
	}
	entities, err := signatureBundles(ctx, ref, opts...)
	if err != nil {
		return err
	}
	if len(entities) == 0 {
		return fmt.Errorf("%s: %w", image, ErrUnsigned)
	}
	var last error
	for _, e := range entities {
		if last = verifyEntity(e, digest, p, trusted); last == nil {
			return nil
		}
	}
	return fmt.Errorf("%s: %w: %w", image, ErrNotVerified, last)
}

// signatureBundles reads the Sigstore signature bundles that are attached
// to ref as OCI referrers. Attestations, such as an SBOM, are left out.
func signatureBundles(ctx context.Context, ref name.Digest, opts ...remote.Option) ([]verify.SignedEntity, error) {
	opts = append(opts, remote.WithContext(ctx))
	index, err := remote.Referrers(ref, opts...)
	if err != nil {
		return nil, fmt.Errorf("list the referrers of %s: %w", ref, err)
	}
	manifest, err := index.IndexManifest()
	if err != nil {
		return nil, errwrap.Wrap(err, "v1.ImageIndex.IndexManifest")
	}
	var out []verify.SignedEntity
	for _, d := range manifest.Manifests {
		if !strings.HasPrefix(d.ArtifactType, bundleArtifactPrefix) ||
			(d.Annotations[bundlePredicateAnnotation] != "" && d.Annotations[bundlePredicateAnnotation] != cosignSignPredicate) {
			continue
		}
		b, err := readBundle(ref.Context().Digest(d.Digest.String()), opts...)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, nil
}

// readBundle reads the one layer of a bundle referrer as a Sigstore bundle.
func readBundle(ref name.Digest, opts ...remote.Option) (*bundle.Bundle, error) {
	img, err := remote.Image(ref, opts...)
	if err != nil {
		return nil, fmt.Errorf("read the signature %s: %w", ref, err)
	}
	layers, err := img.Layers()
	if err != nil {
		return nil, errwrap.Wrap(err, "v1.Image.Layers")
	}
	if len(layers) != 1 {
		return nil, fmt.Errorf("the signature %s has %d layers, want 1", ref, len(layers))
	}
	rc, err := layers[0].Uncompressed()
	if err != nil {
		return nil, errwrap.Wrap(err, "v1.Layer.Uncompressed")
	}
	defer func() { _ = rc.Close() }()
	raw, err := io.ReadAll(io.LimitReader(rc, 1<<20))
	if err != nil {
		return nil, errwrap.Wrap(err, "io.ReadAll")
	}
	b := &bundle.Bundle{}
	if err := b.UnmarshalJSON(raw); err != nil {
		return nil, fmt.Errorf("parse the signature bundle %s: %w", ref, err)
	}
	return b, nil
}

// verifyEntity checks one signature against the manifest digest and p.
func verifyEntity(e verify.SignedEntity, digest v1.Hash, p SignaturePolicy, trusted root.TrustedMaterial) error {
	raw, err := hex.DecodeString(digest.Hex)
	if err != nil {
		return errwrap.Wrap(err, "hex.DecodeString")
	}
	artifact := verify.WithArtifactDigest(digest.Algorithm, raw)
	if len(p.PublicKey) > 0 {
		material, err := keyMaterial(p.PublicKey)
		if err != nil {
			return err
		}
		v, err := verify.NewVerifier(material, verify.WithCurrentTime())
		if err != nil {
			return errwrap.Wrap(err, "verify.NewVerifier")
		}
		_, err = v.Verify(e, verify.NewPolicy(artifact, verify.WithKey()))
		return errwrap.Wrap(err, "verify.Verifier.Verify")
	}
	if trusted == nil {
		return errors.New("a keyless policy needs the Sigstore trusted root")
	}
	id, err := verify.NewShortCertificateIdentity(p.Issuer, "", p.Identity, "")
	if err != nil {
		return errwrap.Wrap(err, "verify.NewShortCertificateIdentity")
	}
	v, err := verify.NewVerifier(trusted, verify.WithTransparencyLog(1), verify.WithObserverTimestamps(1))
	if err != nil {
		return errwrap.Wrap(err, "verify.NewVerifier")
	}
	_, err = v.Verify(e, verify.NewPolicy(artifact, verify.WithCertificateIdentity(id)))
	return errwrap.Wrap(err, "verify.Verifier.Verify")
}

// keyMaterial is the trusted material of one PEM public key, valid at any
// time.
func keyMaterial(pemKey []byte) (root.TrustedMaterial, error) {
	pub, err := cryptoutils.UnmarshalPEMToPublicKey(pemKey)
	if err != nil {
		return nil, fmt.Errorf("parse the public key: %w", err)
	}
	sv, err := signature.LoadVerifier(pub, crypto.SHA256)
	if err != nil {
		return nil, errwrap.Wrap(err, "signature.LoadVerifier")
	}
	key := root.NewExpiringKey(sv, time.Time{}, time.Time{})
	return root.NewTrustedPublicKeyMaterial(func(string) (root.TimeConstrainedVerifier, error) { return key, nil }), nil
}
