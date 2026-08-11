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
	"github.com/zeroroot-ai/setec/internal/snapshot/storage"
)

// seedPool returns a Manager holding exactly one entry for class
// "std" / image "img:v1", with raw state/memory files written under
// the entry directory the way setec-pool-vm does.
func seedPool(t *testing.T, writeStateFiles bool) *pool.Manager {
	t.Helper()
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
	if writeStateFiles {
		for _, e := range pm.QueryAvailable("std", "") {
			if err := os.MkdirAll(e.StorageRef, 0o700); err != nil {
				t.Fatalf("mkdir entry dir: %v", err)
			}
			if err := os.WriteFile(filepath.Join(e.StorageRef, "state.bin"), []byte("STATE"), 0o600); err != nil {
				t.Fatalf("write state: %v", err)
			}
			if err := os.WriteFile(filepath.Join(e.StorageRef, "memory.bin"), []byte("MEMORY"), 0o600); err != nil {
				t.Fatalf("write memory: %v", err)
			}
		}
	}
	return pm
}

func TestClaimPoolEntry_RestoresAndConsumes(t *testing.T) {
	pm := seedPool(t, true)
	entries := pm.QueryAvailable("std", "")
	if len(entries) != 1 {
		t.Fatalf("seeded entries = %d, want 1", len(entries))
	}
	entryDir := entries[0].StorageRef

	fc := &fakeFirecracker{}
	srv := newServer(t, fc, pm)
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
	pm := seedPool(t, true)
	srv := newServer(t, &fakeFirecracker{}, pm)
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
	pm := seedPool(t, true)
	fc := &fakeFirecracker{loadErr: errors.New("socket refused")}
	srv := newServer(t, fc, pm)
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
	pm := seedPool(t, false) // no state.bin/memory.bin on disk
	srv := newServer(t, &fakeFirecracker{}, pm)
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
