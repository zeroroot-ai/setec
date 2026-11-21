// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

// Package sandboxfiles holds the rules for the files that a Sandbox places
// in its machine (docs/design/files.md). The frontend, the admission webhook
// and the operator share it, so one rule has one place.
package sandboxfiles

import (
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
)

// Dir is the directory in the machine that holds the files. A request names a
// path below it.
const Dir = "/setec/files"

// MaxTotalBytes is the bound on the total size of the files of one Sandbox.
// The size of a file is its content, its path and PerFileOverhead. The
// operator keeps the content and a manifest of the paths in a Secret, and a
// Secret holds at most 1 MiB. The number of files has no bound of its own.
const MaxTotalBytes = 512 << 10

// PerFileOverhead is the size that each file adds to the total for its entry
// in the manifest and its key in the Secret.
const PerFileOverhead = 64

// DefaultMode is the mode of a file that names none.
const DefaultMode = 0o444

// allowedModeBits are the only bits of a mode: read and execute. A file of
// the machine is never writable by the workload through its mode.
const allowedModeBits = 0o555

// MachinePath is the absolute path in the machine of the file with the
// request path rel.
func MachinePath(rel string) string { return Dir + "/" + rel }

// ModeOf returns the mode of f, or DefaultMode when f names none.
func ModeOf(f setecv1alpha1.SandboxFile) uint32 {
	if f.Mode == nil {
		return DefaultMode
	}
	return uint32(*f.Mode & allowedModeBits)
}

// ValidatePath reports why rel is not a path below Dir.
func ValidatePath(rel string) error {
	switch {
	case rel == "":
		return errors.New("the path is empty")
	case len(rel) > 512:
		return errors.New("the path is longer than 512 bytes")
	case strings.HasPrefix(rel, "/"):
		return errors.New("the path must be relative to " + Dir)
	case strings.ContainsAny(rel, "\x00\\"):
		return errors.New("the path holds a NUL byte or a backslash")
	case path.Clean(rel) != rel:
		return errors.New("the path must be clean: no empty, \".\" or \"..\" element")
	}
	if slices.Contains(strings.Split(rel, "/"), "..") {
		return errors.New("the path must be clean: no \"..\" element")
	}
	return nil
}

// Validate reports the first problem of files: a bad path, a path that
// appears twice or that is the directory of another path, a mode that
// grants a write bit, a setuid bit or a setgid bit, or a total content
// above MaxTotalBytes.
func Validate(files []setecv1alpha1.SandboxFile) error {
	seen := make(map[string]struct{}, len(files))
	total := 0
	for i, f := range files {
		if err := ValidatePath(f.Path); err != nil {
			return fmt.Errorf("files[%d]: %w", i, err)
		}
		if _, dup := seen[f.Path]; dup {
			return fmt.Errorf("files[%d]: the path %q appears twice", i, f.Path)
		}
		seen[f.Path] = struct{}{}
		if f.Mode != nil && (*f.Mode < 0 || *f.Mode&^allowedModeBits != 0) {
			return fmt.Errorf("files[%d]: the mode %04o must grant no write, setuid or setgid bit (at most 0555)", i, *f.Mode)
		}
		total += len(f.Content) + len(f.Path) + PerFileOverhead
		if total > MaxTotalBytes {
			return fmt.Errorf("files: the total size of the content, the paths and %d bytes for each file is above %d bytes", PerFileOverhead, MaxTotalBytes)
		}
	}
	for p := range seen {
		for dir := path.Dir(p); dir != "."; dir = path.Dir(dir) {
			if _, clash := seen[dir]; clash {
				return fmt.Errorf("files: the path %q is a file and the directory of %q", dir, p)
			}
		}
	}
	return nil
}
