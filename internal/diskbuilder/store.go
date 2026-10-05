// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package diskbuilder

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// The registry form of a disk: an artifact with one layer, the disk
// compressed with gzip, and the signature of the uncompressed disk in an
// annotation of the manifest. Its tag is the hex of the
// image digest, so one digest has one disk.
const (
	DiskMediaType       types.MediaType = "application/vnd.zeroroot.setec.disk.v1.squashfs+gzip"
	SignatureAnnotation                 = "ai.zeroroot.setec.disk.signature"
)

// Builder makes and stores the disk of an image digest once.
type Builder struct {
	// DiskRepo is the repository of the disks, e.g. ghcr.io/org/setec-disks.
	DiskRepo string
	// Key signs each new disk.
	Key ed25519.PrivateKey
	// Pack makes the disk of an image. Nil uses Pack (mksquashfs).
	Pack func(ctx context.Context, img v1.Image, out string) error
	// Options go to each registry call, e.g. credentials and transport.
	Options []remote.Option
	// TempDir holds a disk while it is built.
	TempDir string
}

func diskTag(repo string, d name.Digest) (name.Tag, error) {
	hexPart := strings.TrimPrefix(d.DigestStr(), "sha256:")
	return name.NewTag(repo + ":" + hexPart)
}

// Ensure returns the reference of the disk of ref, and builds it first when
// the registry does not hold it yet.
func (b *Builder) Ensure(ctx context.Context, ref string) (name.Tag, error) {
	d, err := ParseDigestRef(ref)
	if err != nil {
		return name.Tag{}, err
	}
	tag, err := diskTag(b.DiskRepo, d)
	if err != nil {
		return name.Tag{}, err
	}
	opts := append([]remote.Option{remote.WithContext(ctx)}, b.Options...)
	if _, err := remote.Head(tag, opts...); err == nil {
		return tag, nil
	} else if !isNotFound(err) {
		return name.Tag{}, fmt.Errorf("diskbuilder: look up %s: %w", tag, err)
	}

	img, err := remote.Image(d, opts...)
	if err != nil {
		return name.Tag{}, fmt.Errorf("diskbuilder: pull %s: %w", d, err)
	}
	dir, err := os.MkdirTemp(b.TempDir, "disk")
	if err != nil {
		return name.Tag{}, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	out := filepath.Join(dir, "disk.sqfs")
	pack := b.Pack
	if pack == nil {
		pack = Pack
	}
	if err := pack(ctx, img, out); err != nil {
		return name.Tag{}, err
	}
	sig, err := Sign(b.Key, d.DigestStr(), out)
	if err != nil {
		return name.Tag{}, err
	}
	sigJSON, err := json.Marshal(sig)
	if err != nil {
		return name.Tag{}, err
	}
	layer, err := tarball.LayerFromFile(out, tarball.WithMediaType(DiskMediaType),
		tarball.WithCompressedCaching)
	if err != nil {
		return name.Tag{}, err
	}
	art, err := mutate.Append(empty.Image, mutate.Addendum{Layer: layer, MediaType: DiskMediaType})
	if err != nil {
		return name.Tag{}, err
	}
	art = mutate.Annotations(art, map[string]string{SignatureAnnotation: string(sigJSON)}).(v1.Image) //nolint:forcetypeassert // Annotations of an Image is an Image
	if err := remote.Write(tag, art, opts...); err != nil {
		return name.Tag{}, fmt.Errorf("diskbuilder: push %s: %w", tag, err)
	}
	return tag, nil
}

func isNotFound(err error) bool {
	var te *transport.Error
	return errors.As(err, &te) && te.StatusCode == http.StatusNotFound
}

// Fetch is the node side: it downloads the disk of ref to dest and checks
// its signature before it returns. A disk that fails the check is deleted.
func Fetch(ctx context.Context, diskRepo, ref, dest string, keys []ed25519.PublicKey, opts ...remote.Option) error {
	d, err := ParseDigestRef(ref)
	if err != nil {
		return err
	}
	tag, err := diskTag(diskRepo, d)
	if err != nil {
		return err
	}
	opts = append([]remote.Option{remote.WithContext(ctx)}, opts...)
	art, err := remote.Image(tag, opts...)
	if err != nil {
		return fmt.Errorf("diskbuilder: fetch %s: %w", tag, err)
	}
	m, err := art.Manifest()
	if err != nil {
		return err
	}
	var sig Signature
	if err := json.Unmarshal([]byte(m.Annotations[SignatureAnnotation]), &sig); err != nil {
		return fmt.Errorf("%w: no signature on %s", ErrBadSignature, tag)
	}
	layers, err := art.Layers()
	if err != nil || len(layers) != 1 {
		return fmt.Errorf("diskbuilder: %s must hold one disk layer: %v", tag, err)
	}
	rc, err := layers[0].Uncompressed()
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()
	f, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o444) //nolint:gosec // a path of the node agent
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, rc); err != nil {
		_ = f.Close()
		_ = os.Remove(dest)
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := Verify(keys, sig, d.DigestStr(), dest); err != nil {
		_ = os.Remove(dest)
		return err
	}
	return nil
}
