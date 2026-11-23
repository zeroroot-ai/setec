// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package imagesig

import (
	"bytes"
	"context"
	"crypto"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
	"github.com/sigstore/sigstore/pkg/cryptoutils"
	"github.com/sigstore/sigstore/pkg/signature"

	"github.com/zeroroot-ai/setec/internal/errwrap"
)

// The simple signing format of cosign, the format that "cosign sign --key"
// writes by default before cosign v3, and that a KMS key signs. The signature
// is an image at the tag "<algorithm>-<hex of the digest>.sig" of the same
// repository. Each layer of that image is one signature. The layer holds the
// payload, and its annotation holds the signature of the payload. A signature
// of this kind carries no transparency log entry, so only a policy with a
// public key can check it. A keyless signature needs the certificate and the
// log entry, and only the bundle format carries them to this check.
const (
	// simpleSigningSignatureAnnotation holds the base64 signature of a layer.
	simpleSigningSignatureAnnotation = "dev.cosignproject.cosign/signature"
	// simpleSigningType is the type that the payload of a signature has.
	simpleSigningType = "cosign container image signature"
	// payloadLimit bounds the size of a payload that the check reads.
	payloadLimit = 1 << 20
)

// simpleSigningTag is the tag that holds the simple signing signatures of the
// image with the digest d: "sha256-<hex>.sig".
func simpleSigningTag(repo name.Repository, d v1.Hash) name.Tag {
	return repo.Tag(d.Algorithm + "-" + d.Hex + ".sig")
}

// simpleSignature is one signature of the simple signing format.
type simpleSignature struct {
	payload []byte
	sig     []byte
}

// simpleSignatures reads the signatures at the ".sig" tag of ref. An image
// with no such tag has none, and the result is empty with no error. Any other
// failure of the registry is an error, so that a registry that does not answer
// is not taken for an image with no signature.
func simpleSignatures(ctx context.Context, ref name.Digest, digest v1.Hash, opts ...remote.Option) ([]simpleSignature, error) {
	opts = append(opts, remote.WithContext(ctx))
	img, err := remote.Image(simpleSigningTag(ref.Context(), digest), opts...)
	if err != nil {
		var terr *transport.Error
		if errors.As(err, &terr) && terr.StatusCode == http.StatusNotFound {
			return nil, nil
		}
		return nil, fmt.Errorf("read the signature tag of %s: %w", ref, err)
	}
	manifest, err := img.Manifest()
	if err != nil {
		return nil, errwrap.Wrap(err, "v1.Image.Manifest")
	}
	var out []simpleSignature
	for _, d := range manifest.Layers {
		encoded := d.Annotations[simpleSigningSignatureAnnotation]
		if encoded == "" {
			continue
		}
		sig, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			continue
		}
		layer, err := img.LayerByDigest(d.Digest)
		if err != nil {
			return nil, errwrap.Wrap(err, "v1.Image.LayerByDigest")
		}
		rc, err := layer.Compressed()
		if err != nil {
			return nil, errwrap.Wrap(err, "v1.Layer.Compressed")
		}
		payload, err := io.ReadAll(io.LimitReader(rc, payloadLimit))
		_ = rc.Close()
		if err != nil {
			return nil, errwrap.Wrap(err, "io.ReadAll")
		}
		out = append(out, simpleSignature{payload: payload, sig: sig})
	}
	return out, nil
}

// verifySimpleSignature checks that the payload names the digest of the image,
// and that the key signed the payload.
func verifySimpleSignature(s simpleSignature, digest v1.Hash, pemKey []byte) error {
	var p struct {
		Critical struct {
			Image struct {
				Digest string `json:"docker-manifest-digest"`
			} `json:"image"`
			Type string `json:"type"`
		} `json:"critical"`
	}
	if err := json.Unmarshal(s.payload, &p); err != nil {
		return fmt.Errorf("parse the signed payload: %w", err)
	}
	if p.Critical.Type != simpleSigningType {
		return fmt.Errorf("the signed payload has the type %q, want %q", p.Critical.Type, simpleSigningType)
	}
	if p.Critical.Image.Digest != digest.String() {
		return fmt.Errorf("the signed payload names the image %q, want %q", p.Critical.Image.Digest, digest.String())
	}
	pub, err := cryptoutils.UnmarshalPEMToPublicKey(pemKey)
	if err != nil {
		return fmt.Errorf("parse the public key: %w", err)
	}
	sv, err := signature.LoadVerifier(pub, crypto.SHA256)
	if err != nil {
		return errwrap.Wrap(err, "signature.LoadVerifier")
	}
	if err := sv.VerifySignature(bytes.NewReader(s.sig), bytes.NewReader(s.payload)); err != nil {
		return fmt.Errorf("the signature does not match the key: %w", err)
	}
	return nil
}
