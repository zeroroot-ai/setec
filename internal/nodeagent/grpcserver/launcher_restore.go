// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package grpcserver

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	setecgrpcv1 "github.com/zeroroot-ai/setec/api/grpc/v1"
	"github.com/zeroroot-ai/setec/internal/nodeagent/katasandbox"
	"github.com/zeroroot-ai/setec/internal/nodeagent/launchersandbox"
	"github.com/zeroroot-ai/setec/internal/podspec"
	"github.com/zeroroot-ai/setec/internal/snapshot/storage"
)

// defaultLauncherRestoreWait bounds the wait for the evidence of the
// launcher when the call has no deadline.
const defaultLauncherRestoreWait = 2 * time.Minute

// launcherEvidencePoll is the interval at which the node agent looks for
// the evidence file of the launcher.
const launcherEvidencePoll = 100 * time.Millisecond

// restoreLauncher stages a snapshot for the launcher of a Pod and returns
// the evidence of the launcher. The writable layer goes where the launcher
// expects it, and the launcher uses it as it is. The launcher loads the files itself,
// reseeds the guest, gives it a new identity and sets its clock, and only
// then joins it to the Pod network. A missing or negative piece of
// evidence is a failed restore: the operator gate refuses it.
func (s *Server) restoreLauncher(
	ctx context.Context, p katasandbox.Paths, rc io.Reader, backend storage.StorageBackend, takenAtUnixNano int64,
) (*setecgrpcv1.RestoreSandboxResponse, error) {
	statePath := launchersandbox.HostPath(p, podspec.LauncherRestoreState)
	memPath := launchersandbox.HostPath(p, podspec.LauncherRestoreMemory)
	staged := launchersandbox.HostPath(p, podspec.LauncherRestoreStaged)
	evidencePath := launchersandbox.HostPath(p, podspec.LauncherRestoreEvidence)
	if err := os.MkdirAll(filepath.Dir(statePath), 0o700); err != nil {
		return nil, status.Errorf(codes.Internal, "mkdir: %v", err)
	}
	// The plaintext guest image leaves the node disk once the launcher
	// has it. Firecracker maps the memory file privately, so an unlink
	// after the load is safe and an overwrite is not.
	defer func() {
		_ = os.Remove(statePath)
		_ = os.Remove(memPath)
	}()
	diskPath := launchersandbox.HostPath(p, podspec.LauncherWritableDisk)
	openParent := func(ref string) (io.ReadCloser, error) { return backend.Open(ctx, ref) }
	if err := writeLauncherFramedStream(rc, statePath, memPath, diskPath, openParent); err != nil {
		return nil, status.Errorf(codes.Internal, "unpack framed stream: %v", err)
	}
	if takenAtUnixNano > 0 {
		takenAt := launchersandbox.HostPath(p, podspec.LauncherRestoreTakenAt)
		if err := os.WriteFile(takenAt, []byte(strconv.FormatInt(takenAtUnixNano, 10)), 0o600); err != nil {
			return nil, status.Errorf(codes.Internal, "write the time of the state: %v", err)
		}
	}
	if err := os.WriteFile(staged, nil, 0o600); err != nil {
		return nil, status.Errorf(codes.Internal, "mark the snapshot staged: %v", err)
	}

	ev, err := waitLauncherEvidence(ctx, evidencePath)
	if err != nil {
		return &setecgrpcv1.RestoreSandboxResponse{Success: false, Error: err.Error()},
			status.Errorf(codes.DeadlineExceeded, "%v", err)
	}
	if ev.Error != "" || !ev.EntropyReseeded || !ev.Uniquified || !ev.ClockSet {
		msg := fmt.Sprintf("the launcher did not confirm the restored guest (failing closed): %+v", ev)
		return &setecgrpcv1.RestoreSandboxResponse{Success: false, Error: msg}, status.Error(codes.Internal, msg)
	}
	return &setecgrpcv1.RestoreSandboxResponse{
		Success:         true,
		EntropyReseeded: ev.EntropyReseeded,
		Uniquified:      ev.Uniquified,
		EncryptedAtRest: storage.IsEncryptedAtRest(backend),
	}, nil
}

// waitLauncherEvidence waits for the evidence file of the launcher.
func waitLauncherEvidence(ctx context.Context, path string) (podspec.RestoreEvidence, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, defaultLauncherRestoreWait)
		defer cancel()
	}
	for {
		raw, err := os.ReadFile(path) //nolint:gosec // a path in the work volume of the Pod
		if err == nil {
			var ev podspec.RestoreEvidence
			if err := json.Unmarshal(raw, &ev); err != nil {
				return ev, fmt.Errorf("the evidence of the launcher is not valid JSON: %w", err)
			}
			return ev, nil
		}
		if !os.IsNotExist(err) {
			return podspec.RestoreEvidence{}, err
		}
		select {
		case <-ctx.Done():
			return podspec.RestoreEvidence{}, fmt.Errorf("the launcher wrote no evidence: %w", ctx.Err())
		case <-time.After(launcherEvidencePoll):
		}
	}
}
