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
	"io"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

func TestParseDigestRef(t *testing.T) {
	t.Parallel()
	const dg = "sha256:7842752def3da204501c0bb78a9db1427a90199fd9f3e679756fcf58759ff552"
	if _, err := ParseDigestRef("ghcr.io/zeroroot-ai/gibson-executor@" + dg); err != nil {
		t.Fatalf("a digest reference was refused: %v", err)
	}
	if _, err := ParseDigestRef("localhost:5000/tool@" + dg); err != nil {
		t.Fatalf("a registry port is not a tag: %v", err)
	}
	for _, bad := range []string{
		"ghcr.io/zeroroot-ai/gibson-executor:main",
		"ghcr.io/zeroroot-ai/gibson-executor",
		"ghcr.io/zeroroot-ai/gibson-executor:main@" + dg,
		"ghcr.io/zeroroot-ai/gibson-executor@sha256:short",
	} {
		if _, err := ParseDigestRef(bad); err == nil {
			t.Errorf("ParseDigestRef(%q) accepted it", bad)
		}
	}
}

func testImage(t *testing.T) v1.Image {
	t.Helper()
	img, err := random.Image(256, 2)
	if err != nil {
		t.Fatal(err)
	}
	cf, _ := img.ConfigFile()
	cf.Config.Entrypoint = []string{"/usr/local/bin/tool"}
	cf.Config.User = "runner:runner"
	cf.Config.Env = []string{"PATH=/usr/bin"}
	cf.Config.WorkingDir = "/home/runner"
	img, err = mutate.ConfigFile(img, cf)
	if err != nil {
		t.Fatal(err)
	}
	return img
}

// tarPack stands in for mksquashfs: the disk is the tar stream itself.
func tarPack(packs *atomic.Int32) func(context.Context, v1.Image, string) error {
	return func(_ context.Context, img v1.Image, out string) error {
		packs.Add(1)
		f, err := os.Create(out) //nolint:gosec // a test path
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		return imageTar(img, f)
	}
}

func TestImageTar_HoldsTheImageConfig(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := imageTar(testImage(t), &buf); err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(&buf)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			t.Fatal("the disk has no image config")
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Name != ImageConfigPath {
			continue
		}
		var cfg ImageConfig
		if err := json.NewDecoder(tr).Decode(&cfg); err != nil {
			t.Fatal(err)
		}
		if cfg.User != "runner:runner" || cfg.Entrypoint[0] != "/usr/local/bin/tool" || cfg.WorkingDir != "/home/runner" {
			t.Fatalf("image config = %+v", cfg)
		}
		return
	}
}

// TestBuilder_BuildsOnceSignsAndTheNodeVerifies runs the whole path against
// an in-memory registry: the first Ensure builds and pushes, the second
// finds the disk, the node fetch verifies, and a wrong key is refused.
func TestBuilder_BuildsOnceSignsAndTheNodeVerifies(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(registry.New())
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	src, _ := name.NewTag(u.Host + "/tools/scanner:v1")
	img := testImage(t)
	if err := remote.Write(src, img); err != nil {
		t.Fatal(err)
	}
	dg, _ := img.Digest()
	ref := u.Host + "/tools/scanner@" + dg.String()

	pub, key, _ := ed25519.GenerateKey(nil)
	var packs atomic.Int32
	b := &Builder{DiskRepo: u.Host + "/setec-disks", Key: key, Pack: tarPack(&packs), TempDir: t.TempDir()}
	tag1, err := b.Ensure(t.Context(), ref)
	if err != nil {
		t.Fatalf("first Ensure: %v", err)
	}
	tag2, err := b.Ensure(t.Context(), ref)
	if err != nil || tag2 != tag1 {
		t.Fatalf("second Ensure = %v, %v", tag2, err)
	}
	if packs.Load() != 1 {
		t.Fatalf("packs = %d, want 1: a digest is built once", packs.Load())
	}
	if _, err := b.Ensure(t.Context(), u.Host+"/tools/scanner:v1"); err == nil {
		t.Fatal("Ensure accepted a tag")
	}

	dest := filepath.Join(t.TempDir(), "disk.sqfs")
	if err := Fetch(t.Context(), b.DiskRepo, ref, dest, []ed25519.PublicKey{pub}); err != nil {
		t.Fatalf("Fetch with the right key: %v", err)
	}
	other, _, _ := ed25519.GenerateKey(nil)
	dest2 := filepath.Join(t.TempDir(), "disk.sqfs")
	if err := Fetch(t.Context(), b.DiskRepo, ref, dest2, []ed25519.PublicKey{other}); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("Fetch with a wrong key = %v, want ErrBadSignature", err)
	}
	if _, err := os.Stat(dest2); !os.IsNotExist(err) {
		t.Fatal("a refused disk stays on the node")
	}
}

func TestVerify_RefusesAChangedDiskAndAnotherImage(t *testing.T) {
	t.Parallel()
	pub, key, _ := ed25519.GenerateKey(nil)
	path := filepath.Join(t.TempDir(), "d")
	_ = os.WriteFile(path, []byte("disk bytes"), 0o600)
	sig, err := Sign(key, "sha256:aaa", path)
	if err != nil {
		t.Fatal(err)
	}
	keys := []ed25519.PublicKey{pub}
	if err := Verify(keys, sig, "sha256:aaa", path); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if err := Verify(keys, sig, "sha256:bbb", path); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("another image: %v", err)
	}
	_ = os.WriteFile(path, []byte("changed"), 0o600)
	if err := Verify(keys, sig, "sha256:aaa", path); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("a changed disk: %v", err)
	}
}

// TestPack_IsReproducible needs mksquashfs 4.6 or later (-tar). It proves
// that two packs of one image give the same bytes.
func TestPack_IsReproducible(t *testing.T) {
	t.Parallel()
	out, err := exec.Command("mksquashfs", "-help").CombinedOutput()
	if err != nil && len(out) == 0 || !strings.Contains(string(out), "-tar") {
		t.Skip("mksquashfs with -tar is not on this host")
	}
	img := testImage(t)
	dir := t.TempDir()
	var sums []string
	for _, n := range []string{"a", "b"} {
		p := filepath.Join(dir, n)
		if err := Pack(t.Context(), img, p); err != nil {
			t.Fatalf("Pack: %v", err)
		}
		s, _ := FileSHA256(p)
		sums = append(sums, s)
	}
	if sums[0] != sums[1] {
		t.Fatalf("two packs differ: %s %s", sums[0], sums[1])
	}
}
