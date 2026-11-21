// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

// Package sandboxfiles holds the rules for the files that a Sandbox places
// in its machine (docs/design/files.md). The frontend, the admission webhook
// and the operator share it, so one rule has one place.
package sandboxfiles

import (
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
)

// Dir is the directory in the machine that holds the files. A request names a
// path below it.
const Dir = "/setec/files"

// MaxSecretBytes is the bound on the data of the files Secret of one
// Sandbox: the keys and the values, with the manifest as the operator writes
// it. A Secret holds at most 1 MiB. The number of files has no bound of its
// own.
const MaxSecretBytes = 512 << 10

// MountDir is where the launcher container mounts the files Secret.
const MountDir = "/etc/setec/files"

// ManifestKey is the key in the files Secret of the manifest.
const ManifestKey = "manifest.json"

// SecretKey is the key in the files Secret of the content of the file at
// index i of spec.files.
func SecretKey(i int) string { return "f" + strconv.Itoa(i) }

// sourcePattern is the form of the source of a manifest entry.
var sourcePattern = regexp.MustCompile(`^` + regexp.QuoteMeta(MountDir) + `/f[0-9]+$`)

// ValidSource reports whether src is the path of a content key of the files
// Secret, the only form that the operator writes.
func ValidSource(src string) bool { return sourcePattern.MatchString(src) }

// Entry is one entry of the manifest: the path in the machine, the file in
// the mounted Secret that holds the content, and the mode.
type Entry struct {
	Path   string `json:"path"`
	Source string `json:"source"`
	Mode   uint32 `json:"mode"`
}

// SecretData returns the data of the files Secret of files, exactly as the
// operator writes it: the manifest and the content of each file. The
// webhook and the frontend measure this value, so the bound cannot differ
// from the write.
func SecretData(files []setecv1alpha1.SandboxFile) (map[string][]byte, error) {
	list := make([]Entry, 0, len(files))
	data := make(map[string][]byte, len(files)+1)
	for i, f := range files {
		list = append(list, Entry{Path: MachinePath(f.Path), Source: MountDir + "/" + SecretKey(i), Mode: ModeOf(f)})
		data[SecretKey(i)] = f.Content
	}
	manifest, err := json.Marshal(list)
	if err != nil {
		return nil, fmt.Errorf("marshal the files manifest: %w", err)
	}
	data[ManifestKey] = manifest
	return data, nil
}

// SecretSize is the size of the data of a Secret: each key and each value.
func SecretSize(data map[string][]byte) int {
	n := 0
	for k, v := range data {
		n += len(k) + len(v)
	}
	return n
}

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
// grants a write bit, a setuid bit or a setgid bit, or a files Secret
// above MaxSecretBytes. The Secret is measured as the operator writes it.
func Validate(files []setecv1alpha1.SandboxFile) error {
	seen := make(map[string]struct{}, len(files))
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
	}
	for p := range seen {
		for dir := path.Dir(p); dir != "."; dir = path.Dir(dir) {
			if _, clash := seen[dir]; clash {
				return fmt.Errorf("files: the path %q is a file and the directory of %q", dir, p)
			}
		}
	}
	data, err := SecretData(files)
	if err != nil {
		return err
	}
	if n := SecretSize(data); n > MaxSecretBytes {
		return fmt.Errorf("files: the Secret of the files would hold %d bytes, above %d", n, MaxSecretBytes)
	}
	return nil
}
