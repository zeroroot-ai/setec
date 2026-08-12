/*
Copyright 2026 The Setec Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package installer

import (
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// hostPath joins a host-absolute path onto the configured host root.
func (in *Installer) hostPath(parts ...string) string {
	return filepath.Join(append([]string{in.cfg.HostRoot}, parts...)...)
}

// writeFileIfChanged writes content to path (creating parent directories)
// only when the current content differs. Returns whether a write
// happened. Writes go through a same-directory temp file + rename so a
// crash mid-write never leaves a truncated systemd unit or containerd
// config behind.
func writeFileIfChanged(path string, content []byte, mode os.FileMode) (bool, error) {
	current, err := os.ReadFile(path)
	if err == nil && bytes.Equal(current, content) {
		// Content matches; still make sure the mode is right (cheap, and
		// keeps a manually chmod-ed script from staying non-executable).
		if info, statErr := os.Stat(path); statErr == nil && info.Mode().Perm() != mode.Perm() {
			if chmodErr := os.Chmod(path, mode); chmodErr != nil {
				return false, chmodErr
			}
		}
		return false, nil
	}
	if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".setec-installer-*")
	if err != nil {
		return false, err
	}
	tmpName := tmp.Name()
	// Best-effort cleanup: the rename below makes it a no-op on success, and
	// on failure the caller already has the real error.
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		return false, err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return false, err
	}
	if err := tmp.Close(); err != nil {
		return false, err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return false, err
	}
	return true, nil
}

// ensureSymlink makes path a symlink pointing at target. Returns whether
// anything changed.
func ensureSymlink(path, target string) (bool, error) {
	if existing, err := os.Readlink(path); err == nil && existing == target {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	// Symlinks cannot be atomically replaced in place; link to a temp
	// name and rename over.
	tmp := path + ".setec-installer-tmp"
	_ = os.Remove(tmp)
	if err := os.Symlink(target, tmp); err != nil {
		return false, err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return false, err
	}
	return true, nil
}

// copyTree copies src into dst (creating dst), preserving file modes and
// symlinks. Existing files are overwritten; files present only in dst are
// left alone (the payload is versioned as a whole via its VERSION file,
// so stale-file pruning happens by replacing the tree when the version
// changes — see ensureKataPayload).
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			return os.MkdirAll(target, info.Mode().Perm())
		case info.Mode()&os.ModeSymlink != 0:
			linkTarget, err := os.Readlink(path)
			if err != nil {
				return err
			}
			_, err = ensureSymlink(target, linkTarget)
			return err
		case info.Mode().IsRegular():
			return copyFile(path, target, info.Mode().Perm())
		default:
			return fmt.Errorf("payload contains unsupported file type %s: %s", info.Mode(), path)
		}
	})
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".setec-installer-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := io.Copy(tmp, in); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, dst)
}
