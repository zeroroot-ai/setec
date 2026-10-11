// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package diskbuilder

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"github.com/zeroroot-ai/setec/internal/errwrap"
)

// The registry form of a disk is an OCI image with two gzip tar layers. The
// first layer holds the disk, the second its signature. A launcher Pod
// mounts the image as an image volume, so the kubelet pulls the disk on the
// node with the credentials of the node, and the Pod network policy never
// has to allow the registry. The kubelet also keeps the disk in the image
// store of the node for the next Sandbox of the same digest. Its tag is the
// hex of the image digest and the id of the signing key (<digest hex>-<KeyID>),
// so one digest has one disk for each signing key. A rotation of the seed
// builds and signs a new disk, and the immutable tag of the disk of the
// previous seed never blocks it (setec#272).
//
// Three steps make a disk, and only one of them holds the signing seed
// (docs/design/storage.md):
//
//  1. Build (Builder, a Job with network and no seed): pull the image, make
//     the disk, push the disk layer as a blob, and report a BuildResult.
//  2. Sign (the operator, which has no egress to the registries): sign the
//     image digest and the SHA-256 of the disk that the BuildResult names.
//  3. Publish (Publisher, a Job that reaches the disk registry only and has
//     no seed): push the signature layer and the manifest under the tag.
//
// Image content that a tenant names reaches only the build step, and the
// build step never holds the seed.
const (
	// DiskFile and SignatureFile are the files of the two layers.
	DiskFile      = "disk.sqfs"
	SignatureFile = "disk.sig.json"
)

// DiskRef returns the reference of the disk of an image digest in repo that
// the key keyID signed.
func DiskRef(repo, imageRef, keyID string) (string, error) {
	d, err := ParseDigestRef(imageRef)
	if err != nil {
		return "", err
	}
	tag, err := diskTag(repo, d, keyID)
	if err != nil {
		return "", err
	}
	return tag.String(), nil
}

func diskTag(repo string, d name.Digest, keyID string) (name.Tag, error) {
	if !validKeyID(keyID) {
		return name.Tag{}, fmt.Errorf("diskbuilder: %q is not a signing key id", keyID)
	}
	hexPart := strings.TrimPrefix(d.DigestStr(), "sha256:")
	tag, err := name.NewTag(repo + ":" + hexPart + "-" + keyID)
	return tag, errwrap.Wrap(err, "name.NewTag")
}

// LayerRef names a layer blob in the disk repository.
type LayerRef struct {
	// Digest is the digest of the stored (gzip) bytes.
	Digest string `json:"digest"`
	// DiffID is the digest of the tar before gzip. The image config lists
	// it, and the node checks it when it unpacks the layer.
	DiffID string `json:"diffID"`
	// Size is the length of the stored bytes.
	Size int64 `json:"size"`
}

// BuildResult is the report of the build step to the operator, written as
// JSON to the termination message of the build Pod. Exists means that the
// registry already holds the signed disk, and nothing else is set.
type BuildResult struct {
	Exists     bool     `json:"exists,omitempty"`
	Layer      LayerRef `json:"layer"`
	DiskSHA256 string   `json:"diskSHA256,omitempty"`
}

// ErrBadBuildResult is the refusal of a build report.
var ErrBadBuildResult = errors.New("diskbuilder: the build result is not valid")

var (
	sha256Digest = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	sha256Hex    = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// ParseBuildResult reads a build report strictly. The report comes from a
// Pod that handled image content a tenant chose, so each field is checked
// before the operator signs anything.
func ParseBuildResult(raw []byte) (BuildResult, error) {
	var r BuildResult
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return BuildResult{}, fmt.Errorf("%w: %w", ErrBadBuildResult, err)
	}
	if err := r.Validate(); err != nil {
		return BuildResult{}, err
	}
	return r, nil
}

// Validate checks the fields of r.
func (r BuildResult) Validate() error {
	if r.Exists {
		if r.Layer != (LayerRef{}) || r.DiskSHA256 != "" {
			return fmt.Errorf("%w: a disk that exists names no layer", ErrBadBuildResult)
		}
		return nil
	}
	switch {
	case !sha256Digest.MatchString(r.Layer.Digest):
		return fmt.Errorf("%w: the layer digest %q", ErrBadBuildResult, r.Layer.Digest)
	case !sha256Digest.MatchString(r.Layer.DiffID):
		return fmt.Errorf("%w: the layer diff id %q", ErrBadBuildResult, r.Layer.DiffID)
	case r.Layer.Size <= 0:
		return fmt.Errorf("%w: the layer size %d", ErrBadBuildResult, r.Layer.Size)
	case !sha256Hex.MatchString(r.DiskSHA256):
		return fmt.Errorf("%w: the disk SHA-256 %q", ErrBadBuildResult, r.DiskSHA256)
	}
	return nil
}

// Builder is the build step. It holds no signing key.
type Builder struct {
	// DiskRepo is the repository of the disks, e.g. ghcr.io/org/setec-disks.
	DiskRepo string
	// KeyID is the id of the key that will sign the disk. It names the tag
	// that Build looks up.
	KeyID string
	// Pack makes the disk of an image. Nil uses Pack (mksquashfs).
	Pack func(ctx context.Context, img v1.Image, out string) error
	// Options go to each registry call, e.g. credentials and transport.
	Options []remote.Option
	// TempDir holds a disk while it is built.
	TempDir string
}

// Build makes the disk of ref and pushes its layer to the disk repository.
// When the registry holds the signed disk already, it reports Exists.
func (b *Builder) Build(ctx context.Context, ref string) (BuildResult, error) {
	d, err := ParseDigestRef(ref)
	if err != nil {
		return BuildResult{}, err
	}
	tag, err := diskTag(b.DiskRepo, d, b.KeyID)
	if err != nil {
		return BuildResult{}, err
	}
	opts := append([]remote.Option{remote.WithContext(ctx)}, b.Options...)
	if _, err := remote.Head(tag, opts...); err == nil {
		return BuildResult{Exists: true}, nil
	} else if !isNotFound(err) {
		return BuildResult{}, fmt.Errorf("diskbuilder: look up %s: %w", tag, err)
	}

	img, err := remote.Image(d, opts...)
	if err != nil {
		return BuildResult{}, fmt.Errorf("diskbuilder: pull %s: %w", d, err)
	}
	dir, err := os.MkdirTemp(b.TempDir, "disk")
	if err != nil {
		return BuildResult{}, errwrap.Wrap(err, "os.MkdirTemp")
	}
	defer func() { _ = os.RemoveAll(dir) }()
	out := filepath.Join(dir, DiskFile)
	pack := b.Pack
	if pack == nil {
		pack = Pack
	}
	if err := pack(ctx, img, out); err != nil {
		return BuildResult{}, err
	}
	sum, err := FileSHA256(out)
	if err != nil {
		return BuildResult{}, err
	}
	layer, err := fileLayer(filepath.Join(dir, "disk-layer.tar"), DiskFile, out)
	if err != nil {
		return BuildResult{}, err
	}
	if err := remote.WriteLayer(tag.Context(), layer, opts...); err != nil {
		return BuildResult{}, fmt.Errorf("diskbuilder: push the disk layer to %s: %w", tag.Context(), err)
	}
	lr, err := layerRef(layer)
	if err != nil {
		return BuildResult{}, err
	}
	return BuildResult{Layer: lr, DiskSHA256: sum}, nil
}

// Publisher is the publish step. It holds no signing key: it gets the
// signature that the operator made.
type Publisher struct {
	// DiskRepo is the repository of the disks.
	DiskRepo string
	// Options go to each registry call, e.g. credentials and transport.
	Options []remote.Option
	// TempDir holds the signature layer while it is written.
	TempDir string
}

// Publish pushes the signature of the disk that res names and the manifest
// of the two layers under the tag of ref and keyID. The signature must name
// ref and the disk of res.
func (p *Publisher) Publish(ctx context.Context, ref, keyID string, res BuildResult, sig Signature) (name.Tag, error) {
	d, err := ParseDigestRef(ref)
	if err != nil {
		return name.Tag{}, err
	}
	if err := res.Validate(); err != nil {
		return name.Tag{}, err
	}
	if res.Exists {
		return name.Tag{}, fmt.Errorf("%w: a disk that exists has nothing to publish", ErrBadBuildResult)
	}
	if sig.ImageDigest != d.DigestStr() || sig.DiskSHA256 != res.DiskSHA256 {
		return name.Tag{}, fmt.Errorf("%w: it does not name image %s and the built disk", ErrBadSignature, d.DigestStr())
	}
	tag, err := diskTag(p.DiskRepo, d, keyID)
	if err != nil {
		return name.Tag{}, err
	}
	opts := append([]remote.Option{remote.WithContext(ctx)}, p.Options...)
	if _, err := remote.Head(tag, opts...); err == nil {
		return tag, nil
	} else if !isNotFound(err) {
		return name.Tag{}, fmt.Errorf("diskbuilder: look up %s: %w", tag, err)
	}

	sigJSON, err := json.Marshal(sig)
	if err != nil {
		return name.Tag{}, errwrap.Wrap(err, "json.Marshal")
	}
	dir, err := os.MkdirTemp(p.TempDir, "sig")
	if err != nil {
		return name.Tag{}, errwrap.Wrap(err, "os.MkdirTemp")
	}
	defer func() { _ = os.RemoveAll(dir) }()
	sigPath := filepath.Join(dir, SignatureFile)
	if err := os.WriteFile(sigPath, sigJSON, 0o600); err != nil {
		return name.Tag{}, errwrap.Wrap(err, "os.WriteFile")
	}
	sigLayer, err := fileLayer(filepath.Join(dir, "sig-layer.tar"), SignatureFile, sigPath)
	if err != nil {
		return name.Tag{}, err
	}
	if err := remote.WriteLayer(tag.Context(), sigLayer, opts...); err != nil {
		return name.Tag{}, fmt.Errorf("diskbuilder: push the signature layer to %s: %w", tag.Context(), err)
	}
	sigRef, err := layerRef(sigLayer)
	if err != nil {
		return name.Tag{}, err
	}
	manifest, err := diskManifest(ctx, tag.Context(), res.Layer, sigRef, opts)
	if err != nil {
		return name.Tag{}, err
	}
	if err := remote.Put(tag, manifest, opts...); err != nil {
		return name.Tag{}, fmt.Errorf("diskbuilder: push %s: %w", tag, err)
	}
	return tag, nil
}

func isNotFound(err error) bool {
	var te *transport.Error
	return errors.As(err, &te) && te.StatusCode == http.StatusNotFound
}

// rawManifest is a manifest that remote.Put writes as it is.
type rawManifest struct {
	raw []byte
}

func (m rawManifest) RawManifest() ([]byte, error) { return m.raw, nil }

func (rawManifest) MediaType() (types.MediaType, error) { return types.OCIManifestSchema1, nil }

// diskManifest pushes the image config of the disk and returns the manifest
// of the disk layer and the signature layer.
func diskManifest(
	ctx context.Context, repo name.Repository, disk, sig LayerRef, opts []remote.Option,
) (rawManifest, error) {
	diskDigest, err := v1.NewHash(disk.Digest)
	if err != nil {
		return rawManifest{}, errwrap.Wrap(err, "v1.NewHash")
	}
	diskDiff, err := v1.NewHash(disk.DiffID)
	if err != nil {
		return rawManifest{}, errwrap.Wrap(err, "v1.NewHash")
	}
	sigDigest, err := v1.NewHash(sig.Digest)
	if err != nil {
		return rawManifest{}, errwrap.Wrap(err, "v1.NewHash")
	}
	sigDiff, err := v1.NewHash(sig.DiffID)
	if err != nil {
		return rawManifest{}, errwrap.Wrap(err, "v1.NewHash")
	}
	cfg, err := json.Marshal(&v1.ConfigFile{
		Architecture: "amd64", OS: "linux",
		RootFS: v1.RootFS{Type: "layers", DiffIDs: []v1.Hash{diskDiff, sigDiff}},
	})
	if err != nil {
		return rawManifest{}, errwrap.Wrap(err, "json.Marshal")
	}
	cfgLayer := static.NewLayer(cfg, types.OCIConfigJSON)
	if err := remote.WriteLayer(repo, cfgLayer, append([]remote.Option{remote.WithContext(ctx)}, opts...)...); err != nil {
		return rawManifest{}, fmt.Errorf("diskbuilder: push the image config to %s: %w", repo, err)
	}
	cfgDigest, err := cfgLayer.Digest()
	if err != nil {
		return rawManifest{}, errwrap.Wrap(err, "static.Layer.Digest")
	}
	raw, err := json.Marshal(&v1.Manifest{
		SchemaVersion: 2,
		MediaType:     types.OCIManifestSchema1,
		Config:        v1.Descriptor{MediaType: types.OCIConfigJSON, Size: int64(len(cfg)), Digest: cfgDigest},
		Layers: []v1.Descriptor{
			{MediaType: types.OCILayer, Size: disk.Size, Digest: diskDigest},
			{MediaType: types.OCILayer, Size: sig.Size, Digest: sigDigest},
		},
	})
	return rawManifest{raw: raw}, errwrap.Wrap(err, "json.Marshal")
}

// fileLayer writes a tar at tarPath with the one file src under the name
// entry, and returns it as a gzip layer. The header carries no time and no
// owner, so one file always gives one layer digest.
func fileLayer(tarPath, entry, src string) (v1.Layer, error) {
	if err := writeLayer(tarPath, entry, src); err != nil {
		return nil, err
	}
	// LayerFromFile compresses the tar with gzip, so the media type must say
	// gzip. A registry stores the compressed bytes, and the node reads the
	// media type to unpack them.
	layer, err := tarball.LayerFromFile(tarPath, tarball.WithMediaType(types.OCILayer),
		tarball.WithCompressedCaching)
	return layer, errwrap.Wrap(err, "tarball.LayerFromFile")
}

func layerRef(l v1.Layer) (LayerRef, error) {
	digest, err := l.Digest()
	if err != nil {
		return LayerRef{}, errwrap.Wrap(err, "v1.Layer.Digest")
	}
	diff, err := l.DiffID()
	if err != nil {
		return LayerRef{}, errwrap.Wrap(err, "v1.Layer.DiffID")
	}
	size, err := l.Size()
	if err != nil {
		return LayerRef{}, errwrap.Wrap(err, "v1.Layer.Size")
	}
	return LayerRef{Digest: digest.String(), DiffID: diff.String(), Size: size}, nil
}

// writeLayer writes a tar with the one file src under the name entry.
func writeLayer(path, entry, src string) error {
	f, err := os.Create(path) //nolint:gosec // a path in the build directory
	if err != nil {
		return errwrap.Wrap(err, "os.Create")
	}
	tw := tar.NewWriter(f)
	werr := writeLayerEntry(tw, entry, src)
	if cerr := tw.Close(); werr == nil {
		werr = cerr
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	return werr
}

func writeLayerEntry(tw *tar.Writer, entry, src string) error {
	d, err := os.Open(src) //nolint:gosec // a file that this step made
	if err != nil {
		return errwrap.Wrap(err, "os.Open")
	}
	defer func() { _ = d.Close() }()
	st, err := d.Stat()
	if err != nil {
		return errwrap.Wrap(err, "os.File.Stat")
	}
	if err := tw.WriteHeader(&tar.Header{Name: entry, Mode: 0o444, Size: st.Size(), Format: tar.FormatPAX}); err != nil {
		return errwrap.Wrap(err, "tar.Writer.WriteHeader")
	}
	_, err = io.Copy(tw, d)
	return errwrap.Wrap(err, "io.Copy")
}

// VerifyMounted is the check of a launcher before the machine uses a disk.
// The disk and the signature come from the image volume of the Pod. The
// signature must name imageRef, match the disk bytes and be valid under
// one of keys. It returns the hex SHA-256 of the checked disk.
func VerifyMounted(disk, sigFile, imageRef string, keys []ed25519.PublicKey) (diskSHA256 string, err error) {
	d, err := ParseDigestRef(imageRef)
	if err != nil {
		return "", err
	}
	raw, err := os.ReadFile(sigFile) //nolint:gosec // a path of the launcher spec
	if err != nil {
		return "", fmt.Errorf("%w: read the signature: %w", ErrBadSignature, err)
	}
	var sig Signature
	if err := json.Unmarshal(raw, &sig); err != nil {
		return "", fmt.Errorf("%w: the signature is not valid JSON", ErrBadSignature)
	}
	if err := Verify(keys, sig, d.DigestStr(), disk); err != nil {
		return "", err
	}
	return sig.DiskSHA256, nil
}
