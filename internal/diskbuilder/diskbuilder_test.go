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
	"net/http"
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
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/google/go-containerregistry/pkg/v1/types"
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

// makeDisk runs the three steps of a disk as the Job and the operator run
// them: check, build with no credential and no key, the signature of the
// operator over the build report, and publish with no key. It returns the
// tag, and whether the check found the disk.
func makeDisk(
	t *testing.T, repo, ref string, key ed25519.PrivateKey, pack func(context.Context, v1.Image, string) error,
) (name.Tag, bool) {
	t.Helper()
	pub, _ := key.Public().(ed25519.PublicKey)
	ref1, err := DiskRef(repo, ref, KeyID(pub))
	if err != nil {
		t.Fatal(err)
	}
	tag, _ := name.NewTag(ref1)
	exists, err := Exists(t.Context(), repo, ref, KeyID(pub))
	if err != nil {
		t.Fatalf("Exists: %v", err)
	}
	if exists {
		return tag, true
	}
	disk := filepath.Join(t.TempDir(), DiskFile)
	res, err := (&Builder{Pack: pack}).Build(t.Context(), ref, disk)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	// The report crosses a container boundary as JSON, and the operator
	// parses it strictly.
	raw, _ := json.Marshal(res)
	res, err = ParseBuildResult(raw)
	if err != nil {
		t.Fatalf("ParseBuildResult: %v", err)
	}
	d, _ := ParseDigestRef(ref)
	sig := SignDigest(key, d.DigestStr(), res.DiskSHA256)
	got, err := (&Publisher{DiskRepo: repo, TempDir: t.TempDir()}).Publish(t.Context(), ref, KeyID(pub), disk, sig)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if got != tag {
		t.Fatalf("Publish wrote %s, want %s", got, tag)
	}
	return tag, false
}

// pushTestImage writes the test image to an in-memory registry and returns
// the registry host and the digest reference of the image.
func pushTestImage(t *testing.T, h http.Handler) (host, ref string) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	src, _ := name.NewTag(u.Host + "/tools/scanner:v1")
	img := testImage(t)
	if err := remote.Write(src, img); err != nil {
		t.Fatal(err)
	}
	dg, _ := img.Digest()
	host = u.Host
	return host, host + "/tools/scanner@" + dg.String()
}

// TestDisk_BuildsOnceSignsAndTheNodeVerifies runs the whole path against an
// in-memory registry: the check finds nothing, the build makes the disk, the
// operator signs, the publish step writes the tag, a second check finds the
// disk, the node fetch verifies, and a wrong key is refused.
func TestDisk_BuildsOnceSignsAndTheNodeVerifies(t *testing.T) {
	t.Parallel()
	host, ref := pushTestImage(t, registry.New())
	repo := host + "/setec-disks"

	pub, key, _ := ed25519.GenerateKey(nil)
	var packs atomic.Int32
	tag1, found := makeDisk(t, repo, ref, key, tarPack(&packs))
	if found {
		t.Fatal("the first check found a disk")
	}
	tag2, found := makeDisk(t, repo, ref, key, tarPack(&packs))
	if !found || tag2 != tag1 {
		t.Fatalf("second check = %t, tag %v; want found and %v", found, tag2, tag1)
	}
	if packs.Load() != 1 {
		t.Fatalf("packs = %d, want 1: a digest is built once", packs.Load())
	}
	if _, err := (&Builder{}).Build(t.Context(), host+"/tools/scanner:v1", filepath.Join(t.TempDir(), "d")); err == nil {
		t.Fatal("Build accepted a tag")
	}

	// The node side: the kubelet unpacks the one layer into an image
	// volume, and the launcher checks the two files.
	art := checkDiskArtifact(t, tag1)
	mnt := t.TempDir()
	untar(t, mutate.Extract(art), mnt)
	disk, sigFile := filepath.Join(mnt, DiskFile), filepath.Join(mnt, SignatureFile)
	if sum, err := VerifyMounted(disk, sigFile, ref, []ed25519.PublicKey{pub}); err != nil || len(sum) != 64 {
		t.Fatalf("VerifyMounted with the right key: %v", err)
	}
	other, _, _ := ed25519.GenerateKey(nil)
	if _, err := VerifyMounted(disk, sigFile, ref, []ed25519.PublicKey{other}); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("VerifyMounted with a wrong key = %v, want ErrBadSignature", err)
	}
	if _, err := VerifyMounted(disk, filepath.Join(mnt, "missing"), ref, []ed25519.PublicKey{pub}); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("VerifyMounted with no signature = %v, want ErrBadSignature", err)
	}
}

// The publish step writes only a signature that names the image and the
// bytes of the disk in the work volume. A signature of another image or of
// another disk is refused, so a disk is never published under the
// signature of another one.
func TestPublisher_RefusesASignatureOfAnotherDisk(t *testing.T) {
	t.Parallel()
	host, ref := pushTestImage(t, registry.New())
	repo := host + "/setec-disks"
	pub, key, _ := ed25519.GenerateKey(nil)
	var packs atomic.Int32
	disk := filepath.Join(t.TempDir(), DiskFile)
	res, err := (&Builder{Pack: tarPack(&packs)}).Build(t.Context(), ref, disk)
	if err != nil {
		t.Fatal(err)
	}
	d, _ := ParseDigestRef(ref)
	p := &Publisher{DiskRepo: repo, TempDir: t.TempDir()}
	for what, sig := range map[string]Signature{
		"another image": SignDigest(key, "sha256:"+strings.Repeat("b", 64), res.DiskSHA256),
		"another disk":  SignDigest(key, d.DigestStr(), strings.Repeat("c", 64)),
	} {
		if _, err := p.Publish(t.Context(), ref, KeyID(pub), disk, sig); !errors.Is(err, ErrBadSignature) {
			t.Errorf("%s: Publish = %v, want ErrBadSignature", what, err)
		}
	}
	if found, err := Exists(t.Context(), repo, ref, KeyID(pub)); err != nil || found {
		t.Fatalf("a refused publish wrote a disk: %t, %v", found, err)
	}
}

// The operator signs what the build report names, so it reads the reports
// strictly: a field it does not know, more data, or a disk SHA-256 that is
// not hex is refused.
func TestParseReports_AreStrict(t *testing.T) {
	t.Parallel()
	good := `{"diskSHA256":"` + strings.Repeat("c", 64) + `"}`
	if got, err := ParseBuildResult([]byte(good)); err != nil || got.DiskSHA256 != strings.Repeat("c", 64) {
		t.Fatalf("ParseBuildResult(good) = %+v, %v", got, err)
	}
	for what, raw := range map[string]string{
		"short sha":     `{"diskSHA256":"abc"}`,
		"not hex":       `{"diskSHA256":"` + strings.Repeat("z", 64) + `"}`,
		"unknown field": `{"diskSHA256":"` + strings.Repeat("c", 64) + `","extra":1}`,
		"more data":     good + good,
		"not JSON":      "pwned",
	} {
		if _, err := ParseBuildResult([]byte(raw)); !errors.Is(err, ErrBadReport) {
			t.Errorf("%s: err %v, want ErrBadReport", what, err)
		}
	}
	if got, err := ParseCheckResult([]byte(`{"exists":true}`)); err != nil || !got.Exists {
		t.Fatalf("ParseCheckResult = %+v, %v", got, err)
	}
	if _, err := ParseCheckResult([]byte(`{"exists":true,"x":1}`)); !errors.Is(err, ErrBadReport) {
		t.Fatalf("ParseCheckResult with an unknown field: %v", err)
	}
}

// immutableTags refuses a push of a manifest tag that already exists, as an
// ECR repository with immutable tags does (D85).
func immutableTags(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/manifests/") {
			rec := httptest.NewRecorder()
			next.ServeHTTP(rec, httptest.NewRequestWithContext(r.Context(), http.MethodHead, r.URL.Path, nil))
			if rec.Code == http.StatusOK {
				http.Error(w, `{"errors":[{"code":"TAG_INVALID","message":"the tag is immutable"}]}`, http.StatusBadRequest)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// TestDisk_ANewSeedSignsANewDisk is setec#272: after a rotation of the
// signing seed, the disk of the previous key is not reused. A new disk is
// built under a new tag, beside the old one, in a registry with immutable
// tags. A launcher that trusts only the new key accepts the new disk, and
// the disk of the previous key stays for the launchers that still trust it.
func TestDisk_ANewSeedSignsANewDisk(t *testing.T) {
	t.Parallel()
	host, ref := pushTestImage(t, immutableTags(registry.New()))
	repo := host + "/setec-disks"

	oldPub, oldKey, _ := ed25519.GenerateKey(nil)
	newPub, newKey, _ := ed25519.GenerateKey(nil)
	var packs atomic.Int32
	oldTag, _ := makeDisk(t, repo, ref, oldKey, tarPack(&packs))
	newTag, found := makeDisk(t, repo, ref, newKey, tarPack(&packs))
	if found {
		t.Fatal("the new seed found the disk of the previous seed")
	}
	if newTag == oldTag {
		t.Fatalf("the new seed reused the tag %s of the previous seed", oldTag)
	}
	if packs.Load() != 2 {
		t.Fatalf("packs = %d, want 2: a new seed builds a new disk", packs.Load())
	}
	if !strings.HasSuffix(newTag.TagStr(), "-"+KeyID(newPub)) || !strings.HasSuffix(oldTag.TagStr(), "-"+KeyID(oldPub)) {
		t.Fatalf("tags %s and %s do not name their keys", oldTag, newTag)
	}

	for _, c := range []struct {
		tag  name.Tag
		keys []ed25519.PublicKey
		ok   bool
	}{
		{newTag, []ed25519.PublicKey{newPub}, true},         // after the retire step
		{newTag, []ed25519.PublicKey{newPub, oldPub}, true}, // during the rotation
		{oldTag, []ed25519.PublicKey{newPub, oldPub}, true}, // a launcher of an old disk, during the rotation
		{oldTag, []ed25519.PublicKey{newPub}, false},        // after the retire step
		{newTag, []ed25519.PublicKey{oldPub}, false},        // a launcher that does not trust the new key yet
	} {
		mnt := t.TempDir()
		untar(t, mutate.Extract(checkDiskArtifact(t, c.tag)), mnt)
		_, err := VerifyMounted(filepath.Join(mnt, DiskFile), filepath.Join(mnt, SignatureFile), ref, c.keys)
		if (err == nil) != c.ok {
			t.Fatalf("VerifyMounted(%s, %d keys) = %v, want ok=%v", c.tag.TagStr(), len(c.keys), err, c.ok)
		}
	}
}

func TestDiskRef_RefusesABadKeyID(t *testing.T) {
	t.Parallel()
	ref := "registry.test/tools/scanner@sha256:" + strings.Repeat("a", 64)
	for _, id := range []string{"", "abc", strings.Repeat("A", 16), strings.Repeat("g", 16), strings.Repeat("a", 17)} {
		if _, err := DiskRef("registry.test/setec-disks", ref, id); err == nil {
			t.Errorf("DiskRef accepted the key id %q", id)
		}
	}
	got, err := DiskRef("registry.test/setec-disks", ref, strings.Repeat("0", 16))
	if err != nil || got != "registry.test/setec-disks:"+strings.Repeat("a", 64)+"-"+strings.Repeat("0", 16) {
		t.Fatalf("DiskRef = %q, %v", got, err)
	}
}

func TestKeyID_IsStableAndShort(t *testing.T) {
	t.Parallel()
	pub := ed25519.PublicKey(make([]byte, ed25519.PublicKeySize))
	// The SHA-256 of 32 zero bytes starts with 66687aadf862bd77.
	if got := KeyID(pub); got != "66687aadf862bd77" {
		t.Fatalf("KeyID = %q", got)
	}
	if k, err := ParsePrivateKey([]byte("not base64!")); err == nil || k != nil {
		t.Fatal("ParsePrivateKey accepted a bad seed")
	}
}

// checkDiskArtifact checks that the disk at tag is one OCI layer of gzip
// bytes, and returns it.
func checkDiskArtifact(t *testing.T, tag name.Tag) v1.Image {
	t.Helper()
	art, err := remote.Image(tag)
	if err != nil {
		t.Fatal(err)
	}
	if mt, _ := art.MediaType(); mt != types.OCIManifestSchema1 {
		t.Fatalf("manifest media type = %s", mt)
	}
	layers, err := art.Layers()
	if err != nil || len(layers) != 1 {
		t.Fatalf("layers = %d, %v; want 1", len(layers), err)
	}
	for _, l := range layers {
		if mt, _ := l.MediaType(); mt != types.OCILayer {
			t.Fatalf("layer media type = %s", mt)
		}
		// The stored bytes must be what the media type says: the first real
		// pull failed on gzip bytes under the media type of a plain tar.
		blob, err := l.Compressed()
		if err != nil {
			t.Fatal(err)
		}
		magic := make([]byte, 2)
		_, err = io.ReadFull(blob, magic)
		_ = blob.Close()
		if err != nil || magic[0] != 0x1f || magic[1] != 0x8b {
			t.Fatalf("the stored layer is not gzip: % x, %v", magic, err)
		}
	}
	return art
}

// untar writes the files of a tar stream into dir.
func untar(t *testing.T, rc io.ReadCloser, dir string) {
	t.Helper()
	defer func() { _ = rc.Close() }()
	tr := tar.NewReader(rc)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		f, err := os.Create(filepath.Join(dir, filepath.Base(h.Name)))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(f, tr); err != nil { //nolint:gosec // a test archive of a few bytes
			t.Fatal(err)
		}
		_ = f.Close()
	}
}

func TestVerify_RefusesAChangedDiskAndAnotherImage(t *testing.T) {
	t.Parallel()
	pub, key, _ := ed25519.GenerateKey(nil)
	path := filepath.Join(t.TempDir(), "d")
	_ = os.WriteFile(path, []byte("disk bytes"), 0o600)
	sum, err := FileSHA256(path)
	if err != nil {
		t.Fatal(err)
	}
	sig := SignDigest(key, "sha256:aaa", sum)
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
	sums := make([]string, 0, 2)
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

func TestCleanTarName(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"./": "", ".": "", "./etc/": "etc/", "./etc/passwd": "etc/passwd", "/usr/bin/sh": "usr/bin/sh",
		"a/./b": "a/b", "usr//lib/": "usr/lib/",
	} {
		if got, ok := cleanTarName(in); !ok || got != want {
			t.Errorf("cleanTarName(%q) = %q, %t; want %q", in, got, ok, want)
		}
	}
	for _, bad := range []string{"../etc", "a/../../b", "./.."} {
		if _, ok := cleanTarName(bad); ok {
			t.Errorf("cleanTarName(%q) accepted a path that leaves the root", bad)
		}
	}
}

// TestPack_ImageWithDotSlashNames packs a layer whose names start with
// "./", as many image builders write them. mksquashfs refused such names
// on the first real build of the executor image.
func TestPack_ImageWithDotSlashNames(t *testing.T) {
	t.Parallel()
	out, _ := exec.Command("mksquashfs", "-help").CombinedOutput()
	if !strings.Contains(string(out), "-tar") {
		t.Skip("mksquashfs with -tar is not on this host")
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, h := range []*tar.Header{
		{Name: "./", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: "./etc/", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: "./etc/hostname", Typeflag: tar.TypeReg, Mode: 0o644, Size: 2},
		{Name: "./etc/name", Typeflag: tar.TypeLink, Linkname: "./etc/hostname"},
	} {
		_ = tw.WriteHeader(h)
		if h.Size > 0 {
			_, _ = tw.Write([]byte("x\n"))
		}
	}
	_ = tw.Close()
	layer, err := tarballLayer(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	img, err := mutate.AppendLayers(testImage(t), layer)
	if err != nil {
		t.Fatal(err)
	}
	if err := Pack(t.Context(), img, filepath.Join(t.TempDir(), "d")); err != nil {
		t.Fatalf("Pack: %v", err)
	}
}

func tarballLayer(b []byte) (v1.Layer, error) {
	return tarball.LayerFromOpener(func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil })
}
