// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package grpcserver

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zeroroot-ai/setec/internal/snapshot/secretscan"
)

// sparseFile makes a sparse file of size with data at the given offsets.
func sparseFile(t *testing.T, path string, size int64, data map[int64][]byte) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Truncate(size)
	for off, b := range data {
		if _, err := f.WriteAt(b, off); err != nil {
			t.Fatal(err)
		}
	}
	_ = f.Close()
}

// testImageDisk is the image disk digest of the test frames.
var testImageDisk = strings.Repeat("ab", 32)

func frameOf(t *testing.T, parent, state, mem, disk string) []byte {
	t.Helper()
	return frameOn(t, parent, testImageDisk, state, mem, disk)
}

func frameOn(t *testing.T, parent, imageDisk, state, mem, disk string) []byte {
	t.Helper()
	rc, err := makeLauncherFramedReader(parent, imageDisk, state, mem, disk)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()
	var b bytes.Buffer
	if _, err := b.ReadFrom(rc); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// TestLauncherFrame_RoundTripKeepsTheHolesOfTheWritableLayer stores a
// sparse writable layer of 64 MiB with two small data extents and restores
// it byte for byte. The stream holds the data, not the holes.
func TestLauncherFrame_RoundTripKeepsTheHolesOfTheWritableLayer(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sp, mp, dp := filepath.Join(dir, "s"), filepath.Join(dir, "m"), filepath.Join(dir, "d")
	_ = os.WriteFile(sp, []byte("state"), 0o600)
	_ = os.WriteFile(mp, bytes.Repeat([]byte{7}, 4096), 0o600)
	sparseFile(t, dp, 64<<20, map[int64][]byte{1024: []byte("superblock"), 40 << 20: bytes.Repeat([]byte{9}, 8192)})

	cp := filepath.Join(dir, "copy")
	if err := copySparse(dp, cp); err != nil {
		t.Fatal(err)
	}
	stream := frameOf(t, "", sp, mp, cp)
	if len(stream) > 1<<20 {
		t.Fatalf("the stream is %d bytes; the holes of the writable layer were stored", len(stream))
	}
	out := t.TempDir()
	os2, om, od := filepath.Join(out, "s"), filepath.Join(out, "m"), filepath.Join(out, "d")
	if got, err := writeLauncherFramedStream(bytes.NewReader(stream), os2, om, od, nil); err != nil || got != testImageDisk {
		t.Fatalf("writeLauncherFramedStream = %q, %v; want the image disk digest of the frame", got, err)
	}
	want, _ := os.ReadFile(dp)
	got, _ := os.ReadFile(od)
	if !bytes.Equal(want, got) {
		t.Fatal("the restored writable layer differs from the source")
	}
	if st, _ := os.ReadFile(os2); string(st) != "state" {
		t.Fatalf("state = %q", st)
	}
	// A stream with no launcher frame never loads as a launcher snapshot.
	if _, err := writeLauncherFramedStream(bytes.NewReader(make([]byte, 64)), os2, om, od, nil); err == nil {
		t.Fatal("a stream with no launcher magic was accepted")
	}
}

// TestLauncherFrame_DiffGoesOnTheMemoryOfItsParent restores a chain of a
// full snapshot and two diffs. Each diff memory holds only changed pages,
// as Firecracker writes it, and the result is the memory of the last one.
func TestLauncherFrame_DiffGoesOnTheMemoryOfItsParent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	const memSize = 1 << 20
	page := func(b byte) []byte { return bytes.Repeat([]byte{b}, 4096) }
	disk := filepath.Join(dir, "disk")
	sparseFile(t, disk, 1<<20, map[int64][]byte{0: []byte("fs")})
	be := memBackend{blobs: map[string][]byte{}}

	steps := []struct {
		ref, parent string
		pages       map[int64][]byte
	}{
		{"full", "", map[int64][]byte{0: page(1), 8192: page(1), 65536: page(1)}},
		{"diff1", "full", map[int64][]byte{8192: page(2)}},
		{"diff2", "diff1", map[int64][]byte{65536: page(3), 131072: page(3)}},
	}
	for _, st := range steps {
		state, mem := filepath.Join(dir, st.ref+".state"), filepath.Join(dir, st.ref+".mem")
		_ = os.WriteFile(state, []byte(st.ref), 0o600)
		sparseFile(t, mem, memSize, st.pages)
		be.blobs[st.ref] = frameOf(t, st.parent, state, mem, disk)
	}
	if len(be.blobs["diff1"]) > 64<<10 {
		t.Fatalf("diff1 is %d bytes; a diff must store only the changed pages", len(be.blobs["diff1"]))
	}

	out := t.TempDir()
	os2, om, od := filepath.Join(out, "s"), filepath.Join(out, "m"), filepath.Join(out, "d")
	rc, _ := be.Open(context.Background(), "diff2")
	if _, err := writeLauncherFramedStream(rc, os2, om, od, func(ref string) (io.ReadCloser, error) { return be.Open(context.Background(), ref) }); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(om)
	want := make([]byte, memSize)
	copy(want[0:], page(1))
	copy(want[8192:], page(2))
	copy(want[65536:], page(3))
	copy(want[131072:], page(3))
	if !bytes.Equal(got, want) {
		t.Fatal("the memory of the chain is not the memory of the last diff")
	}
	if st, _ := os.ReadFile(os2); string(st) != "diff2" {
		t.Fatalf("state = %q, want the state of the last diff", st)
	}

	// A diff with no way to open its parent fails.
	rc2, _ := be.Open(context.Background(), "diff1")
	if _, err := writeLauncherFramedStream(rc2, os2, om, od, nil); err == nil {
		t.Fatal("a diff with no parent opener was accepted")
	}

	// A diff whose parent ran on another image disk fails (setec#277).
	state, mem := filepath.Join(dir, "other.state"), filepath.Join(dir, "other.mem")
	_ = os.WriteFile(state, []byte("other"), 0o600)
	sparseFile(t, mem, memSize, map[int64][]byte{0: page(4)})
	be.blobs["diff-other"] = frameOn(t, "diff2", strings.Repeat("cd", 32), state, mem, disk)
	rc3, _ := be.Open(context.Background(), "diff-other")
	_, err := writeLauncherFramedStream(rc3, os2, om, od, func(ref string) (io.ReadCloser, error) { return be.Open(context.Background(), ref) })
	if err == nil || !strings.Contains(err.Error(), "another image disk") {
		t.Fatalf("a diff over a parent of another image disk = %v, want a refusal", err)
	}
}

// TestLauncherFrame_RecordsTheImageDisk proves that a frame carries the
// image disk digest, that a frame of the first form (no digest) does not
// load, and that a frame with no valid digest is not made (setec#277).
func TestLauncherFrame_RecordsTheImageDisk(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sp, mp, dp := filepath.Join(dir, "s"), filepath.Join(dir, "m"), filepath.Join(dir, "d")
	_ = os.WriteFile(sp, []byte("state"), 0o600)
	_ = os.WriteFile(mp, []byte("memory"), 0o600)
	_ = os.WriteFile(dp, []byte("disk"), 0o600)
	for _, bad := range []string{"", "abc", strings.Repeat("AB", 32), strings.Repeat("zz", 32)} {
		if _, err := makeLauncherFramedReader("", bad, sp, mp, dp); err == nil {
			t.Fatalf("a frame with the image disk digest %q was made", bad)
		}
	}
	stream := frameOf(t, "", sp, mp, dp)
	out := t.TempDir()
	os2, om, od := filepath.Join(out, "s"), filepath.Join(out, "m"), filepath.Join(out, "d")

	// The first form: the same frame with the old magic and no digest.
	v1 := append(append([]byte{}, launcherFrameMagicV1[:]...), stream[8:16]...)
	v1 = append(v1, stream[16+len(testImageDisk):]...)
	if _, err := writeLauncherFramedStream(bytes.NewReader(v1), os2, om, od, nil); err == nil ||
		!strings.Contains(err.Error(), "no image disk digest") {
		t.Fatalf("a frame of the first form = %v, want a refusal", err)
	}

	// A frame whose digest bytes are not a SHA-256 does not load.
	bad := append([]byte{}, stream...)
	copy(bad[16:], strings.Repeat("Z", len(testImageDisk)))
	if _, err := writeLauncherFramedStream(bytes.NewReader(bad), os2, om, od, nil); err == nil {
		t.Fatal("a frame with an invalid image disk digest loaded")
	}
}

// TestScanSparseFiles_FindsASecretInADataExtent proves that the base scan
// reads the data of a sparse writable layer and refuses a secret in it.
func TestScanSparseFiles_FindsASecretInADataExtent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	clean := filepath.Join(dir, "clean")
	sparseFile(t, clean, 256<<20, map[int64][]byte{4096: []byte("nothing to see")})
	if err := scanSparseFiles([]string{clean}); err != nil {
		t.Fatalf("a clean file: %v", err)
	}
	dirty := filepath.Join(dir, "dirty")
	sparseFile(t, dirty, 256<<20, map[int64][]byte{200 << 20: []byte("-----BEGIN RSA PRIVATE KEY-----\nMIIabc\n")})
	err := scanSparseFiles([]string{clean, dirty})
	if !errors.Is(err, secretscan.ErrSecretsFound) {
		t.Fatalf("a key in a data extent = %v, want ErrSecretsFound", err)
	}
	// The error names the rule, so a failed base says what matched.
	if !strings.Contains(err.Error(), "pem-private-key") {
		t.Fatalf("the error does not name the rule: %v", err)
	}
}
