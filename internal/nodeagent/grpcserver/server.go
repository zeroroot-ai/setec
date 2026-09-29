// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

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
	"github.com/zeroroot-ai/setec/internal/nodeagent/katasandbox"
	"github.com/zeroroot-ai/setec/internal/nodeagent/pool"
	"github.com/zeroroot-ai/setec/internal/nodeagent/poolentry"
	"github.com/zeroroot-ai/setec/internal/snapshot/atrest"
	"github.com/zeroroot-ai/setec/internal/snapshot/storage"
	"github.com/zeroroot-ai/setec/internal/uniquify"
)

// Server implements NodeAgentServiceServer.
type Server struct {
	setecgrpcv1.UnimplementedNodeAgentServiceServer

	// Storage is the node-local default backend snapshot state is
	// persisted to (requests with storage_backend "" or "local-disk").
	Storage storage.StorageBackend

	// SessionStorage builds the portable session-checkpoint backend
	// ("s3", ADR-0007) for one call. The caller-provided kek is the
	// per-session key-encryption key the operator read from the
	// session's Kubernetes Secret and forwarded over the mTLS control
	// channel; it lives only for the duration of the RPC. kek may be
	// nil for Delete/Stat, which never touch the KEK. nil
	// SessionStorage means the node has no S3-compatible store
	// configured and session-checkpoint RPCs fail with
	// FailedPrecondition.
	SessionStorage func(kek []byte) storage.StorageBackend

	// FirecrackerFactory constructs a Firecracker client for a given
	// API socket path. Tests inject a mock here; production wires
	// firecracker.NewClientFromSocket.
	FirecrackerFactory func(sockPath string) firecracker.Client

	// KataSandboxes finds a Pod's kata Firecracker socket and hybrid
	// vsock on this node from the Pod UID (setec#19). Production wires
	// a katasandbox.Resolver backed by containerd.
	KataSandboxes KataSandboxResolver

	// Pool is the pre-warm pool manager. When nil, QueryPool returns
	// an empty list (no pool feature).
	Pool *pool.Manager

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
	ReseedVsockPaths func(in *setecgrpcv1.RestoreSandboxRequest, kata katasandbox.Paths) []string

	// ReseedObserver, when non-nil, receives "success" or "failure"
	// after each reseed attempt (metrics hook).
	ReseedObserver func(outcome string)

	// Uniquifier drives the per-restore identity uniquification over
	// the same vsock UDS after the entropy reseed (ADR-0005 invariant
	// 2, setec#189): fresh machine-id/boot-id/hostname, the
	// CNI-assigned Pod IP reconciled in-guest, and the guest's vsock
	// CID reported for the node-local uniqueness check. When non-nil
	// the restore FAILS CLOSED: the RPC only reports success once the
	// guest has verifiably applied the directed identity, and an
	// unconfirmed VM is paused rather than handed over. nil disables
	// enforcement (explicit --restore-uniquify=off opt-out).
	Uniquifier uniquify.Uniquifier

	// CIDs is the node-local vsock CID registry shared with the pool
	// Manager. The restore path registers the CID a restored guest
	// reports and fails closed when another live sandbox on the node
	// already holds it (two restores of one Snapshot would otherwise
	// share the snapshotted CID undetected). nil skips the registry
	// check (tests); Verify still requires a non-zero reported CID.
	CIDs *uniquify.CIDAllocator

	// UniquifyObserver, when non-nil, receives "success" or "failure"
	// after each uniquification attempt (metrics hook).
	UniquifyObserver func(outcome string)

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

func (s *Server) tracer() trace.Tracer {
	if s.Tracer != nil {
		return s.Tracer
	}
	return tracenoop.NewTracerProvider().Tracer("setec.nodeagent.grpc")
}

// sessionBackendName is the storage_backend identifier of the
// portable S3-compatible session-checkpoint backend (ADR-0007).
const sessionBackendName = "s3"

// backendFor routes a request's storage_backend (plus optional
// per-session KEK) to the concrete StorageBackend. "" and
// "local-disk" resolve to the node-local default; "s3" resolves to
// the session-checkpoint composition built around the forwarded
// session KEK.
func (s *Server) backendFor(backendName string, sessionKEK []byte) (storage.StorageBackend, error) {
	switch backendName {
	case "", "local-disk":
		if len(sessionKEK) > 0 {
			return nil, status.Error(codes.InvalidArgument,
				"session_kek is only valid with the s3 storage backend")
		}
		return s.Storage, nil
	case sessionBackendName:
		if s.SessionStorage == nil {
			return nil, status.Error(codes.FailedPrecondition,
				"s3 session-checkpoint backend is not configured on this node (--s3-bucket)")
		}
		return s.SessionStorage(sessionKEK), nil
	default:
		return nil, status.Errorf(codes.InvalidArgument, "unknown storage backend %q", backendName)
	}
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

	// snapshot_id is joined onto a host path below, so it is checked
	// with the storage rule before any directory is created. The
	// storage backend checks it again, but only after the temp pair
	// is written, which is too late for a traversal id.
	if err := storage.ValidateSnapshotID(in.GetSnapshotId()); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "snapshot_id: %v", err)
	}
	kata, err := s.kataPaths(ctx, in.GetSourcePodUid(), "source_pod_uid")
	if err != nil {
		return nil, err
	}
	backend, err := s.backendFor(in.GetStorageBackend(), in.GetSessionKek())
	if err != nil {
		return nil, err
	}

	fc := s.FirecrackerFactory(kata.APISocket)

	if err := fc.Pause(ctx); err != nil {
		return nil, status.Errorf(codes.Internal, "firecracker pause: %v", err)
	}

	// Firecracker writes the pair itself, and kata runs it chrooted
	// into the VM's jailer root, so the files go under that root and
	// Firecracker gets the paths as it sees them (setec#19).
	dir := filepath.Join(kata.FCRoot, snapshotWorkDir, in.GetSnapshotId())
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, status.Errorf(codes.Internal, "mkdir temp: %v", err)
	}
	statePath := filepath.Join(dir, "state.bin")
	memPath := filepath.Join(dir, "memory.bin")
	fcState, fcMem, err := fcPaths(kata, statePath, memPath)
	if err != nil {
		return nil, err
	}

	// Ensure we clean up the temp files even on error paths. The temp
	// pair is the PLAINTEXT guest image (the durable copy written by
	// Storage.Save is encrypted), so it gets the same zero-overwrite
	// treatment the storage backend applies before unlinking.
	defer func() { shredDir(dir) }()

	if err := fc.CreateSnapshot(ctx, fcState, fcMem); err != nil {
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

	size, ref, saveErr := backend.Save(ctx, in.GetSnapshotId(), combined)
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

	// snapshot_id names the restore temp directory, so the same
	// traversal rule applies before any filesystem call.
	if err := storage.ValidateSnapshotID(in.GetSnapshotId()); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "snapshot_id: %v", err)
	}
	if in.GetStorageRef() == "" {
		return nil, status.Error(codes.InvalidArgument, "storage_ref required")
	}
	kata, err := s.kataPaths(ctx, in.GetTargetPodUid(), "target_pod_uid")
	if err != nil {
		return nil, err
	}
	backend, err := s.backendFor(in.GetStorageBackend(), in.GetSessionKek())
	if err != nil {
		return nil, err
	}

	rc, err := backend.Open(ctx, in.GetStorageRef())
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

	dir := filepath.Join(kata.FCRoot, snapshotWorkDir, in.GetSnapshotId()+"-restore-"+fmt.Sprintf("%d", time.Now().UnixNano()))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, status.Errorf(codes.Internal, "mkdir: %v", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	statePath := filepath.Join(dir, "state.bin")
	memPath := filepath.Join(dir, "memory.bin")

	if err := writeFramedStream(rc, statePath, memPath); err != nil {
		return nil, status.Errorf(codes.Internal, "unpack framed stream: %v", err)
	}

	fcState, fcMem, err := fcPaths(kata, statePath, memPath)
	if err != nil {
		return nil, err
	}
	fc := s.FirecrackerFactory(kata.APISocket)
	if err := fc.LoadSnapshot(ctx, fcState, fcMem); err != nil {
		return &setecgrpcv1.RestoreSandboxResponse{
			Success: false,
			Error:   err.Error(),
		}, status.Errorf(codes.Internal, "firecracker loadSnapshot: %v", err)
	}

	candidates := defaultReseedVsockPaths(in, kata)
	if s.ReseedVsockPaths != nil {
		candidates = s.ReseedVsockPaths(in, kata)
	}

	// Active entropy reseed (setec#72). The snapshot's CSPRNG state is
	// shared by every clone restored from it; before the restore is
	// reported usable, push fresh entropy into the guest and require
	// the in-guest agent's digest-verified ack. Fail closed: an
	// unconfirmed reseed pauses the VM and fails the RPC so the
	// Sandbox is never marked Ready.
	reseeded := false
	if s.Reseeder != nil {
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
		reseeded = true
	}

	// Per-restore uniquification (ADR-0005 invariant 2, setec#189):
	// direct the restored guest to adopt a fresh machine-id, boot-id,
	// and hostname, reconcile its interface to the CNI-assigned Pod
	// IP, and report its vsock CID for the node-local uniqueness
	// check. Same fail-closed contract as the reseed: an unconfirmed
	// identity pauses the VM and fails the RPC.
	uniquified := false
	if s.Uniquifier != nil {
		if err := s.uniquifyRestored(ctx, candidates, in.GetSandboxId(), in.GetHostname(), in.GetPodIp(), 0); err != nil {
			s.observeUniquify("failure")
			msg := fmt.Sprintf("restore uniquification failed (failing closed): %v", err)
			if pauseErr := fc.Pause(ctx); pauseErr != nil {
				msg += fmt.Sprintf("; additionally failed to pause the un-uniquified VM: %v", pauseErr)
			}
			return &setecgrpcv1.RestoreSandboxResponse{
				Success: false,
				Error:   msg,
			}, status.Error(codes.Internal, msg)
		}
		s.observeUniquify("success")
		uniquified = true
	}

	return &setecgrpcv1.RestoreSandboxResponse{
		Success:         true,
		EntropyReseeded: reseeded,
		Uniquified:      uniquified,
		// Reported from the attested capability of the backend that
		// actually served THIS restore (node-local or per-session S3),
		// never assumed: the operator-side invariant gate (ADR-0005)
		// fails closed on false outside dev.
		EncryptedAtRest: storage.IsEncryptedAtRest(backend),
	}, nil
}

// uniquifyRestored mints a fresh per-restore identity, pushes it to
// the restored guest through the first reachable vsock candidate, and
// registers the guest's reported CID in the node-local registry.
// expectedCID, when non-zero, additionally pins the report to the CID
// the pool entry was booted with. Any failure means the restore must
// not be handed over.
func (s *Server) uniquifyRestored(
	ctx context.Context,
	candidates []string,
	owner, hostname, podIP string,
	expectedCID uint32,
) error {
	spec, err := uniquify.NewSpec(hostname, podIP)
	if err != nil {
		return err
	}
	report, err := uniquify.UniquifyFirst(ctx, s.Uniquifier, candidates, spec)
	if err != nil {
		return err
	}
	if expectedCID != 0 && report.GuestCID != expectedCID {
		return fmt.Errorf("uniquify: guest reports CID %d but the pool entry was booted with %d",
			report.GuestCID, expectedCID)
	}
	if s.CIDs != nil {
		if err := s.CIDs.Observe(report.GuestCID, owner); err != nil {
			return err
		}
	}
	return nil
}

// observeUniquify invokes the optional metrics hook.
func (s *Server) observeUniquify(outcome string) {
	if s.UniquifyObserver != nil {
		s.UniquifyObserver(outcome)
	}
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
//   - the target kata sandbox's hybrid vsock, <vm>/root/kata.hvsock
//     (katasandbox), for restores into kata-managed pods.
//
// Candidate probing is not a fail-open: whichever path connects must
// still complete the digest-verified reseed, and if none does the
// restore fails closed.
func defaultReseedVsockPaths(in *setecgrpcv1.RestoreSandboxRequest, kata katasandbox.Paths) []string {
	var out []string
	if ref := in.GetStorageRef(); ref != "" && filepath.IsAbs(ref) {
		out = append(out, filepath.Join(ref, "vsock.sock"))
	}
	if kata.HybridVsock != "" {
		out = append(out, kata.HybridVsock)
	}
	return out
}

// snapshotWorkDir is the directory, under a VM's Firecracker root, that
// holds the state and memory files of a snapshot being written or
// loaded. Firecracker reads and writes them itself.
const snapshotWorkDir = "setec-snapshots"

// fcPaths maps the host state and memory paths into Firecracker's view.
func fcPaths(kata katasandbox.Paths, statePath, memPath string) (fcState, fcMem string, err error) {
	if fcState, err = kata.FCPath(statePath); err != nil {
		return "", "", status.Errorf(codes.Internal, "%v", err)
	}
	if fcMem, err = kata.FCPath(memPath); err != nil {
		return "", "", status.Errorf(codes.Internal, "%v", err)
	}
	return fcState, fcMem, nil
}

// KataSandboxResolver finds a Pod's kata Firecracker files on this
// node from the Pod UID.
type KataSandboxResolver interface {
	Resolve(ctx context.Context, podUID string) (katasandbox.Paths, error)
}

// kataPaths resolves the kata sandbox of the Pod with podUID, mapping
// the failure to a gRPC status. field names the request field for an
// InvalidArgument.
func (s *Server) kataPaths(ctx context.Context, podUID, field string) (katasandbox.Paths, error) {
	if podUID == "" {
		return katasandbox.Paths{}, status.Errorf(codes.InvalidArgument, "%s required", field)
	}
	if s.KataSandboxes == nil {
		return katasandbox.Paths{}, status.Error(codes.FailedPrecondition, "no kata sandbox resolver configured")
	}
	p, err := s.KataSandboxes.Resolve(ctx, podUID)
	if errors.Is(err, katasandbox.ErrNotFound) {
		return katasandbox.Paths{}, status.Errorf(codes.NotFound, "%v", err)
	}
	if err != nil {
		return katasandbox.Paths{}, status.Errorf(codes.Internal, "resolve kata sandbox: %v", err)
	}
	return p, nil
}

// PauseSandbox is a direct wrap of firecracker.Pause.
func (s *Server) PauseSandbox(ctx context.Context, in *setecgrpcv1.PauseSandboxRequest) (*setecgrpcv1.PauseSandboxResponse, error) {
	ctx, span := s.tracer().Start(ctx, "nodeagent.PauseSandbox")
	defer span.End()
	span.SetAttributes(attribute.String("setec.sandbox_id", in.GetSandboxId()))

	kata, err := s.kataPaths(ctx, in.GetTargetPodUid(), "target_pod_uid")
	if err != nil {
		return nil, err
	}
	fc := s.FirecrackerFactory(kata.APISocket)
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

	kata, err := s.kataPaths(ctx, in.GetTargetPodUid(), "target_pod_uid")
	if err != nil {
		return nil, err
	}
	fc := s.FirecrackerFactory(kata.APISocket)
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
	backend, berr := s.backendFor(in.GetStorageBackend(), nil)
	if berr != nil {
		return nil, berr
	}
	if err := backend.Delete(ctx, in.GetStorageRef()); err != nil {
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
	kata, err := s.kataPaths(ctx, in.GetTargetPodUid(), "target_pod_uid")
	if err != nil {
		return nil, err
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
	statePath, memPath, cleanup, decErr := s.decryptPoolEntry(entry.StorageRef, entry.ID, filepath.Join(kata.FCRoot, snapshotWorkDir))
	if decErr != nil {
		s.observeClaim("restore_failed")
		return &setecgrpcv1.ClaimPoolEntryResponse{
			Claimed: true,
			EntryId: entry.ID,
			Error:   fmt.Sprintf("decrypt pool entry state: %v", decErr),
		}, nil
	}
	defer cleanup()

	fcState, fcMem, pathErr := fcPaths(kata, statePath, memPath)
	if pathErr != nil {
		s.observeClaim("restore_failed")
		return &setecgrpcv1.ClaimPoolEntryResponse{Claimed: true, EntryId: entry.ID, Error: pathErr.Error()}, nil
	}
	fc := s.FirecrackerFactory(kata.APISocket)
	if err := fc.LoadSnapshot(ctx, fcState, fcMem); err != nil {
		s.observeClaim("restore_failed")
		return &setecgrpcv1.ClaimPoolEntryResponse{
			Claimed: true,
			EntryId: entry.ID,
			Error:   fmt.Sprintf("firecracker loadSnapshot: %v", err),
		}, nil
	}

	candidates := []string{
		filepath.Join(entry.StorageRef, "vsock.sock"),
		kata.HybridVsock,
	}

	// Active entropy reseed (setec#72), identical fail-closed contract
	// to RestoreSandbox: the pool entry's CSPRNG state is shared with
	// the paused template VM, so the restored clone must confirm fresh
	// entropy before it is handed over. On failure the VM is paused
	// and the operator falls back to cold boot.
	reseeded := false
	if s.Reseeder != nil {
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
		reseeded = true
	}

	// Per-restore uniquification (ADR-0005 invariant 2, setec#189),
	// identical fail-closed contract: the warm-started clone must
	// verifiably adopt its fresh identity (machine-id / boot-id /
	// hostname / Pod IP) and its vsock CID — pinned to the one the
	// entry was booted with — must be unique on the node.
	uniquified := false
	if s.Uniquifier != nil {
		if err := s.uniquifyRestored(ctx, candidates, in.GetSandboxId(), in.GetHostname(), in.GetPodIp(), entry.GuestCID); err != nil {
			s.observeUniquify("failure")
			s.observeClaim("restore_failed")
			msg := fmt.Sprintf("restore uniquification failed (failing closed): %v", err)
			if pauseErr := fc.Pause(ctx); pauseErr != nil {
				msg += fmt.Sprintf("; additionally failed to pause the un-uniquified VM: %v", pauseErr)
			}
			return &setecgrpcv1.ClaimPoolEntryResponse{
				Claimed: true,
				EntryId: entry.ID,
				Error:   msg,
			}, nil
		}
		s.observeUniquify("success")
		uniquified = true
	}

	s.observeClaim("restored")
	return &setecgrpcv1.ClaimPoolEntryResponse{
		Claimed:         true,
		Success:         true,
		EntryId:         entry.ID,
		EntropyReseeded: reseeded,
		Uniquified:      uniquified,
		// These attestations are earned by the only code path that can
		// reach this return: Claim verified the template-provenance
		// record AND the recorded scan verdict (pool.Manager →
		// poolentry.Verify / VerifyScan), and decryptPoolEntry
		// unsealed the per-entry DEK under AAD that binds both records
		// (a foreign or tampered record makes the DEK unopenable),
		// decrypted the always-encrypted state pair, and matched the
		// plaintext digests against the verdict — so the clean-base
		// signal (invariant 1) is independent evidence, not an
		// inference from provenance (invariant 4). ADR-0005
		// invariants 1, 4 and 5.
		ProvenanceVerified: true,
		EncryptedAtRest:    true,
		CleanBaseVerified:  true,
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
func (s *Server) decryptPoolEntry(entryDir, entryID, workDir string) (statePath, memPath string, cleanup func(), err error) {
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
	// The clean-base scan verdict (ADR-0005 invariant 1, setec#206) is
	// as load-bearing as the provenance record: it must exist and be
	// clean (fail closed on absence), it is AAD-bound into the sealed
	// DEK so a swapped or tampered record makes the DEK unopenable,
	// and the digests it names must match the plaintext this claim
	// decrypts — "this exact artifact was scanned clean", per restore.
	verdict, err := poolentry.VerifyScan(entryDir)
	if err != nil {
		return "", "", nil, err
	}
	dek, err := atrest.OpenDEK(kek, sealed, poolentry.DEKAAD(entryID, prov, verdict))
	if err != nil {
		return "", "", nil, err
	}

	dir := filepath.Join(workDir, "pool-claim-"+entryID+"-"+fmt.Sprintf("%d", time.Now().UnixNano()))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", nil, err
	}
	cleanup = func() { _ = os.RemoveAll(dir) }
	statePath = filepath.Join(dir, poolentry.StateFile)
	memPath = filepath.Join(dir, poolentry.MemFile)
	stateDigest, err := atrest.DecryptFile(filepath.Join(entryDir, poolentry.StateFile), statePath, dek)
	if err != nil {
		cleanup()
		return "", "", nil, err
	}
	memDigest, err := atrest.DecryptFile(filepath.Join(entryDir, poolentry.MemFile), memPath, dek)
	if err != nil {
		cleanup()
		return "", "", nil, err
	}
	if stateDigest != verdict.StateSHA256 || memDigest != verdict.MemorySHA256 {
		cleanup()
		return "", "", nil, fmt.Errorf("%w: decrypted artifact digests do not match the recorded scan verdict", poolentry.ErrScanVerdict)
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
