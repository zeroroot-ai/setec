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

package secretscan

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestScanArtifactSHA256_CleanReturnsDigest: a clean artifact yields
// no findings and the exact SHA-256 of its bytes, computed in the same
// pass — the digest a scan verdict records (ADR-0005 invariant 1).
func TestScanArtifactSHA256_CleanReturnsDigest(t *testing.T) {
	body := []byte("plain guest state with nothing secret-shaped in it")
	path := filepath.Join(t.TempDir(), "state.bin")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}

	findings, digest, err := ScanArtifactSHA256(path)
	if err != nil {
		t.Fatalf("ScanArtifactSHA256: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("unexpected findings: %v", findings)
	}
	want := sha256.Sum256(body)
	if digest != hex.EncodeToString(want[:]) {
		t.Fatalf("digest = %s, want the SHA-256 of the scanned bytes", digest)
	}
}

// TestScanArtifactSHA256_DirtyStillReturnsDigest: findings surface as
// ErrSecretsFound, and the digest is still reported so callers can
// name the offending artifact precisely.
func TestScanArtifactSHA256_DirtyStillReturnsDigest(t *testing.T) {
	body := []byte("leak: -----BEGIN RSA PRIVATE KEY-----\n")
	path := filepath.Join(t.TempDir(), "memory.bin")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}

	findings, digest, err := ScanArtifactSHA256(path)
	if !errors.Is(err, ErrSecretsFound) {
		t.Fatalf("err = %v, want ErrSecretsFound", err)
	}
	if len(findings) == 0 {
		t.Fatal("expected findings alongside ErrSecretsFound")
	}
	want := sha256.Sum256(body)
	if digest != hex.EncodeToString(want[:]) {
		t.Fatalf("digest = %s, want the SHA-256 of the scanned bytes", digest)
	}
}
