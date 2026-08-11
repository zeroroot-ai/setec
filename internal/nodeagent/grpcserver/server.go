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

// Package grpcserver implements the NodeAgentService gRPC surface
// the operator dials into. Each RPC composes three cooperating
// internals: the Firecracker client for per-VM API calls, the
// storage backend for snapshot persistence, and the pool manager for
// pre-warm queries. Every RPC is self-contained; there is no
// long-lived state beyond the injected dependencies.
package grpcserver

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	setecgrpcv1 "github.com/zeroroot-ai/setec/api/grpc/v1"
	"github.com/zeroroot-ai/setec/internal/entropy"
	"github.com/zeroroot-ai/setec/internal/firecracker"
	"github.com/zeroroot-ai/setec/internal/nodeagent/pool"
	"github.com/zeroroot-ai/setec/internal/nodeagent/poolentry"
	"github.com/zeroroot-ai/setec/internal/snapshot/atrest"
	"github.com/zeroroot-ai/setec/internal/snapshot/storage"
)

// Server implements NodeAgentServiceServer.
type Server struct {
	setecgrpcv1.UnimplementedNodeAgentServiceServer

	// Storage is the backend all snapshot state is persisted to.
	Storage storage.StorageBackend

	// FirecrackerFactory constructs a Firecracker client for a given
	// API socket path. Tests inject a mock here; production wires
	// firecracker.NewClientFromSocket.
	FirecrackerFactory func(sockPath string) firecracker.Client

	// Pool is the pre-warm pool manager. When nil, QueryPool returns
	// an empty list (no pool feature).
	Pool *pool.Manager

	// TempDir is the directory temp state files are written to during
	// CreateSnapshot/RestoreSandbox. Defaults to /var/lib/setec/tmp.
	TempDir string

	// Reseeder actively reseeds the restored guest's CSPRNG over the
	// Firecracker vsock UDS after every successful LoadSnapshot
	// (setec#72). When non-nil the restore FAILS CLOSED: the RPC only
	// reports success once the in-guest setec-guest-agent has
	// acknowledged fresh entropy, and an unconfirmed VM is paused
	// rather than handed over with cloned RNG state. nil disables the
	// active reseed (explicit --entropy-reseed=off opt-out), leaving
	// only the passive virtio-rng mechanism.
	Reseeder entropy.Reseeder

	// ReseedVsockPaths returns candidate host paths for the restored
	// VM's vsock Unix socket. nil uses defaultReseedVsockPaths.
	ReseedVsockPaths func(in *setecgrpcv1.RestoreSandboxRequest) []string

	// ReseedObserver, when non-nil, receives "success" or "failure"
	// after each reseed attempt (metrics hook).
	ReseedObserver func(outcome string)

	// ClaimObserver, when non-nil, receives the outcome of every
	// ClaimPoolEntry call: "restored", "miss", or "restore_failed"
	// (metrics hook — setec_prewarm_pool_claims_total).
	ClaimObserver func(outcome string)

	// PoolKEKPath is the node-local key-encryption-key file pool
	// entries' per-entry DEKs are sealed with (the same keyfile the
	// EncryptedBackend uses). ClaimPoolEntry needs it to decrypt an
	// entry's state/memory pair before LoadSnapshot — pool state is
	// always encrypted at rest (ADR-0005 invariant 5).
	PoolKEKPath string

	// Tracer is optional.
	Tracer trace.Tracer
}

// tempDir returns the configured TempDir, falling back to the
// default.
func (s *Server) tempDir() string {
	if s.TempDir != "" {
		return s.TempDir
	}
	return "/var/lib/setec/tmp"
}

func (s *Server) tracer() trace.Tracer {
	if s.Tracer != nil {
		return s.Tracer
	}
	return tracenoop.NewTracerProvider().Tracer("setec.nodeagent.grpc")
}

// frameHeaderSize is the size of the leading 16-byte framing header
// written to storage by CreateSnapshot: [stateSize uint64][memSize
// uint64]. The framing keeps the two Firecracker output files paired
// under a single opaque storageRef without inventing a richer
// wrapper format.
const frameHeaderSize = 16

// CreateSnapshot pauses the target VM, asks Firecracker to write
// state+memory files to tempdir, concatenates them with a framing
// header, streams the concat into Storage.Save, and returns the
// resulting storage ref.
func (s *Server) CreateSnapshot(ctx context.Context, in *setecgrpcv1.CreateSnapshotRequest) (*setecgrpcv1.CreateSnapshotResponse, error) {
	ctx, span := s.tracer().Start(ctx, "nodeagent.CreateSnapshot")
	defer span.End()
	span.SetAttributes(
		attribute.String("setec.sandbox_id", in.GetSandboxId()),
		attribute.String("setec.snapshot_id", in.GetSnapshotId()),
	)

	if in.GetSnapshotId() == "" {
		return nil, status.Error(codes.InvalidArgument, "snapshot_id required")
	}
	if in.GetSourceKataSocket() == "" {
		return nil, status.Error(codes.InvalidArgument, "source_kata_socket required")
	}

	fc := s.FirecrackerFactory(in.GetSourceKataSocket())

	if err := fc.Pause(ctx); err != nil {
		return nil, status.Errorf(codes.Internal, "firecracker pause: %v", err)
	}

	dir := filepath.Join(s.tempDir(), in.GetSnapshotId())
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, status.Errorf(codes.Internal, "mkdir temp: %v", err)
	}
	statePath := filepath.Join(dir, "state.bin")
	memPath := filepath.Join(dir, "memory.bin")

	// Ensure we clean up the temp files even on error paths. The temp
	// pair is the PLAINTEXT guest image (the durable copy written by
	// Storage.Save is encrypted), so it gets the same zero-overwrite
	// treatment the storage backend applies before unlinking.
	defer func() { shredDir(dir) }()

	if err := fc.CreateSnapshot(ctx, statePath, memPath); err != nil {
		return nil, status.Errorf(codes.Internal, "firecracker createSnapshot: %v", err)
	}

	// Resume the source VM now that the state+memory pair is on
	// disk. A resume failure is reported but does not prevent
	// Storage.Save (the persisted snapshot is still valid).
	_ = fc.Resume(ctx)

	combined, err := makeFramedReader(statePath, memPath)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "assemble framed stream: %v", err)
	}
	defer func() { _ = combined.Close() }()

	size, ref, saveErr := s.Storage.Save(ctx, in.GetSnapshotId(), combined)
	if saveErr != nil {
		if errors.Is(saveErr, storage.ErrInsufficientStorage) {
			return nil, status.Errorf(codes.ResourceExhausted, "storage: %v", saveErr)
		}
		return nil, status.Errorf(codes.Internal, "storage: %v", saveErr)
	}

	return &setecgrpcv1.CreateSnapshotResponse{
		StorageRef: ref,
		SizeBytes:  size,
		Sha256:     "", // Local-disk backend writes sidecar; operator re-reads if needed.
	}, nil
}

// RestoreSandbox reads the framed payload from storage, writes the
// two temp files, and asks Firecracker to LoadSnapshot.
func (s *Server) RestoreSandbox(ctx context.Context, in *setecgrpcv1.RestoreSandboxRequest) (*setecgrpcv1.RestoreSandboxResponse, error) {
	ctx, span := s.tracer().Start(ctx, "nodeagent.RestoreSandbox")
	defer span.End()
	span.SetAttributes(attribute.String("setec.snapshot_id", in.GetSnapshotId()))

	if in.GetStorageRef() == "" {
		return nil, status.Error(codes.InvalidArgument, "storage_ref required")
	}
	if in.GetKataSocketTarget() == "" {
		return nil, status.Error(codes.InvalidArgument, "kata_socket_target required")
	}

	rc, err := s.Storage.Open(ctx, in.GetStorageRef())
	if err != nil {
		if errors.Is(err, storage.ErrCorrupted) {
			return nil, status.Errorf(codes.DataLoss, "corrupted snapshot: %v", err)
		}
		if errors.Is(err, storage.ErrNotFound) {
			return nil, status.Errorf(codes.NotFound, "snapshot not found: %v", err)
		}
		return nil, status.Errorf(codes.Internal, "open snapshot: %v", err)
	}
	defer func() { _ = rc.Close() }()

	dir := filepath.Join(s.tempDir(), in.GetSnapshotId()+"-restore-"+fmt.Sprintf("%d", time.Now().UnixNano()))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, status.Errorf(codes.Internal, "mkdir: %v", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	statePath := filepath.Join(dir, "state.bin")
	memPath := filepath.Join(dir, "memory.bin")

	if err := writeFramedStream(rc, statePath, memPath); err != nil {
		return nil, status.Errorf(codes.Internal, "unpack framed stream: %v", err)
	}

	fc := s.FirecrackerFactory(in.GetKataSocketTarget())
	if err := fc.LoadSnapshot(ctx, statePath, memPath); err != nil {
		return &setecgrpcv1.RestoreSandboxResponse{
			Success: false,
			Error:   err.Error(),
		}, status.Errorf(codes.Internal, "firecracker loadSnapshot: %v", err)
	}

	// Active entropy reseed (setec#72). The snapshot's CSPRNG state is
	// shared by every clone restored from it; before the restore is
	// reported usable, push fresh entropy into the guest and require
	// the in-guest agent's digest-verified ack. Fail closed: an
	// unconfirmed reseed pauses the VM and fails the RPC so the
	// Sandbox is never marked Ready.
	if s.Reseeder != nil {
		candidates := defaultReseedVsockPaths(in)
		if s.ReseedVsockPaths != nil {
			candidates = s.ReseedVsockPaths(in)
		}
		if err := entropy.ReseedFirst(ctx, s.Reseeder, candidates); err != nil {
			s.observeReseed("failure")
			msg := fmt.Sprintf("entropy reseed after restore failed (failing closed): %v", err)
			if pauseErr := fc.Pause(ctx); pauseErr != nil {
				msg += fmt.Sprintf("; additionally failed to pause the unreseeded VM: %v", pauseErr)
			}
			return &setecgrpcv1.RestoreSandboxResponse{
				Success: false,
				Error:   msg,
			}, status.Error(codes.Internal, msg)
		}
		s.observeReseed("success")
		return &setecgrpcv1.RestoreSandboxResponse{Success: true, EntropyReseeded: true}, nil
	}

	return &setecgrpcv1.RestoreSandboxResponse{Success: true}, nil
}

// observeReseed invokes the optional metrics hook.
func (s *Server) observeReseed(outcome string) {
	if s.ReseedObserver != nil {
		s.ReseedObserver(outcome)
	}
}

// defaultReseedVsockPaths derives the candidate vsock UDS paths for a
// restored VM:
//
//   - <storageRef>/vsock.sock — pool entries persist their state under
//     an absolute on-node directory, and setec-pool-vm binds the vsock
//     device there (vsockUDSPath); non-absolute (opaque backend) refs
//     contribute nothing.
//   - <dir(kataSocketTarget)>/vsock.sock — the sibling of the target
//     Firecracker API socket, for restores into Kata-managed pods.
//
// Candidate probing is not a fail-open: whichever path connects must
// still complete the digest-verified reseed, and if none does the
// restore fails closed.
func defaultReseedVsockPaths(in *setecgrpcv1.RestoreSandboxRequest) []string {
	var out []string
	if ref := in.GetStorageRef(); ref != "" && filepath.IsAbs(ref) {
		out = append(out, filepath.Join(ref, "vsock.sock"))
	}
	if ks := in.GetKataSocketTarget(); ks != "" {
		out = append(out, filepath.Join(filepath.Dir(ks), "vsock.sock"))
	}
	return out
}

// PauseSandbox is a direct wrap of firecracker.Pause.
func (s *Server) PauseSandbox(ctx context.Context, in *setecgrpcv1.PauseSandboxRequest) (*setecgrpcv1.PauseSandboxResponse, error) {
	ctx, span := s.tracer().Start(ctx, "nodeagent.PauseSandbox")
	defer span.End()
	span.SetAttributes(attribute.String("setec.sandbox_id", in.GetSandboxId()))

	if in.GetKataSocketTarget() == "" {
		return nil, status.Error(codes.InvalidArgument, "kata_socket_target required")
	}
	fc := s.FirecrackerFactory(in.GetKataSocketTarget())
	if err := fc.Pause(ctx); err != nil {
		return &setecgrpcv1.PauseSandboxResponse{
			Success: false,
			Error:   err.Error(),
		}, status.Errorf(codes.Internal, "firecracker pause: %v", err)
	}
	return &setecgrpcv1.PauseSandboxResponse{Success: true}, nil
}

// ResumeSandbox is a direct wrap of firecracker.Resume.
func (s *Server) ResumeSandbox(ctx context.Context, in *setecgrpcv1.ResumeSandboxRequest) (*setecgrpcv1.ResumeSandboxResponse, error) {
	ctx, span := s.tracer().Start(ctx, "nodeagent.ResumeSandbox")
	defer span.End()
	span.SetAttributes(attribute.String("setec.sandbox_id", in.GetSandboxId()))

	if in.GetKataSocketTarget() == "" {
		return nil, status.Error(codes.InvalidArgument, "kata_socket_target required")
	}
	fc := s.FirecrackerFactory(in.GetKataSocketTarget())
	if err := fc.Resume(ctx); err != nil {
		return &setecgrpcv1.ResumeSandboxResponse{
			Success: false,
			Error:   err.Error(),
		}, status.Errorf(codes.Internal, "firecracker resume: %v", err)
	}
	return &setecgrpcv1.ResumeSandboxResponse{Success: true}, nil
}

// DeleteSnapshot invokes Storage.Delete so the state files are
// securely erased.
func (s *Server) DeleteSnapshot(ctx context.Context, in *setecgrpcv1.DeleteSnapshotRequest) (*setecgrpcv1.DeleteSnapshotResponse, error) {
	ctx, span := s.tracer().Start(ctx, "nodeagent.DeleteSnapshot")
	defer span.End()
	span.SetAttributes(attribute.String("setec.storage_ref", in.GetStorageRef()))
	if in.GetStorageRef() == "" {
		return nil, status.Error(codes.InvalidArgument, "storage_ref required")
	}
	if err := s.Storage.Delete(ctx, in.GetStorageRef()); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			// Idempotent: treat missing state as success so repeated
			// reconciles don't churn.
			return &setecgrpcv1.DeleteSnapshotResponse{Success: true}, nil
		}
		return &setecgrpcv1.DeleteSnapshotResponse{
			Success: false,
			Error:   err.Error(),
		}, status.Errorf(codes.Internal, "storage delete: %v", err)
	}
	return &setecgrpcv1.DeleteSnapshotResponse{Success: true}, nil
}

// QueryPool delegates to Pool.QueryAvailable.
func (s *Server) QueryPool(ctx context.Context, in *setecgrpcv1.QueryPoolRequest) (*setecgrpcv1.QueryPoolResponse, error) {
	_, span := s.tracer().Start(ctx, "nodeagent.QueryPool")
	defer span.End()
	span.SetAttributes(attribute.String("setec.class", in.GetSandboxClass()))

	if s.Pool == nil {
		return &setecgrpcv1.QueryPoolResponse{}, nil
	}
	entries := s.Pool.QueryAvailable(in.GetSandboxClass(), in.GetImageRef())
	now := time.Now()
	resp := &setecgrpcv1.QueryPoolResponse{}
	for _, e := range entries {
		resp.Entries = append(resp.Entries, &setecgrpcv1.PoolEntry{
			EntryId:    e.ID,
			ImageRef:   e.ImageRef,
			Available:  true,
			AgeSeconds: int64(now.Sub(e.PausedAt).Seconds()),
		})
	}
	return resp, nil
}

// ClaimPoolEntry atomically claims a pre-warmed pool entry for the
// requested class/image and restores its paused-VM state into the
// caller-provided Kata Firecracker socket (ADR-0004). Pool entries
// persist raw state.bin/memory.bin files under their entry directory
// (written by setec-pool-vm) — not the framed stream the Storage
// backend uses — so the restore reads them directly.
//
// The claimed entry is consumed no matter how the restore ends:
// ADR-0005 forbids restoring the same snapshot state twice, so a
// failed restore releases the entry rather than returning it to the
// pool. Both "no entry" and "restore failed" are reported as
// non-error responses; the operator's fallback to cold boot is the
// expected path, not an exception.
func (s *Server) ClaimPoolEntry(ctx context.Context, in *setecgrpcv1.ClaimPoolEntryRequest) (*setecgrpcv1.ClaimPoolEntryResponse, error) {
	ctx, span := s.tracer().Start(ctx, "nodeagent.ClaimPoolEntry")
	defer span.End()
	span.SetAttributes(
		attribute.String("setec.class", in.GetSandboxClass()),
		attribute.String("setec.sandbox_id", in.GetSandboxId()),
	)

	if in.GetSandboxClass() == "" {
		return nil, status.Error(codes.InvalidArgument, "sandbox_class required")
	}
	if in.GetKataSocketTarget() == "" {
		return nil, status.Error(codes.InvalidArgument, "kata_socket_target required")
	}
	if s.Pool == nil {
		s.observeClaim("miss")
		return &setecgrpcv1.ClaimPoolEntryResponse{Claimed: false}, nil
	}

	entry, ok, err := s.Pool.Claim(ctx, in.GetSandboxClass(), in.GetImageRef())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "pool claim: %v", err)
	}
	if !ok {
		s.observeClaim("miss")
		return &setecgrpcv1.ClaimPoolEntryResponse{Claimed: false}, nil
	}

	// The entry is consumed from here on: erase its on-disk state when
	// we return, success or not (ADR-0005 single-restore invariant).
	// Claim already detached it from pool state, so ReleaseClaimed —
	// not Release — is the teardown.
	defer func() { _ = s.Pool.ReleaseClaimed(ctx, entry) }()

	// Pool entry state is encrypted at rest (ADR-0005 invariant 5):
	// unseal the per-entry DEK and decrypt into a private temp dir for
	// LoadSnapshot. Plain RemoveAll on the temp pair — Firecracker may
	// keep the restored memory file mapped, so it must be unlinked,
	// never overwritten (same rationale as RestoreSandbox).
	statePath, memPath, cleanup, decErr := s.decryptPoolEntry(entry.StorageRef, entry.ID)
	if decErr != nil {
		s.observeClaim("restore_failed")
		return &setecgrpcv1.ClaimPoolEntryResponse{
			Claimed: true,
			EntryId: entry.ID,
			Error:   fmt.Sprintf("decrypt pool entry state: %v", decErr),
		}, nil
	}
	defer cleanup()

	fc := s.FirecrackerFactory(in.GetKataSocketTarget())
	if err := fc.LoadSnapshot(ctx, statePath, memPath); err != nil {
		s.observeClaim("restore_failed")
		return &setecgrpcv1.ClaimPoolEntryResponse{
			Claimed: true,
			EntryId: entry.ID,
			Error:   fmt.Sprintf("firecracker loadSnapshot: %v", err),
		}, nil
	}

	// Active entropy reseed (setec#72), identical fail-closed contract
	// to RestoreSandbox: the pool entry's CSPRNG state is shared with
	// the paused template VM, so the restored clone must confirm fresh
	// entropy before it is handed over. On failure the VM is paused
	// and the operator falls back to cold boot.
	if s.Reseeder != nil {
		candidates := []string{
			filepath.Join(entry.StorageRef, "vsock.sock"),
			filepath.Join(filepath.Dir(in.GetKataSocketTarget()), "vsock.sock"),
		}
		if err := entropy.ReseedFirst(ctx, s.Reseeder, candidates); err != nil {
			s.observeReseed("failure")
			s.observeClaim("restore_failed")
			msg := fmt.Sprintf("entropy reseed after pool restore failed (failing closed): %v", err)
			if pauseErr := fc.Pause(ctx); pauseErr != nil {
				msg += fmt.Sprintf("; additionally failed to pause the unreseeded VM: %v", pauseErr)
			}
			return &setecgrpcv1.ClaimPoolEntryResponse{
				Claimed: true,
				EntryId: entry.ID,
				Error:   msg,
			}, nil
		}
		s.observeReseed("success")
		s.observeClaim("restored")
		return &setecgrpcv1.ClaimPoolEntryResponse{
			Claimed:         true,
			Success:         true,
			EntryId:         entry.ID,
			EntropyReseeded: true,
		}, nil
	}

	s.observeClaim("restored")
	return &setecgrpcv1.ClaimPoolEntryResponse{
		Claimed: true,
		Success: true,
		EntryId: entry.ID,
	}, nil
}

// observeClaim invokes the optional pool-claim metrics hook.
func (s *Server) observeClaim(outcome string) {
	if s.ClaimObserver != nil {
		s.ClaimObserver(outcome)
	}
}

// decryptPoolEntry unseals a claimed entry's DEK (bound to the entry's
// identity + provenance record) and streams the encrypted state/memory
// pair into a fresh temp dir as the plaintext files Firecracker's
// LoadSnapshot needs. The returned cleanup unlinks the temp tree.
func (s *Server) decryptPoolEntry(entryDir, entryID string) (statePath, memPath string, cleanup func(), err error) {
	kek, err := atrest.LoadOrCreateKEK(s.poolKEKPath())
	if err != nil {
		return "", "", nil, err
	}
	sealed, err := os.ReadFile(filepath.Join(entryDir, poolentry.DEKFile))
	if err != nil {
		return "", "", nil, fmt.Errorf("read sealed DEK: %w", err)
	}
	prov, err := poolentry.ReadProvenance(entryDir)
	if err != nil {
		return "", "", nil, fmt.Errorf("read provenance: %w", err)
	}
	dek, err := atrest.OpenDEK(kek, sealed, poolentry.DEKAAD(entryID, prov))
	if err != nil {
		return "", "", nil, err
	}

	dir := filepath.Join(s.tempDir(), "pool-claim-"+entryID+"-"+fmt.Sprintf("%d", time.Now().UnixNano()))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", nil, err
	}
	cleanup = func() { _ = os.RemoveAll(dir) }
	statePath = filepath.Join(dir, poolentry.StateFile)
	memPath = filepath.Join(dir, poolentry.MemFile)
	if err := atrest.DecryptFile(filepath.Join(entryDir, poolentry.StateFile), statePath, dek); err != nil {
		cleanup()
		return "", "", nil, err
	}
	if err := atrest.DecryptFile(filepath.Join(entryDir, poolentry.MemFile), memPath, dek); err != nil {
		cleanup()
		return "", "", nil, err
	}
	return statePath, memPath, cleanup, nil
}

// poolKEKPath returns the configured KEK path, falling back to the
// production default shared with cmd/node-agent and setec-pool-vm.
func (s *Server) poolKEKPath() string {
	if s.PoolKEKPath != "" {
		return s.PoolKEKPath
	}
	return "/var/lib/setec/keys/node.key"
}

// shredDir zero-overwrites every regular file directly under dir
// (best effort) before removing the tree. Used for the plaintext temp
// pair CreateSnapshot hands to Firecracker; the restore path keeps
// plain RemoveAll because Firecracker may still have the restored
// memory file mapped — unlinking a mapped file is safe, overwriting
// it is not.
func shredDir(dir string) {
	entries, err := os.ReadDir(dir)
	if err == nil {
		for _, e := range entries {
			if !e.Type().IsRegular() {
				continue
			}
			_ = atrest.Shred(filepath.Join(dir, e.Name()))
		}
	}
	_ = os.RemoveAll(dir)
}

// --- framed stream helpers ----------------------------------------

// makeFramedReader constructs an io.ReadCloser that emits the
// 16-byte framing header followed by the concatenation of statePath
// and memPath. The files are opened lazily on first Read so the
// caller can free the tempdir after Storage.Save has drained the
// stream.
func makeFramedReader(statePath, memPath string) (io.ReadCloser, error) {
	st, err := os.Stat(statePath)
	if err != nil {
		return nil, fmt.Errorf("stat state: %w", err)
	}
	mt, err := os.Stat(memPath)
	if err != nil {
		return nil, fmt.Errorf("stat memory: %w", err)
	}

	header := make([]byte, frameHeaderSize)
	binary.BigEndian.PutUint64(header[0:8], uint64(st.Size()))
	binary.BigEndian.PutUint64(header[8:16], uint64(mt.Size()))

	stateFile, err := os.Open(statePath)
	if err != nil {
		return nil, err
	}
	memFile, err := os.Open(memPath)
	if err != nil {
		_ = stateFile.Close()
		return nil, err
	}
	return &multiReadCloser{
		reader:  io.MultiReader(bytes.NewReader(header), stateFile, memFile),
		closers: []io.Closer{stateFile, memFile},
	}, nil
}

// writeFramedStream reverses the framing produced by
// makeFramedReader: it reads the 16-byte header, then exactly that
// many bytes into statePath and memPath respectively.
func writeFramedStream(r io.Reader, statePath, memPath string) error {
	header := make([]byte, frameHeaderSize)
	if _, err := io.ReadFull(r, header); err != nil {
		return fmt.Errorf("read framed header: %w", err)
	}
	stateSize := binary.BigEndian.Uint64(header[0:8])
	memSize := binary.BigEndian.Uint64(header[8:16])

	if err := writeN(r, statePath, int64(stateSize)); err != nil {
		return fmt.Errorf("state: %w", err)
	}
	if err := writeN(r, memPath, int64(memSize)); err != nil {
		return fmt.Errorf("memory: %w", err)
	}
	return nil
}

func writeN(r io.Reader, path string, n int64) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	_, err = io.CopyN(f, r, n)
	return err
}

// multiReadCloser wraps io.MultiReader with a composite Close.
type multiReadCloser struct {
	reader  io.Reader
	closers []io.Closer
}

func (m *multiReadCloser) Read(p []byte) (int, error) { return m.reader.Read(p) }
func (m *multiReadCloser) Close() error {
	var firstErr error
	for _, c := range m.closers {
		if err := c.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
