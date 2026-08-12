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

// Package poolentry defines the on-disk artifact layout of a pre-warm
// pool entry — the file names, the sealed-key sidecar, and the
// template-provenance record — shared by the writer (cmd/setec-pool-vm)
// and the consumer (internal/nodeagent/pool).
//
// The package exists so ADR-0005 invariants 1, 4 and 5 are enforced
// at the artifact layer with one definition:
//
//   - Invariant 1 (clean base): every pool entry carries the
//     secret-scan verdict recorded at bake time — scanner version,
//     clean result, and the SHA-256 digests of the exact plaintext
//     state/memory pair that was scanned. The pool Manager refuses to
//     hand out an entry whose verdict is missing or not clean, and
//     the claim path re-checks the digests against the decrypted
//     artifacts so "this exact artifact was scanned clean" holds
//     per restore.
//   - Invariant 4 (template provenance): every pool entry carries a
//     provenance record asserting it was built from the class-image
//     boot path. The pool Manager refuses to hand out an entry whose
//     record is missing or claims any other source.
//   - Invariant 5 (at rest): the entry's state/memory files are
//     encrypted with a per-entry DEK sealed under the node KEK; the
//     seal's AAD binds the DEK to the entry's identity, its
//     provenance AND its scan verdict, so re-keying a foreign
//     artifact as a pool entry — or swapping either record after the
//     fact — is not possible without the plaintext DEK.
package poolentry

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

const (
	// StateFile and MemFile are the encrypted Firecracker snapshot
	// pair inside an entry directory.
	StateFile = "state.bin"
	MemFile   = "memory.bin"

	// DEKFile is the sealed per-entry data-encryption key. Destroying
	// it cryptographically erases the entry's state/memory files.
	DEKFile = "entry.dek"

	// ProvenanceFile records how the entry was built.
	ProvenanceFile = "provenance.json"

	// ScanFile records the secret-scan verdict for the entry's
	// plaintext state/memory pair, produced at bake time (ADR-0005
	// invariant 1).
	ScanFile = "scan.json"

	// SourceClassImageBoot is the ONLY provenance source a pool entry
	// may carry: a cold boot from the class's kernel/rootfs/image
	// (ADR-0005 invariant 4). There is deliberately no constant for
	// any other source — a used/dirty sandbox is never a template.
	SourceClassImageBoot = "class-image-boot"
)

// ErrProvenance is returned by Verify when an entry's provenance
// record is missing, unreadable, or claims any source other than the
// class-image boot path.
var ErrProvenance = errors.New("poolentry: template provenance violation (ADR-0005 invariant 4)")

// ErrScanVerdict is returned by VerifyScan when an entry's secret-scan
// verdict is missing, unreadable, incomplete, or not clean. Absence of
// the verdict is a violation: an unscanned entry is never handed out
// (fail closed).
var ErrScanVerdict = errors.New("poolentry: clean-base scan verdict violation (ADR-0005 invariant 1)")

// Provenance describes how a pool entry was built.
type Provenance struct {
	// Source must be SourceClassImageBoot.
	Source string `json:"source"`
	// ImageRef is the class image the entry was booted for.
	ImageRef string `json:"imageRef"`
	// KernelPath and RootfsPath are the boot inputs, recorded for
	// audit.
	KernelPath string `json:"kernelPath"`
	RootfsPath string `json:"rootfsPath"`
}

// ScanVerdict records the secret-scan outcome for an entry's plaintext
// state/memory pair, produced by the bake path immediately before the
// pair is encrypted at rest. It is the per-entry evidence for ADR-0005
// invariant 1 (clean base), independent of the provenance record
// (invariant 4).
type ScanVerdict struct {
	// ScannerVersion identifies the exact detector set that produced
	// the verdict (secretscan.Version()).
	ScannerVersion string `json:"scannerVersion"`
	// Clean is true when the scan reported zero findings. A dirty
	// pair is never persisted — the bake fails instead — so a
	// persisted record with Clean=false is always refused.
	Clean bool `json:"clean"`
	// StateSHA256 and MemorySHA256 are the lowercase-hex SHA-256
	// digests of the plaintext state/memory files EXACTLY as scanned.
	// The claim path re-derives them from the decrypted artifacts so
	// the verdict attests this restore's bytes, not merely this entry.
	StateSHA256  string `json:"stateSHA256"`
	MemorySHA256 string `json:"memorySHA256"`
}

// DEKAAD renders the associated data that binds an entry's sealed DEK
// to its identity, its provenance, and its secret-scan verdict.
// Changing any of them after the fact makes the sealed DEK unopenable.
func DEKAAD(entryID string, p Provenance, v ScanVerdict) string {
	return "setec-pool-entry:" + entryID + ":" + p.Source + ":" + p.ImageRef +
		":scan:" + v.ScannerVersion + ":clean=" + strconv.FormatBool(v.Clean) +
		":" + v.StateSHA256 + ":" + v.MemorySHA256
}

// WriteProvenance persists the record into dir (0600).
func WriteProvenance(dir string, p Provenance) error {
	b, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("poolentry: marshal provenance: %w", err)
	}
	path := filepath.Join(dir, ProvenanceFile)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return fmt.Errorf("poolentry: write %q: %w", path, err)
	}
	return nil
}

// ReadProvenance loads the record from dir.
func ReadProvenance(dir string) (Provenance, error) {
	var p Provenance
	b, err := os.ReadFile(filepath.Join(dir, ProvenanceFile))
	if err != nil {
		return p, err
	}
	if err := json.Unmarshal(b, &p); err != nil {
		return p, fmt.Errorf("poolentry: unmarshal provenance: %w", err)
	}
	return p, nil
}

// WriteScan persists the secret-scan verdict into dir (0600).
func WriteScan(dir string, v ScanVerdict) error {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("poolentry: marshal scan verdict: %w", err)
	}
	path := filepath.Join(dir, ScanFile)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return fmt.Errorf("poolentry: write %q: %w", path, err)
	}
	return nil
}

// ReadScan loads the secret-scan verdict from dir.
func ReadScan(dir string) (ScanVerdict, error) {
	var v ScanVerdict
	b, err := os.ReadFile(filepath.Join(dir, ScanFile))
	if err != nil {
		return v, err
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return v, fmt.Errorf("poolentry: unmarshal scan verdict: %w", err)
	}
	return v, nil
}

// VerifyScan enforces invariant 1's evidence on an entry directory:
// the scan verdict must exist, must be clean, and must be complete
// (scanner version + both artifact digests). Any deviation returns
// ErrScanVerdict (wrapped with detail). Digest agreement with the
// decrypted artifacts is the claim path's job — it is the only place
// the plaintext exists.
func VerifyScan(dir string) (ScanVerdict, error) {
	v, err := ReadScan(dir)
	if err != nil {
		return ScanVerdict{}, fmt.Errorf("%w: read record: %v", ErrScanVerdict, err)
	}
	if !v.Clean {
		return ScanVerdict{}, fmt.Errorf("%w: recorded verdict is not clean", ErrScanVerdict)
	}
	if v.ScannerVersion == "" || v.StateSHA256 == "" || v.MemorySHA256 == "" {
		return ScanVerdict{}, fmt.Errorf("%w: incomplete record (scanner version or artifact digests missing)", ErrScanVerdict)
	}
	return v, nil
}

// Verify enforces invariant 4 on an entry directory: the provenance
// record must exist, must claim the class-image boot path, and must
// match the image the entry is being claimed for. Any deviation
// returns ErrProvenance (wrapped with detail).
func Verify(dir, wantImageRef string) error {
	p, err := ReadProvenance(dir)
	if err != nil {
		return fmt.Errorf("%w: read record: %v", ErrProvenance, err)
	}
	if p.Source != SourceClassImageBoot {
		return fmt.Errorf("%w: source %q, want %q", ErrProvenance, p.Source, SourceClassImageBoot)
	}
	if wantImageRef != "" && p.ImageRef != wantImageRef {
		return fmt.Errorf("%w: image %q, want %q", ErrProvenance, p.ImageRef, wantImageRef)
	}
	return nil
}
