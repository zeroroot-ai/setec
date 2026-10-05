// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

// Package diskbuilder turns an image digest into one signed, read-only disk
// for a launcher machine (docs/design/storage.md). It builds a disk once
// for each digest: the builder pushes the disk to the registry under the
// digest, and a later request finds it there. A node checks the signature
// before a machine uses the disk.
//
// The disk is a squashfs file system with the flattened image, and the image
// config at ImageConfigPath, which the guest agent reads for the entry
// point, the user, the directory and the environment.
package diskbuilder

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
)

// ImageConfigPath is where the disk holds the image config.
const ImageConfigPath = ".setec/image.json"

// ImageConfig is the part of the OCI image config that the guest agent uses.
type ImageConfig struct {
	Entrypoint []string `json:"entrypoint,omitempty"`
	Cmd        []string `json:"cmd,omitempty"`
	Env        []string `json:"env,omitempty"`
	User       string   `json:"user,omitempty"`
	WorkingDir string   `json:"workingDir,omitempty"`
}

// ParseDigestRef accepts an image reference with a digest and refuses a tag,
// also beside a digest: a tag can move, and one disk belongs to one exact
// image.
func ParseDigestRef(ref string) (name.Digest, error) {
	repo, _, ok := strings.Cut(ref, "@")
	if !ok {
		return name.Digest{}, fmt.Errorf("diskbuilder: %q has no digest; a tag is refused", ref)
	}
	if last := repo[strings.LastIndex(repo, "/")+1:]; strings.Contains(last, ":") {
		return name.Digest{}, fmt.Errorf("diskbuilder: %q names a tag; give the digest only", ref)
	}
	d, err := name.NewDigest(ref)
	if err != nil {
		return name.Digest{}, fmt.Errorf("diskbuilder: %q is not an image reference with a digest: %w", ref, err)
	}
	return d, nil
}

// imageTar writes the flattened file system of img, then the image config
// at ImageConfigPath, as one tar stream.
func imageTar(img v1.Image, w io.Writer) error {
	cf, err := img.ConfigFile()
	if err != nil {
		return fmt.Errorf("diskbuilder: read the image config: %w", err)
	}
	cfg, err := json.Marshal(ImageConfig{
		Entrypoint: cf.Config.Entrypoint, Cmd: cf.Config.Cmd, Env: cf.Config.Env,
		User: cf.Config.User, WorkingDir: cf.Config.WorkingDir,
	})
	if err != nil {
		return err
	}
	flat := mutate.Extract(img)
	defer func() { _ = flat.Close() }()
	tr := tar.NewReader(flat)
	tw := tar.NewWriter(w)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("diskbuilder: read the image layers: %w", err)
		}
		// mksquashfs refuses a path with a "." or ".." part, and image
		// layers often start each name with "./". A name that is the root
		// itself carries nothing; a name that climbs out is refused.
		clean, ok := cleanTarName(h.Name)
		if !ok {
			return fmt.Errorf("diskbuilder: the image holds the path %q, which leaves the root", h.Name)
		}
		if clean == "" {
			continue
		}
		h.Name = clean
		if h.Typeflag == tar.TypeLink {
			if h.Linkname, ok = cleanTarName(h.Linkname); !ok || h.Linkname == "" {
				return fmt.Errorf("diskbuilder: the image holds a hard link %q to %q outside the root", h.Name, h.Linkname)
			}
		}
		// The disk is the image and nothing else: the setec directory is
		// written below, never taken from the image.
		if clean == ".setec" || strings.HasPrefix(clean, ".setec/") {
			continue
		}
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		if _, err := io.Copy(tw, tr); err != nil { //nolint:gosec // the size is bounded by the image
			return err
		}
	}
	epoch := time.Unix(0, 0)
	for _, h := range []*tar.Header{
		{Name: ".setec/", Typeflag: tar.TypeDir, Mode: 0o755, ModTime: epoch},
		{Name: ImageConfigPath, Typeflag: tar.TypeReg, Mode: 0o444, Size: int64(len(cfg)), ModTime: epoch},
	} {
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
	}
	if _, err := tw.Write(cfg); err != nil {
		return err
	}
	return tw.Close()
}

// cleanTarName drops "." parts and a leading "/", keeps a trailing "/"
// of a directory, and reports false for a name with a ".." part.
func cleanTarName(n string) (string, bool) {
	dir := strings.HasSuffix(n, "/")
	var parts []string
	for p := range strings.SplitSeq(n, "/") {
		switch p {
		case "", ".":
		case "..":
			return "", false
		default:
			parts = append(parts, p)
		}
	}
	out := strings.Join(parts, "/")
	if dir && out != "" {
		out += "/"
	}
	return out, true
}

// Pack makes the squashfs disk at out from img. The time stamps of the
// file system are fixed, so one image gives the same bytes each time and
// the digest of the disk is stable (proof 6, setec#185, found the build
// time in the disk).
func Pack(ctx context.Context, img v1.Image, out string) error {
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(imageTar(img, pw)) }()
	cmd := exec.CommandContext(ctx, "mksquashfs", "-", out, "-tar", //nolint:gosec // fixed arguments
		"-comp", "zstd", "-noappend", "-quiet", "-no-progress",
		"-mkfs-time", "0", "-all-time", "0", "-no-xattrs")
	cmd.Stdin = pr
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		_ = pr.Close()
		return fmt.Errorf("diskbuilder: mksquashfs: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// Signature binds a disk to the image digest it came from.
type Signature struct {
	ImageDigest string `json:"imageDigest"`
	DiskSHA256  string `json:"diskSHA256"`
	Value       []byte `json:"value"` // ed25519 over Payload()
}

// Payload is the signed message.
func (s Signature) Payload() []byte {
	return []byte("setec-disk-v1\n" + s.ImageDigest + "\n" + s.DiskSHA256 + "\n")
}

// FileSHA256 returns the hex SHA-256 of a file.
func FileSHA256(path string) (string, error) {
	f, err := os.Open(path) //nolint:gosec // a disk path of the builder or the node
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Sign signs the disk at path for imageDigest.
func Sign(key ed25519.PrivateKey, imageDigest, path string) (Signature, error) {
	sum, err := FileSHA256(path)
	if err != nil {
		return Signature{}, err
	}
	s := Signature{ImageDigest: imageDigest, DiskSHA256: sum}
	s.Value = ed25519.Sign(key, s.Payload())
	return s, nil
}

// ErrBadSignature is the refusal of a disk.
var ErrBadSignature = errors.New("diskbuilder: the disk has no valid signature")

// Verify is the check of a node before a machine uses the disk at path: the
// signature must be valid under one of keys, name imageDigest, and match
// the bytes of the disk.
func Verify(keys []ed25519.PublicKey, s Signature, imageDigest, path string) error {
	if s.ImageDigest != imageDigest {
		return fmt.Errorf("%w: it is for image %s, not %s", ErrBadSignature, s.ImageDigest, imageDigest)
	}
	sum, err := FileSHA256(path)
	if err != nil {
		return err
	}
	if sum != s.DiskSHA256 {
		return fmt.Errorf("%w: the disk bytes changed", ErrBadSignature)
	}
	for _, k := range keys {
		if ed25519.Verify(k, s.Payload(), s.Value) {
			return nil
		}
	}
	return ErrBadSignature
}
