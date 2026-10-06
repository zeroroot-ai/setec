// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

// Package grpcserver implements the NodeAgentService gRPC surface that
// the operator dials into. Each RPC acts on the Firecracker machine of
// one launcher Pod on this node: a snapshot, a restore, a pause or a
// resume. The storage backend keeps each snapshot encrypted at rest.
package grpcserver

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	setecgrpcv1 "github.com/zeroroot-ai/setec/api/grpc/v1"
	"github.com/zeroroot-ai/setec/internal/firecracker"
	"github.com/zeroroot-ai/setec/internal/nodeagent/launchersandbox"
	"github.com/zeroroot-ai/setec/internal/podspec"
	"github.com/zeroroot-ai/setec/internal/snapshot/atrest"
	"github.com/zeroroot-ai/setec/internal/snapshot/storage"
)

// Server implements NodeAgentServiceServer.
type Server struct {
	setecgrpcv1.UnimplementedNodeAgentServiceServer

	// Storage is the node-local default backend snapshot state is
	// persisted to (requests with storage_backend "" or "local-disk").
	Storage storage.StorageBackend

	// SessionStorage builds the portable session-checkpoint backend
	// ("s3", docs/design/storage.md) for one call. kek is the
	// per-session key-encryption key that the operator forwards over the
	// mTLS control channel; it lives only for the duration of the RPC.
	// nil means the node has no S3-compatible store, and a session
	// checkpoint RPC fails with FailedPrecondition.
	SessionStorage func(kek []byte) storage.StorageBackend

	// FirecrackerFactory constructs a Firecracker client for a given
	// API socket path. Tests inject a mock here; production wires
	// firecracker.NewClientFromSocket.
	FirecrackerFactory func(sockPath string) firecracker.Client

	// Machines finds the Firecracker files of a launcher Pod on this
	// node from the Pod UID.
	Machines MachineResolver

	// EntropyReseedOff is the explicit --entropy-reseed=off opt-out. A
	// restore then asks the launcher for no reseed, and the operator gate
	// refuses the restore. False is the default: a restore succeeds only
	// once the guest confirmed fresh entropy.
	EntropyReseedOff bool

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
// portable S3-compatible session-checkpoint backend (docs/design/storage.md).
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
	m, err := s.machine(ctx, in.GetSourcePodUid(), "source_pod_uid")
	if err != nil {
		return nil, err
	}
	backend, err := s.backendFor(in.GetStorageBackend(), in.GetSessionKek())
	if err != nil {
		return nil, err
	}

	fc := s.FirecrackerFactory(m.APISocket)

	if err := fc.Pause(ctx); err != nil {
		return nil, status.Errorf(codes.Internal, "firecracker pause: %v", err)
	}

	// Firecracker writes the pair itself and sees only the work volume
	// of the launcher Pod, so the files go under that volume and
	// Firecracker gets the paths as it sees them (setec#19).
	dir := filepath.Join(m.FCRoot, snapshotWorkDir, in.GetSnapshotId())
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, status.Errorf(codes.Internal, "mkdir temp: %v", err)
	}
	statePath := filepath.Join(dir, "state.bin")
	memPath := filepath.Join(dir, "memory.bin")
	fcState, fcMem, err := fcPaths(m, statePath, memPath)
	if err != nil {
		return nil, err
	}

	// Ensure we clean up the temp files even on error paths. The temp
	// pair is the PLAINTEXT guest image (the durable copy written by
	// Storage.Save is encrypted), so it gets the same zero-overwrite
	// treatment the storage backend applies before unlinking.
	defer func() { shredDir(dir) }()

	parent := in.GetParentStorageRef()
	if parent != "" {
		err = fc.CreateDiffSnapshot(ctx, fcState, fcMem)
	} else {
		err = fc.CreateSnapshot(ctx, fcState, fcMem)
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "firecracker createSnapshot: %v", err)
	}
	// A launcher machine keeps its writable layer in the work volume. The
	// copy is taken while the machine is paused, so it matches the memory.
	diskPath := filepath.Join(dir, "writable.ext4")
	if err := copySparse(launchersandbox.HostPath(m, podspec.LauncherWritableDisk), diskPath); err != nil {
		_ = fc.Resume(ctx)
		return nil, status.Errorf(codes.Internal, "copy the writable layer: %v", err)
	}

	// The identity generation rises while the machine is paused: each
	// token after the snapshot carries the new generation, and no token
	// in the snapshot does (setec#235).
	if gen := in.GetIdentityGeneration(); gen > 0 {
		if err := writeIdentityGeneration(launchersandbox.HostPath(m, podspec.LauncherIdentityGeneration), gen); err != nil {
			_ = fc.Resume(ctx)
			return nil, status.Errorf(codes.Internal, "raise the identity generation: %v", err)
		}
	}

	// Resume the source VM now that the state+memory pair is on
	// disk. A resume failure is reported but does not prevent
	// Storage.Save (the persisted snapshot is still valid). A suspend
	// keeps the machine paused until its Pod ends.
	if !in.GetLeavePaused() {
		_ = fc.Resume(ctx)
	}

	// A base for the warm pool holds no tenant data. The scan runs on the
	// plaintext files before the store sees them, and a finding stops the
	// snapshot (ADR-0145 invariant 1, setec#103).
	clean := false
	if in.GetScanForSecrets() {
		files := []string{statePath, memPath, diskPath}
		if err := scanSparseFiles(files); err != nil {
			return nil, status.Errorf(codes.FailedPrecondition, "secret scan: %v", err)
		}
		clean = true
	}

	combined, err := makeLauncherFramedReader(parent, statePath, memPath, diskPath)
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
		StorageRef:        ref,
		SizeBytes:         size,
		CleanBaseVerified: clean,
		Sha256:            "", // Local-disk backend writes sidecar; operator re-reads if needed.
	}, nil
}

// RestoreSandbox reads the framed payload from storage and stages it for
// the launcher of the target Pod, which loads it and confirms the guest.
func (s *Server) RestoreSandbox(ctx context.Context, in *setecgrpcv1.RestoreSandboxRequest) (*setecgrpcv1.RestoreSandboxResponse, error) {
	ctx, span := s.tracer().Start(ctx, "nodeagent.RestoreSandbox")
	defer span.End()
	span.SetAttributes(attribute.String("setec.snapshot_id", in.GetSnapshotId()))

	if err := storage.ValidateSnapshotID(in.GetSnapshotId()); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "snapshot_id: %v", err)
	}
	if in.GetStorageRef() == "" {
		return nil, status.Error(codes.InvalidArgument, "storage_ref required")
	}
	m, err := s.machine(ctx, in.GetTargetPodUid(), "target_pod_uid")
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
	return s.restoreLauncher(ctx, m, rc, backend, in.GetStateTakenUnixNano())
}

// snapshotWorkDir is the directory, under the work volume of the Pod, that
// holds the state and memory files of a snapshot being written.
// Firecracker writes them itself.
const snapshotWorkDir = "setec-snapshots"

// fcPaths maps the host state and memory paths into Firecracker's view.
func fcPaths(m launchersandbox.Paths, statePath, memPath string) (fcState, fcMem string, err error) {
	if fcState, err = m.FCPath(statePath); err != nil {
		return "", "", status.Errorf(codes.Internal, "%v", err)
	}
	if fcMem, err = m.FCPath(memPath); err != nil {
		return "", "", status.Errorf(codes.Internal, "%v", err)
	}
	return fcState, fcMem, nil
}

// MachineResolver finds the Firecracker files of a launcher Pod on this
// node from the Pod UID.
type MachineResolver interface {
	Resolve(ctx context.Context, podUID string) (launchersandbox.Paths, error)
}

// machine resolves the launcher machine of the Pod with podUID, mapping
// the failure to a gRPC status. field names the request field for an
// InvalidArgument.
func (s *Server) machine(ctx context.Context, podUID, field string) (launchersandbox.Paths, error) {
	if podUID == "" {
		return launchersandbox.Paths{}, status.Errorf(codes.InvalidArgument, "%s required", field)
	}
	if s.Machines == nil {
		return launchersandbox.Paths{}, status.Error(codes.FailedPrecondition, "no machine resolver configured")
	}
	p, err := s.Machines.Resolve(ctx, podUID)
	if errors.Is(err, launchersandbox.ErrNotFound) {
		return launchersandbox.Paths{}, status.Errorf(codes.NotFound, "%v", err)
	}
	if err != nil {
		return launchersandbox.Paths{}, status.Errorf(codes.Internal, "resolve the launcher machine: %v", err)
	}
	return p, nil
}

// PauseSandbox is a direct wrap of firecracker.Pause.
func (s *Server) PauseSandbox(ctx context.Context, in *setecgrpcv1.PauseSandboxRequest) (*setecgrpcv1.PauseSandboxResponse, error) {
	ctx, span := s.tracer().Start(ctx, "nodeagent.PauseSandbox")
	defer span.End()
	span.SetAttributes(attribute.String("setec.sandbox_id", in.GetSandboxId()))

	m, err := s.machine(ctx, in.GetTargetPodUid(), "target_pod_uid")
	if err != nil {
		return nil, err
	}
	fc := s.FirecrackerFactory(m.APISocket)
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

	m, err := s.machine(ctx, in.GetTargetPodUid(), "target_pod_uid")
	if err != nil {
		return nil, err
	}
	fc := s.FirecrackerFactory(m.APISocket)
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
