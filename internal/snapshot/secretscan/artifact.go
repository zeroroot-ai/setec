// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package secretscan

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

// ErrSecretsFound is returned by ScanPath / ScanArtifact when the scanner
// detects secret-shaped material. Callers (the snapshot builder, the CI
// gate) treat this as fatal: a snapshot that carries secrets must never be
// persisted or shipped.
var ErrSecretsFound = errors.New("secretscan: secret-shaped material found in snapshot artifact")

// PathFinding pairs a Finding with the artifact path it was found in.
type PathFinding struct {
	Path string
	Finding
}

// ScanArtifact scans a single named snapshot artifact (a regular file). It
// returns the findings and, when any are present, ErrSecretsFound so callers
// can fail closed with a single errors.Is check.
func ScanArtifact(path string) ([]PathFinding, error) {
	f, err := os.Open(path) //nolint:gosec // path is operator/CI supplied, not attacker input
	if err != nil {
		return nil, fmt.Errorf("secretscan: open %q: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	findings, err := New().Scan(f)
	if err != nil {
		return nil, err
	}
	if len(findings) == 0 {
		return nil, nil
	}
	out := make([]PathFinding, 0, len(findings))
	for _, fnd := range findings {
		out = append(out, PathFinding{Path: path, Finding: fnd})
	}
	return out, ErrSecretsFound
}

// ScanArtifactSHA256 scans a single artifact like ScanArtifact and
// additionally returns the lowercase-hex SHA-256 of the bytes EXACTLY
// as scanned, computed in the same pass. Callers that record a scan
// verdict (the pool bake path, ADR-0005 invariant 1) use the digest to
// bind the verdict to the artifact: a later consumer re-deriving the
// digest proves the scanned bytes are the restored bytes.
func ScanArtifactSHA256(path string) ([]PathFinding, string, error) {
	f, err := os.Open(path) //nolint:gosec // path is builder/CI supplied, not attacker input
	if err != nil {
		return nil, "", fmt.Errorf("secretscan: open %q: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	h := sha256.New()
	findings, err := New().Scan(io.TeeReader(f, h))
	if err != nil {
		return nil, "", err
	}
	digest := hex.EncodeToString(h.Sum(nil))
	if len(findings) == 0 {
		return nil, digest, nil
	}
	out := make([]PathFinding, 0, len(findings))
	for _, fnd := range findings {
		out = append(out, PathFinding{Path: path, Finding: fnd})
	}
	return out, digest, ErrSecretsFound
}

// ScanPath scans a file or, recursively, every regular file under a
// directory. It aggregates findings across all artifacts. The returned error
// is ErrSecretsFound when any artifact contained secret-shaped material, or a
// wrapped I/O error on a hard failure. Findings are always returned even
// alongside ErrSecretsFound so callers can print every leak.
func ScanPath(root string) ([]PathFinding, error) {
	info, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("secretscan: stat %q: %w", root, err)
	}
	if !info.IsDir() {
		return ScanArtifact(root)
	}

	var all []PathFinding
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		found, scanErr := ScanArtifact(path)
		if scanErr != nil && !errors.Is(scanErr, ErrSecretsFound) {
			return scanErr
		}
		all = append(all, found...)
		return nil
	})
	if walkErr != nil {
		return all, fmt.Errorf("secretscan: walk %q: %w", root, walkErr)
	}

	sort.SliceStable(all, func(i, j int) bool {
		if all[i].Path != all[j].Path {
			return all[i].Path < all[j].Path
		}
		return all[i].Offset < all[j].Offset
	})
	if len(all) > 0 {
		return all, ErrSecretsFound
	}
	return nil, nil
}
