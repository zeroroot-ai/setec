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
// is the hex of the image digest and the id of the signing key
// (<digest hex>-<KeyID>), so one digest has one disk for each signing key. A
// rotation of the seed builds and signs a new disk, and the immutable tag of
// the disk of the previous seed never blocks it (setec#272).
//
// One Job makes a disk in three containers, and none holds the signing seed
// (docs/design/runtime.md):
//
//  1. check (Exists): looks up the tag with the push credential. It reads no
//     image content.
//  2. build (Builder): pulls the image and makes the disk in the work
//     volume. It has no credential. It reports the SHA-256 of the disk.
//  3. publish (Publisher): waits for the signature that the operator makes
//     from the report, then pushes the disk and the signature with the push
//     credential. It treats the disk as bytes.
//
// The operator holds the seed and has no registry egress. Image content that
// a tenant names reaches only the build container, which holds no secret.
const (
	// DiskFile and SignatureFile are the two files of the layer.
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

// Exists reports whether repo holds the disk of ref for the key keyID.
func Exists(ctx context.Context, repo, ref, keyID string, opts ...remote.Option) (bool, error) {
	d, err := ParseDigestRef(ref)
	if err != nil {
		return false, err
	}
	tag, err := diskTag(repo, d, keyID)
	if err != nil {
		return false, err
	}
	_, err = remote.Head(tag, append([]remote.Option{remote.WithContext(ctx)}, opts...)...)
	switch {
	case err == nil:
		return true, nil
	case isNotFound(err):
		return false, nil
	default:
		return false, fmt.Errorf("diskbuilder: look up %s: %w", tag, err)
	}
}

// CheckResult is the report of the check container: whether the registry
// holds the disk.
type CheckResult struct {
	Exists bool `json:"exists"`
}

// BuildResult is the report of the build container to the operator, written
// as JSON to its termination message: the SHA-256 of the disk it made.
type BuildResult struct {
	DiskSHA256 string `json:"diskSHA256"`
}

// ErrBadReport is the refusal of a report of the check or build container.
var ErrBadReport = errors.New("diskbuilder: the report is not valid")

var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// strictJSON decodes raw into out and refuses an unknown field or more data.
func strictJSON(raw []byte, out any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("%w: %w", ErrBadReport, err)
	}
	if dec.More() {
		return fmt.Errorf("%w: data after the report", ErrBadReport)
	}
	return nil
}

// ParseCheckResult reads the report of the check container strictly.
func ParseCheckResult(raw []byte) (CheckResult, error) {
	var r CheckResult
	if err := strictJSON(raw, &r); err != nil {
		return CheckResult{}, err
	}
	return r, nil
}

// ParseBuildResult reads the report of the build container strictly. The
// build container handled image content that a tenant chose, and the
// operator signs what the report names.
func ParseBuildResult(raw []byte) (BuildResult, error) {
	var r BuildResult
	if err := strictJSON(raw, &r); err != nil {
		return BuildResult{}, err
	}
	if !sha256Hex.MatchString(r.DiskSHA256) {
		return BuildResult{}, fmt.Errorf("%w: the disk SHA-256 %q", ErrBadReport, r.DiskSHA256)
	}
	return r, nil
}

// Builder is the build step. It holds no credential and no key.
type Builder struct {
	// Pack makes the disk of an image. Nil uses Pack (mksquashfs).
	Pack func(ctx context.Context, img v1.Image, out string) error
	// Options go to each registry call, e.g. a transport.
	Options []remote.Option
}

// Build pulls ref and makes its disk at out.
func (b *Builder) Build(ctx context.Context, ref, out string) (BuildResult, error) {
	d, err := ParseDigestRef(ref)
	if err != nil {
		return BuildResult{}, err
	}
	img, err := remote.Image(d, append([]remote.Option{remote.WithContext(ctx)}, b.Options...)...)
	if err != nil {
		return BuildResult{}, fmt.Errorf("diskbuilder: pull %s: %w", d, err)
	}
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
	return BuildResult{DiskSHA256: sum}, nil
}

// Publisher is the publish step. It holds the push credential and no key:
// it gets the signature that the operator made.
type Publisher struct {
	// DiskRepo is the repository of the disks.
	DiskRepo string
	// Options go to each registry call, e.g. credentials and transport.
	Options []remote.Option
	// TempDir holds the layer while it is written.
	TempDir string
}

// Publish pushes the disk at disk and its signature under the tag of ref and
// keyID. The signature must name ref and the bytes of the disk.
func (p *Publisher) Publish(ctx context.Context, ref, keyID, disk string, sig Signature) (name.Tag, error) {
	d, err := ParseDigestRef(ref)
	if err != nil {
		return name.Tag{}, err
	}
	sum, err := FileSHA256(disk)
	if err != nil {
		return name.Tag{}, err
	}
	if sig.ImageDigest != d.DigestStr() || sig.DiskSHA256 != sum {
		return name.Tag{}, fmt.Errorf("%w: it does not name image %s and the disk", ErrBadSignature, d.DigestStr())
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
	dir, err := os.MkdirTemp(p.TempDir, "publish")
	if err != nil {
		return name.Tag{}, errwrap.Wrap(err, "os.MkdirTemp")
	}
	defer func() { _ = os.RemoveAll(dir) }()
	art, err := diskImage(dir, disk, sig)
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
