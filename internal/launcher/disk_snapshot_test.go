// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package launcher

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zeroroot-ai/setec/internal/diskbuilder"
)

// rotationDisk writes an image disk with content, signs it with key for
// imageRef, and returns the disk path and the signature path.
func rotationDisk(t *testing.T, key ed25519.PrivateKey, imageRef, content string) (disk, sig string) {
	t.Helper()
	dir := t.TempDir()
	disk = filepath.Join(dir, diskbuilder.DiskFile)
	if err := os.WriteFile(disk, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	d, err := diskbuilder.ParseDigestRef(imageRef)
	if err != nil {
		t.Fatal(err)
	}
	s, err := diskbuilder.Sign(key, d.DigestStr(), disk)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	sig = filepath.Join(dir, diskbuilder.SignatureFile)
	if err := os.WriteFile(sig, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return disk, sig
}

// TestRun_RestoreAfterARotationLoadsOnlyOverTheSameDisk is setec#277. A
// snapshot ran on the disk that the previous key signed. After a rotation
// the Pod gets the disk that the new key signed. Both disks pass the
// signature check, because both keys are trusted during a rotation. The
// restore loads only when the new disk has the same bytes; a disk with
// other bytes (a changed builder, or changed content) is refused before the
// load, and the machine never reaches the network.
func TestRun_RestoreAfterARotationLoadsOnlyOverTheSameDisk(t *testing.T) {
	t.Parallel()
	const imageRef = "registry.test/app@sha256:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	oldPub, oldKey, _ := ed25519.GenerateKey(nil)
	newPub, newKey, _ := ed25519.GenerateKey(nil)
	keys := []ed25519.PublicKey{oldPub, newPub}

	// The disk of the snapshot, signed by the previous key.
	oldDisk, _ := rotationDisk(t, oldKey, imageRef, "squashfs of the image")
	snapDisk, err := diskbuilder.FileSHA256(oldDisk)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name    string
		content string
		loads   bool
	}{
		{"the new key signed the same bytes", "squashfs of the image", true},
		{"the new key signed other bytes", "squashfs of the image, tampered", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			disk, sig := rotationDisk(t, newKey, imageRef, tc.content)
			s := testSpec(t)
			s.ImageDisk, s.DiskSignature, s.ImageRef = disk, sig, imageRef
			dir := t.TempDir()
			s.Source = Source{Snapshot: &SnapshotSource{
				State: filepath.Join(dir, "state.bin"), Memory: filepath.Join(dir, "memory.bin"),
				Evidence: filepath.Join(dir, "evidence.json"), ImageDisk: filepath.Join(dir, "image-disk.sha256"),
			}}
			if err := os.WriteFile(s.Source.Snapshot.ImageDisk, []byte(snapDisk+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			nw := &fakeNet{}
			vmm := &fakeVMM{report: 0}
			l := &Launcher{Spec: s, Net: nw, VMM: vmm, Console: io.Discard, Grace: time.Second, Format: noFormat,
				CheckDisk: func(s *Spec) (string, error) {
					return diskbuilder.VerifyMounted(s.ImageDisk, s.DiskSignature, s.ImageRef, keys)
				}}
			code, err := l.Run(t.Context())
			if tc.loads {
				if err != nil || len(vmm.snapshot) != 2 {
					t.Fatalf("Run = %d, %v, load = %v; want a load over the same disk", code, err, vmm.snapshot)
				}
				return
			}
			var le *Error
			if code != LaunchFailedExit || !errors.As(err, &le) || le.Reason != ReasonSourceFailed || !errors.Is(err, errSnapshotDisk) {
				t.Fatalf("Run = %d, %v; want a refusal with reason %s", code, err, ReasonSourceFailed)
			}
			if vmm.snapshot != nil || nw.connected != 0 {
				t.Fatalf("load = %v, connected = %d; want no load and no network", vmm.snapshot, nw.connected)
			}
			var ev RestoreEvidence
			raw, _ := os.ReadFile(s.Source.Snapshot.Evidence)
			if json.Unmarshal(raw, &ev) != nil || !strings.Contains(ev.Error, "not the disk of the snapshot") {
				t.Fatalf("evidence = %s; want the refusal for the node agent", raw)
			}
		})
	}
}

// TestRun_RestoreFailsClosedWithNoDiskDigest proves that a snapshot load
// needs both digests: a staged digest that is missing or not a SHA-256,
// and a launcher that checked no disk, each refuse the load (setec#277).
func TestRun_RestoreFailsClosedWithNoDiskDigest(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		staged string // "" leaves the file out
		check  bool
	}{
		{"no staged digest", "", true},
		{"a staged digest that is not a SHA-256", "not-a-digest", true},
		{"no disk check", testDiskSHA, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := testSpec(t)
			dir := t.TempDir()
			s.Source = Source{Snapshot: &SnapshotSource{
				State: filepath.Join(dir, "state.bin"), Memory: filepath.Join(dir, "memory.bin"),
				ImageDisk: filepath.Join(dir, "image-disk.sha256"),
			}}
			if tc.staged != "" {
				if err := os.WriteFile(s.Source.Snapshot.ImageDisk, []byte(tc.staged), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			vmm := &fakeVMM{report: 0}
			l := &Launcher{Spec: s, Net: &fakeNet{}, VMM: vmm, Console: io.Discard, Grace: time.Second, Format: noFormat}
			if tc.check {
				l.CheckDisk = func(*Spec) (string, error) { return testDiskSHA, nil }
			}
			code, err := l.Run(t.Context())
			var le *Error
			if code != LaunchFailedExit || !errors.As(err, &le) || le.Reason != ReasonSourceFailed || vmm.snapshot != nil {
				t.Fatalf("Run = %d, %v, load = %v; want a refusal before the load", code, err, vmm.snapshot)
			}
		})
	}
}

// TestRun_RecordsTheCheckedDisk proves that the launcher writes the digest
// of the checked disk into its work directory, where the node agent reads
// it for each snapshot of the machine (setec#277).
func TestRun_RecordsTheCheckedDisk(t *testing.T) {
	t.Parallel()
	s := testSpec(t)
	l := &Launcher{Spec: s, Net: &fakeNet{}, VMM: &fakeVMM{report: 0}, Console: io.Discard, Grace: time.Second,
		Format: noFormat, CheckDisk: func(*Spec) (string, error) { return testDiskSHA, nil }}
	if _, err := l.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(s.WorkDir, ImageDiskDigest))
	if err != nil || strings.TrimSpace(string(raw)) != testDiskSHA {
		t.Fatalf("recorded digest = %q, %v; want %s", raw, err, testDiskSHA)
	}
	l = &Launcher{Spec: testSpec(t), Net: &fakeNet{}, VMM: &fakeVMM{}, Console: io.Discard, Grace: time.Second,
		Format: noFormat, CheckDisk: func(*Spec) (string, error) { return "short", nil }}
	if code, err := l.Run(t.Context()); code != LaunchFailedExit || err == nil {
		t.Fatalf("a disk check with no SHA-256 = %d, %v; want a refusal", code, err)
	}
}
