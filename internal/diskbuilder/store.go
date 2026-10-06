// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package diskbuilder

import (
	"archive/tar"
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
	"github.com/zeroroot-ai/setec/internal/errwrap"
)

// The registry form of a disk is an OCI image with one gzip tar layer. The layer holds two files: the disk and its signature. A launcher
// Pod mounts the image as an image volume, so the kubelet pulls the disk on
// the node with the credentials of the node, and the Pod network policy
// never has to allow the registry. The kubelet also keeps the disk in the
// image store of the node for the next Sandbox of the same digest. Its tag
// is the hex of the image digest, so one digest has one disk.
const (
	// DiskFile and SignatureFile are the two files of the layer.
	DiskFile      = "disk.sqfs"
	SignatureFile = "disk.sig.json"
)

// DiskRef returns the reference of the disk of an image digest in repo.
func DiskRef(repo, imageRef string) (string, error) {
	d, err := ParseDigestRef(imageRef)
	if err != nil {
		return "", err
	}
	tag, err := diskTag(repo, d)
	if err != nil {
		return "", err
	}
	return tag.String(), nil
}

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
	tag, err := name.NewTag(repo + ":" + hexPart)
	return tag, errwrap.Wrap(err, "name.NewTag")
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
		return name.Tag{}, errwrap.Wrap(err, "os.MkdirTemp")
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
	art, err := diskImage(dir, out, sig)
	if err != nil {
		return name.Tag{}, err
	}
	if err := remote.Write(tag, art, opts...); err != nil {
		return name.Tag{}, fmt.Errorf("diskbuilder: push %s: %w", tag, err)
	}
	return tag, nil
}

func isNotFound(err error) bool {
	var te *transport.Error
	return errors.As(err, &te) && te.StatusCode == http.StatusNotFound
}

// diskImage makes the OCI image of a disk and its signature.
func diskImage(dir, disk string, sig Signature) (v1.Image, error) {
	sigJSON, err := json.Marshal(sig)
	if err != nil {
		return nil, errwrap.Wrap(err, "json.Marshal")
	}
	layerPath := filepath.Join(dir, "layer.tar")
	if err := writeLayer(layerPath, disk, sigJSON); err != nil {
		return nil, err
	}
	// LayerFromFile compresses the tar with gzip, so the media type must say
	// gzip. A registry stores the compressed bytes, and the node reads the
	// media type to unpack them.
	layer, err := tarball.LayerFromFile(layerPath, tarball.WithMediaType(types.OCILayer),
		tarball.WithCompressedCaching)
	if err != nil {
		return nil, errwrap.Wrap(err, "tarball.LayerFromFile")
	}
	base := mutate.MediaType(empty.Image, types.OCIManifestSchema1)
	base = mutate.ConfigMediaType(base, types.OCIConfigJSON)
	base, err = mutate.ConfigFile(base, &v1.ConfigFile{
		Architecture: "amd64", OS: "linux",
		RootFS: v1.RootFS{Type: "layers"},
	})
	if err != nil {
		return nil, errwrap.Wrap(err, "mutate.ConfigFile")
	}
	img, err := mutate.Append(base, mutate.Addendum{Layer: layer, MediaType: types.OCILayer})
	return img, errwrap.Wrap(err, "mutate.Append")
}

// writeLayer writes a tar with the disk and its signature. The headers carry
// no time and no owner, so one disk always gives one layer digest.
func writeLayer(path, disk string, sigJSON []byte) error {
	f, err := os.Create(path) //nolint:gosec // a path in the build directory
	if err != nil {
		return errwrap.Wrap(err, "os.Create")
	}
	tw := tar.NewWriter(f)
	werr := writeLayerEntries(tw, disk, sigJSON)
	if cerr := tw.Close(); werr == nil {
		werr = cerr
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	return werr
}

func writeLayerEntries(tw *tar.Writer, disk string, sigJSON []byte) error {
	d, err := os.Open(disk) //nolint:gosec // the disk that this builder made
	if err != nil {
		return errwrap.Wrap(err, "os.Open")
	}
	defer func() { _ = d.Close() }()
	st, err := d.Stat()
	if err != nil {
		return errwrap.Wrap(err, "os.File.Stat")
	}
	if err := tw.WriteHeader(&tar.Header{Name: DiskFile, Mode: 0o444, Size: st.Size(), Format: tar.FormatPAX}); err != nil {
		return errwrap.Wrap(err, "tar.Writer.WriteHeader")
	}
	if _, err := io.Copy(tw, d); err != nil {
		return errwrap.Wrap(err, "io.Copy")
	}
	if err := tw.WriteHeader(&tar.Header{
		Name: SignatureFile, Mode: 0o444, Size: int64(len(sigJSON)), Format: tar.FormatPAX,
	}); err != nil {
		return errwrap.Wrap(err, "tar.Writer.WriteHeader")
	}
	_, err = tw.Write(sigJSON)
	return errwrap.Wrap(err, "tar.Writer.Write")
}

// VerifyMounted is the check of a launcher before the machine uses a disk.
// The disk and the signature come from the image volume of the Pod. The
// signature must name imageRef, match the disk bytes and be valid under
// one of keys.
func VerifyMounted(disk, sigFile, imageRef string, keys []ed25519.PublicKey) error {
	d, err := ParseDigestRef(imageRef)
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(sigFile) //nolint:gosec // a path of the launcher spec
	if err != nil {
		return fmt.Errorf("%w: read the signature: %w", ErrBadSignature, err)
	}
	var sig Signature
	if err := json.Unmarshal(raw, &sig); err != nil {
		return fmt.Errorf("%w: the signature is not valid JSON", ErrBadSignature)
	}
	return Verify(keys, sig, d.DigestStr(), disk)
}
