// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package grpcserver

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestLauncherFrame_RoundTripKeepsTheHolesOfTheWritableLayer stores a
// sparse writable layer of 64 MiB with two small data extents and restores
// it byte for byte. The stream holds the data, not the holes.
func TestLauncherFrame_RoundTripKeepsTheHolesOfTheWritableLayer(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sp, mp, dp := filepath.Join(dir, "s"), filepath.Join(dir, "m"), filepath.Join(dir, "d")
	_ = os.WriteFile(sp, []byte("state"), 0o600)
	_ = os.WriteFile(mp, bytes.Repeat([]byte{7}, 4096), 0o600)
	f, err := os.Create(dp)
	if err != nil {
		t.Fatal(err)
	}
	const size = 64 << 20
	_ = f.Truncate(size)
	_, _ = f.WriteAt([]byte("superblock"), 1024)
	_, _ = f.WriteAt(bytes.Repeat([]byte{9}, 8192), 40<<20)
	_ = f.Close()

	copyPath := filepath.Join(dir, "copy")
	if err := copySparse(dp, copyPath); err != nil {
		t.Fatal(err)
	}
	rc, err := makeLauncherFramedReader(sp, mp, copyPath)
	if err != nil {
		t.Fatal(err)
	}
	var stream bytes.Buffer
	if _, err := stream.ReadFrom(rc); err != nil {
		t.Fatal(err)
	}
	_ = rc.Close()
	if stream.Len() > 1<<20 {
		t.Fatalf("the stream is %d bytes; the holes of the writable layer were stored", stream.Len())
	}

	out := t.TempDir()
	os2, om, od := filepath.Join(out, "s"), filepath.Join(out, "m"), filepath.Join(out, "d")
	if err := writeLauncherFramedStream(bytes.NewReader(stream.Bytes()), os2, om, od); err != nil {
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
	if err := writeLauncherFramedStream(bytes.NewReader(make([]byte, 64)), os2, om, od); err == nil {
		t.Fatal("a stream with no launcher magic was accepted")
	}
}
