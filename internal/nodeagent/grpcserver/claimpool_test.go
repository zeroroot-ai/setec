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

package grpcserver

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	setecgrpcv1 "github.com/zeroroot-ai/setec/api/grpc/v1"
	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
	"github.com/zeroroot-ai/setec/internal/firecracker"
	"github.com/zeroroot-ai/setec/internal/nodeagent/pool"
	"github.com/zeroroot-ai/setec/internal/nodeagent/poolentry"
	"github.com/zeroroot-ai/setec/internal/snapshot/atrest"
	"github.com/zeroroot-ai/setec/internal/snapshot/storage"
)

// seedPool returns a Manager holding exactly one entry for class
// "std" / image "img:v1", with the full artifact layer written under
// the entry directory the way setec-pool-vm does: encrypted
// state/memory (when writeStateFiles), a sealed per-entry DEK, and a
// class-image-boot provenance record. The returned kekPath must be
// wired into the Server (PoolKEKPath) so ClaimPoolEntry can unseal.
func seedPool(t *testing.T, writeStateFiles bool) (*pool.Manager, string) {
	t.Helper()
	kekPath := filepath.Join(t.TempDir(), "node.key")
	pm := pool.New(
		&storage.LocalDiskBackend{Root: t.TempDir()},
		noPrefetch{},
		func(_ string) firecracker.Client { return &fakeFirecracker{} },
		"node-x",
	)
	pm.PoolStorageRoot = t.TempDir()
	pm.SocketPattern = filepath.Join(t.TempDir(), "pool-%s", "firecracker.socket")
	pm.Launcher = noopLauncher{}
	cls := setecv1alpha1.SandboxClass{
		ObjectMeta: metav1.ObjectMeta{Name: "std"},
		Spec: setecv1alpha1.SandboxClassSpec{
			VMM: setecv1alpha1.VMMFirecracker, PreWarmPoolSize: 1, PreWarmImage: "img:v1",
		},
	}
	if err := pm.ReconcilePools(context.Background(), []setecv1alpha1.SandboxClass{cls}); err != nil {
		t.Fatalf("seed pool: %v", err)
	}
	kek, err := atrest.LoadOrCreateKEK(kekPath)
	if err != nil {
		t.Fatalf("seed KEK: %v", err)
	}
	for _, e := range pm.QueryAvailable("std", "") {
		if err := os.MkdirAll(e.StorageRef, 0o700); err != nil {
			t.Fatalf("mkdir entry dir: %v", err)
		}
		dek, err := atrest.NewDEK()
		if err != nil {
			t.Fatalf("seed DEK: %v", err)
		}
		if writeStateFiles {
			for name, payload := range map[string]string{
				poolentry.StateFile: "STATE",
				poolentry.MemFile:   "MEMORY",
			} {
				p := filepath.Join(e.StorageRef, name)
				if err := os.WriteFile(p, []byte(payload), 0o600); err != nil {
					t.Fatalf("write %s: %v", name, err)
				}
				if err := atrest.EncryptFile(p, dek); err != nil {
					t.Fatalf("encrypt %s: %v", name, err)
				}
			}
		}
		prov := poolentry.Provenance{Source: poolentry.SourceClassImageBoot, ImageRef: "img:v1"}
		sealed, err := atrest.SealDEK(kek, dek, poolentry.DEKAAD(e.ID, prov))
		if err != nil {
			t.Fatalf("seal DEK: %v", err)
		}
		if err := os.WriteFile(filepath.Join(e.StorageRef, poolentry.DEKFile), sealed, 0o600); err != nil {
			t.Fatalf("write sealed DEK: %v", err)
		}
		if err := poolentry.WriteProvenance(e.StorageRef, prov); err != nil {
			t.Fatalf("write provenance: %v", err)
		}
	}
	return pm, kekPath
}

func TestClaimPoolEntry_RestoresAndConsumes(t *testing.T) {
	pm, kekPath := seedPool(t, true)
	entries := pm.QueryAvailable("std", "")
	if len(entries) != 1 {
		t.Fatalf("seeded entries = %d, want 1", len(entries))
	}
	entryDir := entries[0].StorageRef

	fc := &fakeFirecracker{}
	srv := newServer(t, fc, pm)
	srv.PoolKEKPath = kekPath
	var outcomes []string
	srv.ClaimObserver = func(outcome string) { outcomes = append(outcomes, outcome) }
	cli := newBufconnClient(t, srv)

	resp, err := cli.ClaimPoolEntry(context.Background(), &setecgrpcv1.ClaimPoolEntryRequest{
		SandboxClass:     "std",
		ImageRef:         "img:v1",
		KataSocketTarget: filepath.Join(t.TempDir(), "firecracker.socket"),
		SandboxId:        "t-a/sb",
	})
	if err != nil {
		t.Fatalf("ClaimPoolEntry: %v", err)
	}
	if !resp.GetClaimed() || !resp.GetSuccess() {
		t.Fatalf("claimed=%v success=%v error=%q, want claimed+success", resp.GetClaimed(), resp.GetSuccess(), resp.GetError())
	}
	if resp.GetEntryId() == "" {
		t.Fatal("entry_id must identify the consumed entry")
	}
	// ADR-0005 attestations for the operator-side invariant gate: the
	// claim path verified the template provenance (AAD-bound into the
	// sealed DEK) and decrypted always-encrypted state, so both signals
	// must be reported affirmatively.
	if !resp.GetProvenanceVerified() {
		t.Fatal("provenance_verified must be true on a successful claim")
	}
	if !resp.GetEncryptedAtRest() {
		t.Fatal("encrypted_at_rest must be true on a successful claim")
	}

	// The restore drove LoadSnapshot with the entry's raw state files.
	if len(fc.loadCalls) != 1 || !strings.Contains(fc.loadCalls[0], "state.bin") {
		t.Fatalf("loadCalls = %v, want one call with the entry's state.bin", fc.loadCalls)
	}

	// The entry is consumed (ADR-0005: one restore per snapshot state)
	// and its on-disk state is gone.
	if n := pm.CountClass("std"); n != 0 {
		t.Fatalf("pool still holds %d entries after claim, want 0", n)
	}
	if _, statErr := os.Stat(entryDir); !os.IsNotExist(statErr) {
		t.Fatalf("entry dir %q still present after claim (stat err: %v)", entryDir, statErr)
	}

	if len(outcomes) != 1 || outcomes[0] != "restored" {
		t.Fatalf("claim outcomes = %v, want [restored]", outcomes)
	}
}

func TestClaimPoolEntry_MissIsNotAnError(t *testing.T) {
	pm, kekPath := seedPool(t, true)
	srv := newServer(t, &fakeFirecracker{}, pm)
	srv.PoolKEKPath = kekPath
	var outcomes []string
	srv.ClaimObserver = func(outcome string) { outcomes = append(outcomes, outcome) }
	cli := newBufconnClient(t, srv)

	// Wrong image: the pool holds img:v1 only.
	resp, err := cli.ClaimPoolEntry(context.Background(), &setecgrpcv1.ClaimPoolEntryRequest{
		SandboxClass:     "std",
		ImageRef:         "other:v2",
		KataSocketTarget: "/tmp/x.socket",
	})
	if err != nil {
		t.Fatalf("ClaimPoolEntry: %v", err)
	}
	if resp.GetClaimed() {
		t.Fatal("claimed=true for a non-matching image, want miss")
	}
	// The non-matching entry stays pooled.
	if n := pm.CountClass("std"); n != 1 {
		t.Fatalf("pool entries = %d after miss, want 1", n)
	}
	if len(outcomes) != 1 || outcomes[0] != "miss" {
		t.Fatalf("claim outcomes = %v, want [miss]", outcomes)
	}
}

func TestClaimPoolEntry_NilPoolMisses(t *testing.T) {
	cli := newBufconnClient(t, newServer(t, &fakeFirecracker{}, nil))
	resp, err := cli.ClaimPoolEntry(context.Background(), &setecgrpcv1.ClaimPoolEntryRequest{
		SandboxClass:     "std",
		KataSocketTarget: "/tmp/x.socket",
	})
	if err != nil {
		t.Fatalf("ClaimPoolEntry: %v", err)
	}
	if resp.GetClaimed() {
		t.Fatal("claimed=true with no pool wired")
	}
}

func TestClaimPoolEntry_LoadFailureConsumesEntry(t *testing.T) {
	pm, kekPath := seedPool(t, true)
	fc := &fakeFirecracker{loadErr: errors.New("socket refused")}
	srv := newServer(t, fc, pm)
	srv.PoolKEKPath = kekPath
	var outcomes []string
	srv.ClaimObserver = func(outcome string) { outcomes = append(outcomes, outcome) }
	cli := newBufconnClient(t, srv)

	resp, err := cli.ClaimPoolEntry(context.Background(), &setecgrpcv1.ClaimPoolEntryRequest{
		SandboxClass:     "std",
		KataSocketTarget: "/tmp/x.socket",
	})
	if err != nil {
		t.Fatalf("ClaimPoolEntry must not fail the RPC on a restore error: %v", err)
	}
	if !resp.GetClaimed() || resp.GetSuccess() {
		t.Fatalf("claimed=%v success=%v, want claimed=true success=false", resp.GetClaimed(), resp.GetSuccess())
	}
	if resp.GetError() == "" {
		t.Fatal("error message must explain the failed restore")
	}
	// The failed entry must NOT return to the pool (ADR-0005).
	if n := pm.CountClass("std"); n != 0 {
		t.Fatalf("pool entries = %d after failed restore, want 0", n)
	}
	if len(outcomes) != 1 || outcomes[0] != "restore_failed" {
		t.Fatalf("claim outcomes = %v, want [restore_failed]", outcomes)
	}
}

func TestClaimPoolEntry_MissingStateFilesConsumesEntry(t *testing.T) {
	pm, kekPath := seedPool(t, false) // no state.bin/memory.bin on disk
	srv := newServer(t, &fakeFirecracker{}, pm)
	srv.PoolKEKPath = kekPath
	cli := newBufconnClient(t, srv)

	resp, err := cli.ClaimPoolEntry(context.Background(), &setecgrpcv1.ClaimPoolEntryRequest{
		SandboxClass:     "std",
		KataSocketTarget: "/tmp/x.socket",
	})
	if err != nil {
		t.Fatalf("ClaimPoolEntry: %v", err)
	}
	if !resp.GetClaimed() || resp.GetSuccess() {
		t.Fatalf("claimed=%v success=%v, want claimed=true success=false for missing state", resp.GetClaimed(), resp.GetSuccess())
	}
	if n := pm.CountClass("std"); n != 0 {
		t.Fatalf("pool entries = %d, want 0 after consuming the broken entry", n)
	}
}

func TestClaimPoolEntry_MissingArgs(t *testing.T) {
	cli := newBufconnClient(t, newServer(t, &fakeFirecracker{}, nil))

	for name, req := range map[string]*setecgrpcv1.ClaimPoolEntryRequest{
		"no class":  {KataSocketTarget: "/tmp/x.socket"},
		"no socket": {SandboxClass: "std"},
	} {
		_, err := cli.ClaimPoolEntry(context.Background(), req)
		if status.Code(err) != codes.InvalidArgument {
			t.Fatalf("%s: code = %v, want InvalidArgument", name, status.Code(err))
		}
	}
}
