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

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zeroroot-ai/setec/internal/nodeagent/poolentry"
	"github.com/zeroroot-ai/setec/internal/snapshot/atrest"
	"github.com/zeroroot-ai/setec/internal/snapshot/secretscan"
)

// secretSnapshotWriter emulates Firecracker writing a guest image that
// contains a recognisable sensitive pattern.
var secretMarker = bytes.Repeat([]byte("GUEST-MEMORY-SECRET-PATTERN-"), 64)

func secretSnapshotWriter(state, mem string) error {
	if err := os.WriteFile(state, secretMarker, 0o644); err != nil {
		return err
	}
	return os.WriteFile(mem, secretMarker, 0o644)
}

// TestRunLauncher_EncryptsEntryAtRest is the pool half of ADR-0005
// invariant 5: after a successful launch, the entry's state/memory
// files on disk are ciphertext (no plaintext marker, correct framing),
// the sealed per-entry DEK exists, the provenance record claims the
// class-image boot path, and the DEK decrypts the files back to the
// original guest image.
func TestRunLauncher_EncryptsEntryAtRest(t *testing.T) {
	opts := tempOpts(t)
	spawner := newSpawnerWithSocket(opts)
	defer spawner.closeListener()
	fc := &fakeFC{snapshotWriter: secretSnapshotWriter}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := runLauncher(ctx, opts, spawner, factoryReturning(fc)); err != nil {
		t.Fatalf("runLauncher: %v", err)
	}

	entryDir := filepath.Join(opts.StorageRoot, opts.PoolEntryID)
	for _, name := range []string{stateFileName, memFileName} {
		raw, err := os.ReadFile(filepath.Join(entryDir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if bytes.Contains(raw, secretMarker[:28]) {
			t.Fatalf("%s contains plaintext guest memory at rest", name)
		}
	}

	// Sealed DEK + provenance record + scan verdict present.
	sealed, err := os.ReadFile(filepath.Join(entryDir, poolentry.DEKFile))
	if err != nil {
		t.Fatalf("sealed DEK missing: %v", err)
	}
	prov, err := poolentry.ReadProvenance(entryDir)
	if err != nil {
		t.Fatalf("provenance missing: %v", err)
	}
	if prov.Source != poolentry.SourceClassImageBoot || prov.ImageRef != opts.ImageRef {
		t.Fatalf("unexpected provenance: %+v", prov)
	}
	verdict, err := poolentry.VerifyScan(entryDir)
	if err != nil {
		t.Fatalf("scan verdict missing or not clean: %v", err)
	}
	if verdict.ScannerVersion != secretscan.Version() {
		t.Fatalf("verdict scanner version = %q, want %q", verdict.ScannerVersion, secretscan.Version())
	}
	wantDigest := sha256.Sum256(secretMarker)
	if verdict.StateSHA256 != hex.EncodeToString(wantDigest[:]) || verdict.MemorySHA256 != hex.EncodeToString(wantDigest[:]) {
		t.Fatalf("verdict digests %q/%q do not match the scanned plaintext pair", verdict.StateSHA256, verdict.MemorySHA256)
	}

	// The node KEK unseals the DEK under the identity+provenance+scan
	// AAD, and the DEK recovers the original guest image.
	kek, err := atrest.LoadOrCreateKEK(opts.KeyFile)
	if err != nil {
		t.Fatalf("load KEK: %v", err)
	}
	dek, err := atrest.OpenDEK(kek, sealed, poolentry.DEKAAD(opts.PoolEntryID, prov, verdict))
	if err != nil {
		t.Fatalf("unseal DEK: %v", err)
	}
	out := filepath.Join(t.TempDir(), "plain")
	digest, err := atrest.DecryptFile(filepath.Join(entryDir, stateFileName), out, dek)
	if err != nil {
		t.Fatalf("decrypt state: %v", err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, secretMarker) {
		t.Fatal("decrypted state does not match original guest image")
	}
	if digest != verdict.StateSHA256 {
		t.Fatalf("decrypted state digest %q does not match the recorded verdict %q", digest, verdict.StateSHA256)
	}

	// A forged provenance record must not unseal the DEK (the AAD
	// binds source+image into the key wrap).
	forged := prov
	forged.Source = "used-sandbox"
	if _, err := atrest.OpenDEK(kek, sealed, poolentry.DEKAAD(opts.PoolEntryID, forged, verdict)); err == nil {
		t.Fatal("sealed DEK must be bound to its provenance record")
	}

	// A forged scan verdict must not unseal the DEK either (ADR-0005
	// invariant 1, setec#206): the verdict is AAD-bound exactly like
	// the provenance record.
	forgedScan := verdict
	forgedScan.StateSHA256 = strings.Repeat("0", 64)
	if _, err := atrest.OpenDEK(kek, sealed, poolentry.DEKAAD(opts.PoolEntryID, prov, forgedScan)); err == nil {
		t.Fatal("sealed DEK must be bound to its scan verdict")
	}
}

// TestRunLauncher_RefusesSecretInSnapshot is the bake half of ADR-0005
// invariant 1 (setec#206): a guest image that contains secret-shaped
// material must never be persisted as a pool entry — the launch fails
// and every artifact (including any scan record) is removed.
func TestRunLauncher_RefusesSecretInSnapshot(t *testing.T) {
	opts := tempOpts(t)
	spawner := newSpawnerWithSocket(opts)
	defer spawner.closeListener()
	leaky := append([]byte("config dump: aws_secret= "), []byte("AKIAABCDEFGHIJKLMNOP is the access key id\n")...)
	fc := &fakeFC{snapshotWriter: func(state, mem string) error {
		if err := os.WriteFile(state, secretMarker, 0o644); err != nil {
			return err
		}
		return os.WriteFile(mem, leaky, 0o644)
	}}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := runLauncher(ctx, opts, spawner, factoryReturning(fc))
	if !errors.Is(err, secretscan.ErrSecretsFound) {
		t.Fatalf("runLauncher error = %v, want secretscan.ErrSecretsFound", err)
	}
	// The redacted finding report must never re-leak the secret body.
	if err != nil && strings.Contains(err.Error(), "AKIAABCDEFGHIJKLMNOP") {
		t.Fatal("error message re-leaks the detected secret")
	}
	entryDir := filepath.Join(opts.StorageRoot, opts.PoolEntryID)
	if _, statErr := os.Stat(entryDir); !os.IsNotExist(statErr) {
		t.Fatalf("entry dir %q must be removed after a dirty scan (stat err: %v)", entryDir, statErr)
	}
}

// TestRunLauncher_RefusesLiveSocket is ADR-0005 invariant 4 at the
// builder: a live listener on the requested Firecracker socket means a
// pre-existing (possibly used) VM — the launcher must refuse to adopt
// and snapshot it.
func TestRunLauncher_RefusesLiveSocket(t *testing.T) {
	opts := tempOpts(t)
	ln, err := net.Listen("unix", opts.SocketPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		for {
			conn, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			_ = conn.Close()
		}
	}()

	spawner := &fakeSpawner{socketPath: opts.SocketPath}
	fc := &fakeFC{}
	err = runLauncher(context.Background(), opts, spawner, factoryReturning(fc))
	if err == nil {
		t.Fatal("expected refusal on live pre-existing socket")
	}
	if spawner.startCalled.Load() != 0 {
		t.Fatal("launcher must refuse BEFORE spawning firecracker")
	}
	if fc.snapshotCalled.Load() != 0 {
		t.Fatal("launcher must never snapshot a pre-existing VM")
	}
}

// TestParseFlags_KeyFileRequired: encryption at rest has no opt-out; an
// explicitly emptied --key-file is a refused misconfiguration.
func TestParseFlags_KeyFileRequired(t *testing.T) {
	_, err := parseFlags([]string{
		"--kernel-path", "/k",
		"--rootfs-path", "/r",
		"--socket-path", "/s",
		"--storage-root", "/sr",
		"--pool-entry-id", "e1",
		"--key-file", "",
	})
	if err == nil {
		t.Fatal("empty --key-file must be rejected")
	}
}
