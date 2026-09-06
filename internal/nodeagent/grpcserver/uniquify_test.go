// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package grpcserver

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	setecgrpcv1 "github.com/zeroroot-ai/setec/api/grpc/v1"
	"github.com/zeroroot-ai/setec/internal/uniquify"
)

// recordingUniquifier implements uniquify.Uniquifier for tests.
type recordingUniquifier struct {
	mu    sync.Mutex
	specs []uniquify.Spec
	cid   uint32
	err   error
}

// Repeated failure reason under test.
const (
	reasonRestoreFailed = "restore_failed"
)

func (r *recordingUniquifier) Uniquify(_ context.Context, _ string, spec uniquify.Spec) (uniquify.Report, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.specs = append(r.specs, spec)
	if r.err != nil {
		return uniquify.Report{}, r.err
	}
	return uniquify.Report{
		Status:      uniquify.StatusOK,
		MachineID:   spec.MachineID,
		BootID:      spec.BootID,
		Hostname:    spec.Hostname,
		ObservedIPs: []string{spec.PodIP},
		GuestCID:    r.cid,
	}, nil
}

func (r *recordingUniquifier) seen() []uniquify.Spec {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]uniquify.Spec(nil), r.specs...)
}

func TestRestoreSandbox_UniquifySuccessIsReported(t *testing.T) {
	fc := &fakeFirecracker{}
	srv := newServer(t, fc, nil)
	uq := &recordingUniquifier{cid: 33}
	var outcomes []string
	srv.Uniquifier = uq
	srv.CIDs = uniquify.NewCIDAllocator()
	srv.UniquifyObserver = func(outcome string) { outcomes = append(outcomes, outcome) }
	cli := newBufconnClient(t, srv)
	ctx := context.Background()

	framed := makeFramedPayload(t, []byte("S"), []byte("M"))
	if _, _, err := srv.Storage.Save(ctx, "snap-u", bytes.NewReader(framed)); err != nil {
		t.Fatalf("Save: %v", err)
	}
	resp, err := cli.RestoreSandbox(ctx, &setecgrpcv1.RestoreSandboxRequest{
		SnapshotId:       "snap-u",
		StorageRef:       "snap-u",
		KataSocketTarget: "/run/kata-containers/pod-u/firecracker.socket",
		SandboxId:        "ns/sb-u",
		PodIp:            "10.2.3.4",
		Hostname:         "sb-u",
	})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if !resp.Success || !resp.Uniquified {
		t.Fatalf("expected success + uniquified, got %+v", resp)
	}
	specs := uq.seen()
	if len(specs) != 1 {
		t.Fatalf("uniquifier invocations = %d, want 1", len(specs))
	}
	if specs[0].PodIP != "10.2.3.4" || specs[0].Hostname != "sb-u" {
		t.Fatalf("spec did not carry the request identity: %+v", specs[0])
	}
	if specs[0].MachineID == "" || specs[0].BootID == "" {
		t.Fatalf("spec must mint fresh machine-id/boot-id: %+v", specs[0])
	}
	if owner, held := srv.CIDs.Owner(33); !held || owner != "ns/sb-u" {
		t.Fatalf("CID 33 not registered to the sandbox (owner=%q held=%v)", owner, held)
	}
	if len(outcomes) != 1 || outcomes[0] != "success" {
		t.Fatalf("observer outcomes = %v", outcomes)
	}
}

// TestRestoreSandbox_UniquifyFailureFailsClosed is the core ADR-0005
// invariant-2 contract of setec#189: when the guest cannot confirm
// its fresh identity, the restore RPC must fail (so the sandbox is
// never reported Ready) and the VM must be paused, not handed over.
func TestRestoreSandbox_UniquifyFailureFailsClosed(t *testing.T) {
	fc := &fakeFirecracker{}
	srv := newServer(t, fc, nil)
	uq := &recordingUniquifier{err: errors.New("guest never reported")}
	var outcomes []string
	srv.Uniquifier = uq
	srv.UniquifyObserver = func(outcome string) { outcomes = append(outcomes, outcome) }
	cli := newBufconnClient(t, srv)
	ctx := context.Background()

	framed := makeFramedPayload(t, []byte("S"), []byte("M"))
	if _, _, err := srv.Storage.Save(ctx, "snap-uf", bytes.NewReader(framed)); err != nil {
		t.Fatalf("Save: %v", err)
	}
	_, err := cli.RestoreSandbox(ctx, &setecgrpcv1.RestoreSandboxRequest{
		SnapshotId:       "snap-uf",
		StorageRef:       "snap-uf",
		KataSocketTarget: "/run/kata-containers/pod-uf/firecracker.socket",
		SandboxId:        "ns/sb-uf",
	})
	if err == nil {
		t.Fatal("restore must FAIL when the uniquification cannot be confirmed")
	}
	if s, _ := status.FromError(err); s.Code() != codes.Internal {
		t.Fatalf("code = %v, want Internal", s.Code())
	}
	fc.mu.Lock()
	pauses := fc.pauseCalls
	fc.mu.Unlock()
	if pauses == 0 {
		t.Fatal("the un-uniquified VM must be paused (not handed over with a cloned identity)")
	}
	if len(outcomes) != 1 || outcomes[0] != "failure" {
		t.Fatalf("observer outcomes = %v", outcomes)
	}
}

// TestRestoreSandbox_CIDCollisionFailsClosed pins the node-local CID
// uniqueness gate: a second restore whose guest reports a CID already
// held by another live sandbox must be destroyed, not handed over.
func TestRestoreSandbox_CIDCollisionFailsClosed(t *testing.T) {
	fc := &fakeFirecracker{}
	srv := newServer(t, fc, nil)
	// Both restores report the same CID — exactly what happens when
	// one Snapshot is restored twice (the CID is part of the
	// snapshotted VM config).
	srv.Uniquifier = &recordingUniquifier{cid: 77}
	srv.CIDs = uniquify.NewCIDAllocator()
	cli := newBufconnClient(t, srv)
	ctx := context.Background()

	for i, snap := range []string{"snap-c1", "snap-c2"} {
		framed := makeFramedPayload(t, []byte("S"), []byte("M"))
		if _, _, err := srv.Storage.Save(ctx, snap, bytes.NewReader(framed)); err != nil {
			t.Fatalf("Save: %v", err)
		}
		_, err := cli.RestoreSandbox(ctx, &setecgrpcv1.RestoreSandboxRequest{
			SnapshotId:       snap,
			StorageRef:       snap,
			KataSocketTarget: "/run/kata-containers/pod-c/firecracker.socket",
			SandboxId:        []string{"ns/sb-c1", "ns/sb-c2"}[i],
		})
		if i == 0 && err != nil {
			t.Fatalf("first restore must succeed: %v", err)
		}
		if i == 1 && err == nil {
			t.Fatal("second restore sharing the CID must FAIL closed")
		}
	}
}

func TestClaimPoolEntry_UniquifyFailureConsumesEntryAndFallsBack(t *testing.T) {
	pm, kekPath := seedPool(t, true)
	fc := &fakeFirecracker{}
	srv := newServer(t, fc, pm)
	srv.PoolKEKPath = kekPath
	srv.Uniquifier = &recordingUniquifier{err: errors.New("guest never reported")}
	var claims []string
	srv.ClaimObserver = func(outcome string) { claims = append(claims, outcome) }
	cli := newBufconnClient(t, srv)

	resp, err := cli.ClaimPoolEntry(context.Background(), &setecgrpcv1.ClaimPoolEntryRequest{
		SandboxClass:     "std",
		ImageRef:         "img:v1",
		KataSocketTarget: filepath.Join(t.TempDir(), "firecracker.socket"),
		SandboxId:        "t-a/sb",
		PodIp:            "10.5.6.7",
		Hostname:         "sb",
	})
	if err != nil {
		t.Fatalf("ClaimPoolEntry: %v", err)
	}
	// Cold-boot fallback, not an RPC error: claimed but unsuccessful.
	if !resp.GetClaimed() || resp.GetSuccess() {
		t.Fatalf("claimed=%v success=%v, want claimed+!success", resp.GetClaimed(), resp.GetSuccess())
	}
	if resp.GetError() == "" {
		t.Fatal("the fallback must carry the uniquification failure detail")
	}
	// The entry is consumed regardless (ADR-0005 single-restore).
	if n := pm.CountClass("std"); n != 0 {
		t.Fatalf("pool still holds %d entries, want 0", n)
	}
	// The failed VM was paused.
	fc.mu.Lock()
	pauses := fc.pauseCalls
	fc.mu.Unlock()
	if pauses == 0 {
		t.Fatal("the un-uniquified VM must be paused")
	}
	if len(claims) != 1 || claims[0] != reasonRestoreFailed {
		t.Fatalf("claim outcomes = %v, want [restore_failed]", claims)
	}
}

func TestClaimPoolEntry_UniquifySuccessReportsAndAdoptsCID(t *testing.T) {
	pm, kekPath := seedPool(t, true)
	cids := uniquify.NewCIDAllocator()
	pm.CIDs = cids
	fc := &fakeFirecracker{}
	srv := newServer(t, fc, pm)
	srv.PoolKEKPath = kekPath
	srv.CIDs = cids
	// seedPool boots without a CID allocator wired into the launcher
	// path, so entries carry GuestCID 0 and the pin is skipped; the
	// reported CID is still registered to the sandbox.
	srv.Uniquifier = &recordingUniquifier{cid: 55}
	cli := newBufconnClient(t, srv)

	resp, err := cli.ClaimPoolEntry(context.Background(), &setecgrpcv1.ClaimPoolEntryRequest{
		SandboxClass:     "std",
		ImageRef:         "img:v1",
		KataSocketTarget: filepath.Join(t.TempDir(), "firecracker.socket"),
		SandboxId:        "t-a/sb-ok",
		PodIp:            "10.5.6.8",
		Hostname:         "sb-ok",
	})
	if err != nil {
		t.Fatalf("ClaimPoolEntry: %v", err)
	}
	if !resp.GetClaimed() || !resp.GetSuccess() || !resp.GetUniquified() {
		t.Fatalf("claimed=%v success=%v uniquified=%v error=%q",
			resp.GetClaimed(), resp.GetSuccess(), resp.GetUniquified(), resp.GetError())
	}
	if owner, held := cids.Owner(55); !held || owner != "t-a/sb-ok" {
		t.Fatalf("CID 55 not adopted by the sandbox (owner=%q held=%v)", owner, held)
	}
}

// TestUniquifyRestored_PinsPoolEntryCID: when the pool entry was
// booted with a known CID, a guest reporting a different one is a
// broken restore and must fail closed.
func TestUniquifyRestored_PinsPoolEntryCID(t *testing.T) {
	srv := &Server{Uniquifier: &recordingUniquifier{cid: 44}}
	err := srv.uniquifyRestored(context.Background(), []string{"/tmp/vsock.sock"}, "ns/sb", "sb", "", 43)
	if err == nil {
		t.Fatal("a CID mismatch against the pool entry's boot CID must fail closed")
	}
	if err := srv.uniquifyRestored(context.Background(), []string{"/tmp/vsock.sock"}, "ns/sb", "sb", "", 44); err != nil {
		t.Fatalf("matching CID must pass: %v", err)
	}
}
