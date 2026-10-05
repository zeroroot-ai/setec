// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package launcher

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// prepareDisks makes the writable layer when it does not exist, and links
// each disk into WorkDir under its fixed name.
func (s *Spec) prepareDisks() error {
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
