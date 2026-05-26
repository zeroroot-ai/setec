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

	setecgrpcv1alpha1 "github.com/zeroroot-ai/setec/api/grpc/v1alpha1"
	"github.com/zeroroot-ai/setec/internal/firecracker"
	"github.com/zeroroot-ai/setec/internal/nodeagent/pool"
	"github.com/zeroroot-ai/setec/internal/snapshot/storage"
)

// Server implements NodeAgentServiceServer.
type Server struct {
	setecgrpcv1alpha1.UnimplementedNodeAgentServiceServer

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
func (s *Server) CreateSnapshot(ctx context.Context, in *setecgrpcv1alpha1.CreateSnapshotRequest) (*setecgrpcv1alpha1.CreateSnapshotResponse, error) {
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

	// Ensure we clean up the temp files even on error paths.
	defer func() { _ = os.RemoveAll(dir) }()

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

	return &setecgrpcv1alpha1.CreateSnapshotResponse{
		StorageRef: ref,
		SizeBytes:  size,
		Sha256:     "", // Local-disk backend writes sidecar; operator re-reads if needed.
	}, nil
}

// RestoreSandbox reads the framed payload from storage, writes the
// two temp files, and asks Firecracker to LoadSnapshot.
func (s *Server) RestoreSandbox(ctx context.Context, in *setecgrpcv1alpha1.RestoreSandboxRequest) (*setecgrpcv1alpha1.RestoreSandboxResponse, error) {
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
		return &setecgrpcv1alpha1.RestoreSandboxResponse{
			Success: false,
			Error:   err.Error(),
		}, status.Errorf(codes.Internal, "firecracker loadSnapshot: %v", err)
	}

	return &setecgrpcv1alpha1.RestoreSandboxResponse{Success: true}, nil
}

// PauseSandbox is a direct wrap of firecracker.Pause.
func (s *Server) PauseSandbox(ctx context.Context, in *setecgrpcv1alpha1.PauseSandboxRequest) (*setecgrpcv1alpha1.PauseSandboxResponse, error) {
	ctx, span := s.tracer().Start(ctx, "nodeagent.PauseSandbox")
	defer span.End()
	span.SetAttributes(attribute.String("setec.sandbox_id", in.GetSandboxId()))

	if in.GetKataSocketTarget() == "" {
		return nil, status.Error(codes.InvalidArgument, "kata_socket_target required")
	}
	fc := s.FirecrackerFactory(in.GetKataSocketTarget())
	if err := fc.Pause(ctx); err != nil {
		return &setecgrpcv1alpha1.PauseSandboxResponse{
			Success: false,
			Error:   err.Error(),
		}, status.Errorf(codes.Internal, "firecracker pause: %v", err)
	}
	return &setecgrpcv1alpha1.PauseSandboxResponse{Success: true}, nil
}

// ResumeSandbox is a direct wrap of firecracker.Resume.
func (s *Server) ResumeSandbox(ctx context.Context, in *setecgrpcv1alpha1.ResumeSandboxRequest) (*setecgrpcv1alpha1.ResumeSandboxResponse, error) {
	ctx, span := s.tracer().Start(ctx, "nodeagent.ResumeSandbox")
	defer span.End()
	span.SetAttributes(attribute.String("setec.sandbox_id", in.GetSandboxId()))

	if in.GetKataSocketTarget() == "" {
		return nil, status.Error(codes.InvalidArgument, "kata_socket_target required")
	}
	fc := s.FirecrackerFactory(in.GetKataSocketTarget())
	if err := fc.Resume(ctx); err != nil {
		return &setecgrpcv1alpha1.ResumeSandboxResponse{
			Success: false,
			Error:   err.Error(),
		}, status.Errorf(codes.Internal, "firecracker resume: %v", err)
	}
	return &setecgrpcv1alpha1.ResumeSandboxResponse{Success: true}, nil
}

// DeleteSnapshot invokes Storage.Delete so the state files are
// securely erased.
func (s *Server) DeleteSnapshot(ctx context.Context, in *setecgrpcv1alpha1.DeleteSnapshotRequest) (*setecgrpcv1alpha1.DeleteSnapshotResponse, error) {
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
			return &setecgrpcv1alpha1.DeleteSnapshotResponse{Success: true}, nil
		}
		return &setecgrpcv1alpha1.DeleteSnapshotResponse{
			Success: false,
			Error:   err.Error(),
		}, status.Errorf(codes.Internal, "storage delete: %v", err)
	}
	return &setecgrpcv1alpha1.DeleteSnapshotResponse{Success: true}, nil
}

// QueryPool delegates to Pool.QueryAvailable.
func (s *Server) QueryPool(ctx context.Context, in *setecgrpcv1alpha1.QueryPoolRequest) (*setecgrpcv1alpha1.QueryPoolResponse, error) {
	_, span := s.tracer().Start(ctx, "nodeagent.QueryPool")
	defer span.End()
	span.SetAttributes(attribute.String("setec.class", in.GetSandboxClass()))

	if s.Pool == nil {
		return &setecgrpcv1alpha1.QueryPoolResponse{}, nil
	}
	entries := s.Pool.QueryAvailable(in.GetSandboxClass(), in.GetImageRef())
	now := time.Now()
	resp := &setecgrpcv1alpha1.QueryPoolResponse{}
	for _, e := range entries {
		resp.Entries = append(resp.Entries, &setecgrpcv1alpha1.PoolEntry{
			EntryId:    e.ID,
			ImageRef:   e.ImageRef,
			Available:  true,
			AgeSeconds: int64(now.Sub(e.PausedAt).Seconds()),
		})
	}
	return resp, nil
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
