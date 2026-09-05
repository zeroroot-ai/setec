// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package installer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// Runner executes a command on the host and returns its combined output.
// name is resolved against the HOST's filesystem (see lookPathIn), never
// the installer container's own image — the distroless installer image
// carries no shell and no host tools by design.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

// hostPATH is the search path used to resolve host binaries. It mirrors a
// root login shell's default so systemctl/dmsetup/losetup resolve on any
// mainstream distro regardless of the host's own PATH configuration.
var hostPATH = []string{
	"/usr/local/sbin", "/usr/local/bin",
	"/usr/sbin", "/usr/bin",
	"/sbin", "/bin",
}

// lookPathIn resolves name to an absolute path INSIDE root (i.e. the
// returned path does not include the root prefix — it is valid after
// chrooting into root). Returns an error when no executable candidate
// exists.
func lookPathIn(root, name string) (string, error) {
	if filepath.IsAbs(name) {
		if isExecutable(filepath.Join(root, name)) {
			return name, nil
		}
		return "", fmt.Errorf("%s: not found under %s", name, root)
	}
	for _, dir := range hostPATH {
		candidate := filepath.Join(dir, name)
		if isExecutable(filepath.Join(root, candidate)) {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("%s: not found in %v under %s", name, hostPATH, root)
}

func isExecutable(path string) bool {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return false
	}
	return info.Mode()&0o111 != 0
}
