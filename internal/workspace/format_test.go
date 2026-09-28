// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package workspace

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	diskfs "github.com/diskfs/go-diskfs"
	"github.com/diskfs/go-diskfs/filesystem/ext4"
)

// blankDevice returns a regular file standing in for an unformatted
// block device. go-diskfs and this package's own byte-offset reads
// operate identically on a regular file or a real block device special
// file, so a temp file is a faithful stand-in for everything this
// package does except the final mount(2) call, which needs
// CAP_SYS_ADMIN and is proved empirically in the e2e suite instead.
func blankDevice(t *testing.T, size int64) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "device.img")
	if err := os.WriteFile(path, make([]byte, size), 0o600); err != nil {
		t.Fatalf("create blank device fixture: %v", err)
	}
	return path
}

func TestIsFormatted_BlankDeviceIsFalse(t *testing.T) {
	dev := blankDevice(t, 16<<20)
	got, err := IsFormatted(dev)
	if err != nil {
		t.Fatalf("IsFormatted: %v", err)
	}
	if got {
		t.Fatalf("IsFormatted(blank) = true, want false")
	}
}

func TestIsFormatted_TooShortForSuperblockIsFalse(t *testing.T) {
	// Shorter than the superblock's magic-number offset (1080 bytes).
	// A real device is never this small, but the detector must not
	// error on it — it must report "not formatted", not fail.
	dev := blankDevice(t, 64)
	got, err := IsFormatted(dev)
	if err != nil {
		t.Fatalf("IsFormatted: %v", err)
	}
	if got {
		t.Fatalf("IsFormatted(too-short) = true, want false")
	}
}

func TestIsFormatted_AfterFormatIsTrue(t *testing.T) {
	dev := blankDevice(t, 16<<20)
	if err := FormatOnce(dev); err != nil {
		t.Fatalf("FormatOnce: %v", err)
	}
	got, err := IsFormatted(dev)
	if err != nil {
		t.Fatalf("IsFormatted: %v", err)
	}
	if !got {
		t.Fatalf("IsFormatted(freshly formatted) = false, want true")
	}
}

// TestFormatOnce_ProducesAReadableExt4Filesystem proves FormatOnce does
// not merely write a plausible magic number: the result is a real,
// readable ext4 filesystem.
func TestFormatOnce_ProducesAReadableExt4Filesystem(t *testing.T) {
	dev := blankDevice(t, 16<<20)
	if err := FormatOnce(dev); err != nil {
		t.Fatalf("FormatOnce: %v", err)
	}

	d, err := diskfs.Open(dev)
	if err != nil {
		t.Fatalf("open formatted device: %v", err)
	}
	fs, err := ext4.Read(d.Backend, d.Size, 0, d.LogicalBlocksize)
	if err != nil {
		t.Fatalf("read back ext4 filesystem: %v", err)
	}
	// go-diskfs paths follow io/fs.ValidPath: no leading slash, "."
	// for the root directory.
	if _, err := fs.ReadDir("."); err != nil {
		t.Fatalf("read root directory of formatted filesystem: %v", err)
	}
}

// TestFormatOnce_NeverReformatsExistingFilesystem is the failing
// fixture for setec#91's core guarantee: calling FormatOnce a second
// time against an already-formatted device must be a complete no-op.
// Without the IsFormatted gate, a second FormatOnce call would run mkfs
// again — ext4.Create mints a fresh UUID and superblock timestamps on
// every call, so the device's bytes would change and this test would
// fail, proving the guard is load-bearing rather than decorative.
func TestFormatOnce_NeverReformatsExistingFilesystem(t *testing.T) {
	dev := blankDevice(t, 16<<20)

	if err := FormatOnce(dev); err != nil {
		t.Fatalf("first FormatOnce: %v", err)
	}
	before, err := os.ReadFile(dev)
	if err != nil {
		t.Fatalf("read device after first format: %v", err)
	}

	// Simulate a session VM restart: the same PVC, a fresh init
	// container run of the exact same logic.
	if err := FormatOnce(dev); err != nil {
		t.Fatalf("second FormatOnce: %v", err)
	}
	after, err := os.ReadFile(dev)
	if err != nil {
		t.Fatalf("read device after second format: %v", err)
	}

	if !bytes.Equal(before, after) {
		t.Fatalf("FormatOnce reformatted an already-formatted device: bytes changed, " +
			"a session's workspace would be silently wiped on every VM restart")
	}
}

// TestFormatOnce_PreservesDataWrittenAfterFormat goes one step further
// than the byte-identity check above: it writes real data into the
// formatted filesystem's data region and asserts FormatOnce leaves that
// data alone. This is the property the issue is actually about — a
// session's corpus surviving a VM restart — expressed without needing a
// real mount(2), which the unit test environment cannot perform.
func TestFormatOnce_PreservesDataWrittenAfterFormat(t *testing.T) {
	dev := blankDevice(t, 16<<20)
	if err := FormatOnce(dev); err != nil {
		t.Fatalf("first FormatOnce: %v", err)
	}

	// Write a session-marker file into the filesystem the same way the
	// e2e scenario's workload does after mounting, except here it goes
	// straight through go-diskfs since nothing is actually mounted.
	d, err := diskfs.Open(dev, diskfs.WithOpenMode(diskfs.ReadWriteExclusive))
	if err != nil {
		t.Fatalf("re-open formatted device: %v", err)
	}
	fs, err := ext4.Read(d.Backend, d.Size, 0, d.LogicalBlocksize)
	if err != nil {
		t.Fatalf("read back ext4 filesystem: %v", err)
	}
	wf, err := fs.OpenFile("marker", os.O_CREATE|os.O_RDWR)
	if err != nil {
		t.Fatalf("create marker file: %v", err)
	}
	if _, err := wf.Write([]byte("alive")); err != nil {
		t.Fatalf("write marker file: %v", err)
	}
	if err := wf.Close(); err != nil {
		t.Fatalf("close marker file: %v", err)
	}

	// The "VM restart": FormatOnce runs again against the same device.
	if err := FormatOnce(dev); err != nil {
		t.Fatalf("second FormatOnce: %v", err)
	}

	d2, err := diskfs.Open(dev)
	if err != nil {
		t.Fatalf("re-open device after second FormatOnce: %v", err)
	}
	fs2, err := ext4.Read(d2.Backend, d2.Size, 0, d2.LogicalBlocksize)
	if err != nil {
		t.Fatalf("read back ext4 filesystem after second FormatOnce: %v", err)
	}
	rf, err := fs2.OpenFile("marker", os.O_RDONLY)
	if err != nil {
		t.Fatalf("marker file did not survive a second FormatOnce (session data lost): %v", err)
	}
	got := make([]byte, 5)
	// Read may legitimately return n==len(got) together with io.EOF
	// (ordinary io.Reader behavior for a short underlying file), so
	// only a short read is an actual failure here.
	n, err := rf.Read(got)
	if n != len(got) && err != nil {
		t.Fatalf("read marker file: %v", err)
	}
	if string(got) != "alive" {
		t.Fatalf("marker file contents = %q, want %q", got, "alive")
	}
}

func TestFormatOnce_UnknownDeviceErrors(t *testing.T) {
	if err := FormatOnce(filepath.Join(t.TempDir(), "does-not-exist")); err == nil {
		t.Fatalf("FormatOnce(missing device) = nil error, want an error")
	}
}

// TestFormatOnce_ReleasesTheDevice asserts that FormatOnce keeps no
// descriptor open on the device. It opens the device O_EXCL, and a
// descriptor left open makes the mount(2) that follows fail with EBUSY
// on a real block device (setec#91, every first boot of a kata-fc
// session).
func TestFormatOnce_ReleasesTheDevice(t *testing.T) {
	dev := blankDevice(t, 16<<20)
	before := openDescriptorsOn(t, dev)
	if err := FormatOnce(dev); err != nil {
		t.Fatalf("FormatOnce: %v", err)
	}
	if after := openDescriptorsOn(t, dev); after != before {
		t.Fatalf("open descriptors on %s: %d before FormatOnce, %d after; the device was not released", dev, before, after)
	}
}

// openDescriptorsOn counts this process's open descriptors on path.
func openDescriptorsOn(t *testing.T, path string) int {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatalf("read /proc/self/fd: %v", err)
	}
	n := 0
	for _, e := range entries {
		if target, err := os.Readlink(filepath.Join("/proc/self/fd", e.Name())); err == nil && target == path {
			n++
		}
	}
	return n
}
