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
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zeroroot-ai/setec/internal/nodeagent/poolentry"
	"github.com/zeroroot-ai/setec/internal/snapshot/atrest"
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

	// Sealed DEK + provenance record present.
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

	// The node KEK unseals the DEK under the identity+provenance AAD,
	// and the DEK recovers the original guest image.
	kek, err := atrest.LoadOrCreateKEK(opts.KeyFile)
	if err != nil {
		t.Fatalf("load KEK: %v", err)
	}
	dek, err := atrest.OpenDEK(kek, sealed, poolentry.DEKAAD(opts.PoolEntryID, prov))
	if err != nil {
		t.Fatalf("unseal DEK: %v", err)
	}
	out := filepath.Join(t.TempDir(), "plain")
	if err := atrest.DecryptFile(filepath.Join(entryDir, stateFileName), out, dek); err != nil {
		t.Fatalf("decrypt state: %v", err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, secretMarker) {
		t.Fatal("decrypted state does not match original guest image")
	}

	// A forged provenance record must not unseal the DEK (the AAD
	// binds source+image into the key wrap).
	forged := prov
	forged.Source = "used-sandbox"
	if _, err := atrest.OpenDEK(kek, sealed, poolentry.DEKAAD(opts.PoolEntryID, forged)); err == nil {
		t.Fatal("sealed DEK must be bound to its provenance record")
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
