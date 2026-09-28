// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

// Package workspace formats and mounts the durable per-session
// workspace volume for the kata-fc backend (setec#91, ADR-0007
// addendum).
//
// Kata Containers + Firecracker carries no virtio-fs, so kata cannot
// share a host directory with the guest the way it does on kata-qemu:
// it copies a filesystem-mode volume's contents into the guest once, at
// container start, and guest writes never reach the PVC back on the
// host. A raw block device is the one volume type Firecracker can
// attach to the guest (the same mechanism kata already uses for a
// container's own rootfs), so the workspace PVC is provisioned with
// volumeMode: Block on this backend and reaches the guest as a block
// device instead of a mounted filesystem. Something inside the guest
// has to turn that block device back into a filesystem before the
// workload can use it — that is this package's job.
//
// Format-once is the safety property that matters most here: a
// session's workspace PVC re-attaches to a brand-new Pod on every VM
// restart (a node dying, an eviction, an explicit Pod delete), and each
// of those incarnations runs this package's logic again. Reformatting
// an already-formatted device would silently destroy the very corpus
// ADR-0006/0007 promise a session never loses, so FormatOnce checks for
// an existing filesystem before ever calling mkfs.
package workspace

import (
	"errors"
	"fmt"
	"io"
	"os"

	diskfs "github.com/diskfs/go-diskfs"
	"github.com/diskfs/go-diskfs/disk"
	"github.com/diskfs/go-diskfs/filesystem"
	"golang.org/x/sys/unix"
)

const (
	// ext4SuperblockOffset is the byte offset of the ext2/ext3/ext4
	// superblock from the start of the volume. This has been fixed
	// since the original ext2 on-disk layout and has never moved
	// across ext3 or ext4.
	ext4SuperblockOffset = 1024

	// ext4SuperblockMagicOffset is the byte offset of the s_magic
	// field within the superblock.
	ext4SuperblockMagicOffset = 56

	// ext4SuperblockMagicAbsolute is the absolute byte offset of
	// s_magic from the start of the volume: 1024 + 56 = 1080.
	ext4SuperblockMagicAbsolute = ext4SuperblockOffset + ext4SuperblockMagicOffset

	// ext4Magic is the little-endian value ext2/ext3/ext4 write to
	// s_magic. It is a detector, not a filesystem check: this package
	// trusts nothing past this one field, because a false negative
	// (formatting over real data) is far more expensive than a false
	// positive (skipping a format that was actually needed, which
	// just surfaces as a mount failure instead).
	ext4Magic = 0xEF53

	// extVolumeLabel is the label FormatOnce gives the filesystem it
	// creates. It has no functional role — nothing looks it up by
	// label — but it makes the volume identifiable with blkid/lsblk
	// during diagnostics.
	extVolumeLabel = "setec-workspace"
)

// IsFormatted reports whether device already carries an ext2/ext3/ext4
// filesystem, by reading the magic number at the fixed superblock
// offset. A device shorter than the superblock cannot carry a
// filesystem yet and reports false, not an error.
func IsFormatted(device string) (bool, error) {
	f, err := os.Open(device) //nolint:gosec // device is an operator-controlled block device path, not user input
	if err != nil {
		return false, fmt.Errorf("open %s: %w", device, err)
	}
	defer f.Close()

	buf := make([]byte, 2)
	n, err := f.ReadAt(buf, ext4SuperblockMagicAbsolute)
	if err != nil && !errors.Is(err, io.EOF) {
		return false, fmt.Errorf("read superblock magic from %s: %w", device, err)
	}
	if n < len(buf) {
		return false, nil
	}
	magic := uint16(buf[0]) | uint16(buf[1])<<8
	return magic == ext4Magic, nil
}

// FormatOnce formats device as ext4 unless it already carries a
// filesystem, in which case it does nothing. This is the single gate
// between a restarted session and mkfs silently wiping the corpus a
// prior VM incarnation wrote: calling mkfs unconditionally on every
// boot would format over the workspace every time the session's Pod is
// recreated.
func FormatOnce(device string) error {
	formatted, err := IsFormatted(device)
	if err != nil {
		return err
	}
	if formatted {
		return nil
	}

	d, err := diskfs.Open(device, diskfs.WithOpenMode(diskfs.ReadWriteExclusive))
	if err != nil {
		return fmt.Errorf("open %s for formatting: %w", device, err)
	}
	_, err = d.CreateFilesystem(disk.FilesystemSpec{
		Partition:   0, // whole device, no partition table — mkfs.ext4's default behavior on a bare device
		FSType:      filesystem.TypeExt4,
		VolumeLabel: extVolumeLabel,
	})
	// Close before returning: the device was opened O_EXCL, and the
	// mount(2) that follows fails with EBUSY while this process still
	// holds it (setec#91, every first boot of a kata-fc session).
	if cerr := d.Close(); cerr != nil && err == nil {
		err = fmt.Errorf("close %s after formatting: %w", device, cerr)
	} else if err != nil {
		err = fmt.Errorf("format %s as ext4: %w", device, err)
	}
	return err
}

// Mount mounts device at target as ext4.
//
// Requires CAP_SYS_ADMIN. Per ADR-0052 that costs nothing extra on
// kata-fc: the sandbox's containment boundary is the microVM the whole
// Pod runs inside, not this container's own capability set — the same
// reasoning that already re-adds NET_RAW/NET_ADMIN to the kata-fc
// workload container for raw-socket tooling.
func Mount(device, target string) error {
	if err := unix.Mount(device, target, "ext4", 0, ""); err != nil {
		return fmt.Errorf("mount %s at %s: %w", device, target, err)
	}
	return nil
}

// FormatAndMount is the single entry point a kata-fc session's workload
// container runs, before running its own command: format device once —
// never reformatting an existing filesystem — then mount it at target.
// Idempotent: a Pod that runs this again against an already-formatted
// device skips straight to mount, which is exactly what happens every
// time a session's Pod is recreated against its durable workspace PVC.
func FormatAndMount(device, target string) error {
	if err := FormatOnce(device); err != nil {
		return err
	}
	return Mount(device, target)
}
