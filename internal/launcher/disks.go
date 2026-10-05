// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package launcher

import (
	"errors"
	"fmt"
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
