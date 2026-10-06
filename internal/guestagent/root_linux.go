// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

//go:build linux

package guestagent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/zeroroot-ai/setec/internal/errwrap"
)

// The disks of a launcher machine, in the order the launcher attaches them.
const (
	ImageDevice     = "/dev/vda"
	WritableDevice  = "/dev/vdb"
	WorkspaceDevice = "/dev/vdc"

	// NewRoot is the root of the image: the read-only image disk under the
	// writable layer, joined by overlayfs.
	NewRoot = "/newroot"
)

// The kernel file systems that the root of the workload needs.
const (
	fsDevtmpfs = "devtmpfs"
	fsProc     = "proc"
	fsSysfs    = "sysfs"
	fsTmpfs    = "tmpfs"
	dirDev     = "/dev"
	dirProc    = "/proc"
	dirSys     = "/sys"
)

// PrepareRoot runs once as PID 1. It mounts the kernel file systems, joins
// the image disk and the writable layer as the root of the workload, and
// mounts the session workspace at /workspace when the machine has one.
// lowerFS is the file system of the image disk, from the kernel command
// line (setec.lowerfs=), squashfs by default.
func PrepareRoot(lowerFS string) error {
	if lowerFS == "" {
		lowerFS = "squashfs"
	}
	for _, d := range []string{dirProc, dirSys, dirDev, "/lower", "/rw", NewRoot} {
		if err := os.MkdirAll(d, 0o755); err != nil { //nolint:gosec // a directory of the guest system, which each guest user must read
			return errwrap.Wrap(err, "os.MkdirAll")
		}
	}
	mounts := []struct {
		src, dst, fs string
		flags        uintptr
		data         string
	}{
		{fsDevtmpfs, dirDev, fsDevtmpfs, 0, ""},
		{fsProc, dirProc, fsProc, 0, ""},
		{fsSysfs, dirSys, fsSysfs, 0, ""},
		{ImageDevice, "/lower", lowerFS, syscall.MS_RDONLY, ""},
		{WritableDevice, "/rw", "ext4", 0, ""},
	}
	for _, m := range mounts {
		if err := syscall.Mount(m.src, m.dst, m.fs, m.flags, m.data); err != nil && !errors.Is(err, syscall.EBUSY) {
			return fmt.Errorf("mount %s on %s: %w", m.src, m.dst, err)
		}
	}
	for _, d := range []string{"/rw/upper", "/rw/work"} {
		if err := os.MkdirAll(d, 0o755); err != nil { //nolint:gosec // a directory of the guest system, which each guest user must read
			return errwrap.Wrap(err, "os.MkdirAll")
		}
	}
	if err := syscall.Mount("overlay", NewRoot, "overlay", 0,
		"lowerdir=/lower,upperdir=/rw/upper,workdir=/rw/work"); err != nil {
		return fmt.Errorf("mount the overlay root: %w", err)
	}
	inner := []struct{ src, dst, fs, data string }{
		{fsProc, dirProc, fsProc, ""},
		{fsSysfs, dirSys, fsSysfs, ""},
		{fsDevtmpfs, dirDev, fsDevtmpfs, ""},
		{"devpts", "/dev/pts", "devpts", "newinstance,ptmxmode=0666"},
		{fsTmpfs, "/dev/shm", fsTmpfs, "mode=1777"},
		{fsTmpfs, "/tmp", fsTmpfs, "mode=1777"},
		{fsTmpfs, "/run", fsTmpfs, "mode=755"},
	}
	for _, m := range inner {
		dst := filepath.Join(NewRoot, m.dst)
		if err := os.MkdirAll(dst, 0o755); err != nil { //nolint:gosec // a directory of the guest system, which each guest user must read
			return errwrap.Wrap(err, "os.MkdirAll")
		}
		if err := syscall.Mount(m.src, dst, m.fs, 0, m.data); err != nil {
			return fmt.Errorf("mount %s in the root: %w", m.dst, err)
		}
	}
	if _, err := os.Stat(WorkspaceDevice); err == nil {
		dst := filepath.Join(NewRoot, "workspace")
		if err := os.MkdirAll(dst, 0o755); err != nil { //nolint:gosec // a directory of the guest system, which each guest user must read
			return errwrap.Wrap(err, "os.MkdirAll")
		}
		if err := syscall.Mount(WorkspaceDevice, dst, "ext4", 0, ""); err != nil {
			return fmt.Errorf("mount the workspace: %w", err)
		}
	}
	return nil
}

// KernelArg returns the value of key=value on the kernel command line.
func KernelArg(key string) string {
	raw, _ := os.ReadFile("/proc/cmdline")
	for f := range strings.FieldsSeq(string(raw)) {
		if v, ok := strings.CutPrefix(f, key+"="); ok {
			return v
		}
	}
	return ""
}
