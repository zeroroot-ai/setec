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

// makeDisk runs the three steps of a disk as the operator runs them: the
// build step with no key, the signature of the operator over the build
// report, and the publish step with no key. It returns the tag and the
// build report.
func makeDisk(
	t *testing.T, repo, ref string, key ed25519.PrivateKey, pack func(context.Context, v1.Image, string) error,
) (name.Tag, BuildResult) {
	t.Helper()
	pub, _ := key.Public().(ed25519.PublicKey)
	b := &Builder{DiskRepo: repo, KeyID: KeyID(pub), Pack: pack, TempDir: t.TempDir()}
	res, err := b.Build(t.Context(), ref)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if res.Exists {
		tag, err := DiskRef(repo, ref, KeyID(pub))
		if err != nil {
			t.Fatal(err)
		}
		parsed, _ := name.NewTag(tag)
		return parsed, res
	}
	// The report crosses a Pod boundary as JSON, and the operator parses it
	// strictly.
	raw, _ := json.Marshal(res)
	res, err = ParseBuildResult(raw)
	if err != nil {
		t.Fatalf("ParseBuildResult: %v", err)
	}
	d, _ := ParseDigestRef(ref)
	sig := SignDigest(key, d.DigestStr(), res.DiskSHA256)
	tag, err := (&Publisher{DiskRepo: repo, TempDir: t.TempDir()}).Publish(t.Context(), ref, KeyID(pub), res, sig)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	return tag, res
}

// TestDisk_BuildsOnceSignsAndTheNodeVerifies runs the whole path against an
// in-memory registry: the first build makes and pushes the disk layer, the
// operator signs, the publish step writes the tag, a second build finds the
// disk, the node fetch verifies, and a wrong key is refused.
func TestDisk_BuildsOnceSignsAndTheNodeVerifies(t *testing.T) {
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
	repo := u.Host + "/setec-disks"

	pub, key, _ := ed25519.GenerateKey(nil)
	var packs atomic.Int32
	tag1, res1 := makeDisk(t, repo, ref, key, tarPack(&packs))
	if res1.Exists {
		t.Fatal("the first build found a disk")
	}
	tag2, res2 := makeDisk(t, repo, ref, key, tarPack(&packs))
	if !res2.Exists || tag2 != tag1 {
		t.Fatalf("second build = %+v, tag %v; want Exists and %v", res2, tag2, tag1)
	}
	if packs.Load() != 1 {
		t.Fatalf("packs = %d, want 1: a digest is built once", packs.Load())
	}
	if _, err := (&Builder{DiskRepo: repo, KeyID: KeyID(pub)}).Build(t.Context(), u.Host+"/tools/scanner:v1"); err == nil {
		t.Fatal("Build accepted a tag")
	}

	// The node side: the kubelet unpacks the two layers into one image
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
	if got, err := DiskRef(repo, ref, KeyID(pub)); err != nil || got != tag1.String() {
		t.Fatalf("DiskRef = %q, %v; want %q", got, err, tag1)
	}
	// The image config lists the diff id of each layer, which the node
	// checks when it unpacks them.
	cf, err := art.ConfigFile()
	if err != nil || len(cf.RootFS.DiffIDs) != 2 || cf.RootFS.DiffIDs[0].String() != res1.Layer.DiffID {
		t.Fatalf("config diff ids = %v, %v; want the disk layer first", cf.RootFS.DiffIDs, err)
	}
}

// The publish step writes only a signature that names the image and the
// disk of the build report. A signature of another image or of another
// disk is refused, so a build report of one image never gets the tag of
// another.
func TestPublisher_RefusesASignatureOfAnotherDisk(t *testing.T) {
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
	repo := u.Host + "/setec-disks"
	pub, key, _ := ed25519.GenerateKey(nil)
	var packs atomic.Int32
	res, err := (&Builder{DiskRepo: repo, KeyID: KeyID(pub), Pack: tarPack(&packs), TempDir: t.TempDir()}).Build(t.Context(), ref)
	if err != nil {
		t.Fatal(err)
	}
	p := &Publisher{DiskRepo: repo, TempDir: t.TempDir()}
	for name, sig := range map[string]Signature{
		"another image": SignDigest(key, "sha256:"+strings.Repeat("b", 64), res.DiskSHA256),
		"another disk":  SignDigest(key, dg.String(), strings.Repeat("c", 64)),
	} {
		if _, err := p.Publish(t.Context(), ref, KeyID(pub), res, sig); !errors.Is(err, ErrBadSignature) {
			t.Errorf("%s: Publish = %v, want ErrBadSignature", name, err)
		}
	}
	if _, err := p.Publish(t.Context(), ref, KeyID(pub), BuildResult{Exists: true}, SignDigest(key, dg.String(), res.DiskSHA256)); !errors.Is(err, ErrBadBuildResult) {
		t.Errorf("Publish of an existing disk = %v, want ErrBadBuildResult", err)
	}
}

// The operator signs what the build report names, so it reads the report
// strictly: a field it does not know, a short digest, an empty size or a
// disk SHA-256 that is not hex is refused.
func TestParseBuildResult_IsStrict(t *testing.T) {
	t.Parallel()
	good := BuildResult{
		Layer:      LayerRef{Digest: "sha256:" + strings.Repeat("a", 64), DiffID: "sha256:" + strings.Repeat("b", 64), Size: 10},
		DiskSHA256: strings.Repeat("c", 64),
	}
	raw, _ := json.Marshal(good)
	if got, err := ParseBuildResult(raw); err != nil || got != good {
		t.Fatalf("ParseBuildResult(good) = %+v, %v", got, err)
	}
	if got, err := ParseBuildResult([]byte(`{"exists":true,"layer":{"digest":"","diffID":"","size":0}}`)); err != nil || !got.Exists {
		t.Fatalf("ParseBuildResult(exists) = %+v, %v", got, err)
	}
	for name, mut := range map[string]func(*BuildResult){
		"short digest":   func(r *BuildResult) { r.Layer.Digest = "sha256:abc" },
		"bad diff id":    func(r *BuildResult) { r.Layer.DiffID = "md5:" + strings.Repeat("b", 64) },
		"no size":        func(r *BuildResult) { r.Layer.Size = 0 },
		"disk not hex":   func(r *BuildResult) { r.DiskSHA256 = strings.Repeat("z", 64) },
		"exists + layer": func(r *BuildResult) { r.Exists = true },
	} {
		r := good
		mut(&r)
		raw, _ := json.Marshal(r)
		if _, err := ParseBuildResult(raw); !errors.Is(err, ErrBadBuildResult) {
			t.Errorf("%s: err %v, want ErrBadBuildResult", name, err)
		}
	}
	if _, err := ParseBuildResult([]byte(`{"diskSHA256":"` + strings.Repeat("c", 64) + `","extra":1}`)); !errors.Is(err, ErrBadBuildResult) {
		t.Errorf("unknown field: err %v, want ErrBadBuildResult", err)
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
// signing seed, the build must not reuse the disk of the previous key. It
// builds a new disk under a new tag, beside the old one, in a registry with
// immutable tags. A launcher that trusts only the new key accepts the new
// disk, and the disk of the previous key stays for the launchers that still
// trust it.
func TestDisk_ANewSeedSignsANewDisk(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(immutableTags(registry.New()))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	src, _ := name.NewTag(u.Host + "/tools/scanner:v1")
	img := testImage(t)
	if err := remote.Write(src, img); err != nil {
		t.Fatal(err)
	}
	dg, _ := img.Digest()
	ref := u.Host + "/tools/scanner@" + dg.String()
	repo := u.Host + "/setec-disks"

	oldPub, oldKey, _ := ed25519.GenerateKey(nil)
	newPub, newKey, _ := ed25519.GenerateKey(nil)
	var packs atomic.Int32
	oldTag, _ := makeDisk(t, repo, ref, oldKey, tarPack(&packs))
	newTag, newRes := makeDisk(t, repo, ref, newKey, tarPack(&packs))
	if newRes.Exists {
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
	if got, err := DiskRef(repo, ref, KeyID(newPub)); err != nil || got != newTag.String() {
		t.Fatalf("DiskRef of the new key = %q, %v; want %q", got, err, newTag)
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

// checkDiskArtifact checks that the disk at tag is two OCI layers of gzip
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
	if err != nil || len(layers) != 2 {
		t.Fatalf("layers = %d, %v; want 2", len(layers), err)
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
