// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package guestagent

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/zeroroot-ai/setec/internal/errwrap"
)

// copyInDirMode is the mode of a directory that copy-in makes: the owner of
// the agent writes, and the workload user reads and enters.
const copyInDirMode = 0o700 | 0o055

// symlinkFree resolves a path below a directory file descriptor and refuses
// each symbolic link on the way, and refuses each path that leaves the root.
const symlinkFree = unix.RESOLVE_IN_ROOT | unix.RESOLVE_NO_SYMLINKS

// copyInto writes size bytes from r to the absolute path abs below root with
// mode. root is the root of the image, or "" for the root of the machine. The
// write never follows a symbolic link, so a link in the image cannot send it
// outside root. Each directory that this call makes gets copyInDirMode,
// whatever the umask of the agent, and a directory that exists keeps its mode.
func copyInto(root, abs string, mode os.FileMode, r io.Reader, size int64) error {
	if root == "" {
		root = "/"
	}
	dirFD, err := unix.Open(root, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return errwrap.Wrap(err, "unix.Open")
	}
	rel := strings.TrimPrefix(abs, "/")
	if rel == "" || rel == "." {
		_ = unix.Close(dirFD)
		return fmt.Errorf("copy-in: %q names no file", abs)
	}
	dir, name := filepath.Split(rel)
	for el := range strings.SplitSeq(strings.Trim(dir, "/"), "/") {
		if el == "" {
			continue
		}
		next, derr := enterDir(dirFD, el)
		_ = unix.Close(dirFD)
		if derr != nil {
			return derr
		}
		dirFD = next
	}
	defer func() { _ = unix.Close(dirFD) }()
	fd, err := unix.Openat2(dirFD, name, &unix.OpenHow{
		Flags:   unix.O_WRONLY | unix.O_CREAT | unix.O_TRUNC | unix.O_CLOEXEC | unix.O_NOFOLLOW,
		Mode:    uint64(mode.Perm()),
		Resolve: symlinkFree,
	})
	if err != nil {
		return errwrap.Wrap(err, "unix.Openat2")
	}
	f := os.NewFile(uintptr(fd), name)
	n, err := io.CopyN(f, r, size)
	if err == nil {
		// The umask of the agent must not change the mode that the launcher asked for.
		err = f.Chmod(mode)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("copy-in: wrote %d of %d bytes: %w", n, size, err)
	}
	return nil
}

// enterDir opens the directory el below parent without a symbolic link, and
// makes it with copyInDirMode when it does not exist.
func enterDir(parent int, el string) (int, error) {
	how := &unix.OpenHow{Flags: unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC | unix.O_NOFOLLOW, Resolve: symlinkFree}
	fd, err := unix.Openat2(parent, el, how)
	if err == nil {
		return fd, nil
	}
	if !errors.Is(err, unix.ENOENT) {
		return -1, errwrap.Wrap(err, "unix.Openat2")
	}
	if err := unix.Mkdirat(parent, el, 0o700); err != nil && !errors.Is(err, unix.EEXIST) {
		return -1, errwrap.Wrap(err, "unix.Mkdirat")
	}
	fd, err = unix.Openat2(parent, el, how)
	if err != nil {
		return -1, errwrap.Wrap(err, "unix.Openat2")
	}
	if err := unix.Fchmod(fd, copyInDirMode); err != nil {
		_ = unix.Close(fd)
		return -1, errwrap.Wrap(err, "unix.Fchmod")
	}
	return fd, nil
}
