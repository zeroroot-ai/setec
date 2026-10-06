// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package atrest

import (
	"bytes"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// TestZeroData_OverwritesTheDataAndKeepsTheHoles proves that the shred of
// a sparse file zeroes each data extent and does not fill the holes.
func TestZeroData_OverwritesTheDataAndKeepsTheHoles(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "sparse")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	const size = 1 << 30
	_ = f.Truncate(size)
	secret := bytes.Repeat([]byte("secret!!"), 1024)
	for _, off := range []int64{0, 512 << 20} {
		if _, err := f.WriteAt(secret, off); err != nil {
			t.Fatal(err)
		}
	}
	if err := zeroData(f, size); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	got := make([]byte, len(secret))
	g, _ := os.Open(path)
	defer func() { _ = g.Close() }()
	for _, off := range []int64{0, 512 << 20} {
		_, _ = g.ReadAt(got, off)
		if bytes.Contains(got, []byte("secret")) {
			t.Fatalf("data at %d survived the shred", off)
		}
	}
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		t.Fatal(err)
	}
	if allocated := st.Blocks * 512; allocated > 64<<20 {
		t.Fatalf("the shred allocated %d bytes of a sparse file; it filled the holes", allocated)
	}
	if err := Shred(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("Shred left the file")
	}
}
