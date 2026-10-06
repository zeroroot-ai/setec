// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package grpcserver

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
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

func frameOf(t *testing.T, parent, state, mem, disk string) []byte {
	t.Helper()
	rc, err := makeLauncherFramedReader(parent, state, mem, disk)
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
	if err := writeLauncherFramedStream(bytes.NewReader(stream), os2, om, od, nil); err != nil {
		t.Fatal(err)
	}
	want, _ := os.ReadFile(dp)
	got, _ := os.ReadFile(od)
	if !bytes.Equal(want, got) {
		t.Fatal("the restored writable layer differs from the source")
	}
	if st, _ := os.ReadFile(os2); string(st) != "state" {
		t.Fatalf("state = %q", st)
	}
	// A kata snapshot never loads as a launcher snapshot.
	if err := writeLauncherFramedStream(bytes.NewReader(make([]byte, 64)), os2, om, od, nil); err == nil {
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
	if err := writeLauncherFramedStream(rc, os2, om, od, func(ref string) (io.ReadCloser, error) { return be.Open(context.Background(), ref) }); err != nil {
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
	if err := writeLauncherFramedStream(rc2, os2, om, od, nil); err == nil {
		t.Fatal("a diff with no parent opener was accepted")
	}
}
