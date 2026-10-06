// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package launcher

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
)

// Formatter makes an empty ext4 file system on a new writable layer.
type Formatter func(path string) error

// MkfsExt4 formats with mkfs.ext4 from the launcher image. Lazy init keeps
// the format fast on a large sparse file.
func MkfsExt4(path string) error {
	out, err := exec.Command("mkfs.ext4", "-q", "-F", //nolint:gosec // a path the launcher made
		"-E", "lazy_itable_init=1,lazy_journal_init=1", path).CombinedOutput()
	if err != nil {
		return fmt.Errorf("mkfs.ext4: %w: %s", err, out)
	}
	return nil
}

// prepareDisks makes and formats the writable layer when it does not exist,
// and links each disk into WorkDir under its fixed name.
func (s *Spec) prepareDisks(format Formatter) error {
	if _, err := os.Stat(s.ImageDisk); err != nil {
		return fmt.Errorf("the image disk: %w", err)
	}
	if _, err := os.Stat(s.WritableDisk); errors.Is(err, os.ErrNotExist) {
		f, err := os.OpenFile(s.WritableDisk, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return fmt.Errorf("make the writable layer: %w", err)
		}
		if err := f.Truncate(s.WritableBytes); err != nil {
			_ = f.Close()
			return fmt.Errorf("size the writable layer: %w", err)
		}
		if err := f.Close(); err != nil {
			return err
		}
		if err := format(s.WritableDisk); err != nil {
			_ = os.Remove(s.WritableDisk)
			return fmt.Errorf("format the writable layer: %w", err)
		}
	} else if err != nil {
		return fmt.Errorf("the writable layer: %w", err)
	}
	links := map[string]string{imageLink: s.ImageDisk, writableLink: s.WritableDisk}
	if s.WorkspaceDevice != "" {
		if _, err := os.Stat(s.WorkspaceDevice); err != nil {
			return fmt.Errorf("the workspace device: %w", err)
		}
		// A new workspace is empty. The guest mounts an ext4 file system
		// from it, so the first launch of a session formats it, and each
		// later launch keeps what is there.
		has, err := hasExt4(s.WorkspaceDevice)
		if err != nil {
			return fmt.Errorf("read the workspace device: %w", err)
		}
		if !has {
			if err := format(s.WorkspaceDevice); err != nil {
				return fmt.Errorf("format the workspace: %w", err)
			}
		}
		links[workspaceLink] = s.WorkspaceDevice
	}
	for name, target := range links {
		link := filepath.Join(s.WorkDir, name)
		_ = os.Remove(link)
		if err := os.Symlink(target, link); err != nil {
			return fmt.Errorf("link %s: %w", name, err)
		}
	}
	return nil
}

// ext4 keeps its superblock at byte 1024, with the magic 0xEF53 at offset
// 56 of it, little endian.
const ext4MagicOffset = 1024 + 56

// hasExt4 reports whether path holds an ext4 (or ext2/3) file system.
func hasExt4(path string) (bool, error) {
	f, err := os.Open(path) //nolint:gosec // the workspace device of the spec
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()
	magic := make([]byte, 2)
	if _, err := f.ReadAt(magic, ext4MagicOffset); err != nil {
		if errors.Is(err, io.EOF) {
			return false, nil
		}
		return false, err
	}
	return magic[0] == 0x53 && magic[1] == 0xEF, nil
}
