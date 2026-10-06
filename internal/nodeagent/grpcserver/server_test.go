// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package grpcserver

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	setecgrpcv1 "github.com/zeroroot-ai/setec/api/grpc/v1"
	"github.com/zeroroot-ai/setec/internal/firecracker"
	"github.com/zeroroot-ai/setec/internal/nodeagent/launchersandbox"
	"github.com/zeroroot-ai/setec/internal/podspec"
	"github.com/zeroroot-ai/setec/internal/snapshot/storage"
)

// fakeFirecracker records calls and optionally returns errors.
type fakeFirecracker struct {
	mu         sync.Mutex
	pauseCalls int
	resumeOK   bool
	createOK   bool

	pauseErr  error
	createErr error
	// root is the directory the fake runs "chrooted" in: it resolves
	// the paths it is handed under root, as a jailed Firecracker does.
	root            string
	lastCreateState string
}

func (f *fakeFirecracker) Pause(_ context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pauseCalls++
	return f.pauseErr
}
func (f *fakeFirecracker) Resume(_ context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resumeOK = true
	return nil
}
func (f *fakeFirecracker) CreateSnapshot(_ context.Context, state, mem string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return f.createErr
	}
	// Write plausible files so Storage.Save can read them.
	f.lastCreateState = state
	_ = os.WriteFile(f.host(state), []byte("STATE"), 0o600)
	_ = os.WriteFile(f.host(mem), []byte("MEMORY-PAYLOAD"), 0o600)
	f.createOK = true
	return nil
}

// host maps a path that Firecracker sees in the launcher Pod to the host.
func (f *fakeFirecracker) host(p string) string {
	return filepath.Join(f.root, strings.TrimPrefix(p, podspec.LauncherWorkMountPath))
}

// testPodUID is the Pod UID every test request names.
const testPodUID = "05c716c8-eae5-4540-9daf-5c0bb659810a"

// fakeMachine resolves testPodUID to a launcher work volume at root, with
// a writable layer, and any other UID to launchersandbox.ErrNotFound.
type fakeMachine struct{ root string }

func (m fakeMachine) Resolve(_ context.Context, podUID string) (launchersandbox.Paths, error) {
	if podUID != testPodUID {
		return launchersandbox.Paths{}, launchersandbox.ErrNotFound
	}
	if err := os.MkdirAll(filepath.Join(m.root, "vm"), 0o700); err != nil {
		return launchersandbox.Paths{}, err
	}
	disk := filepath.Join(m.root, "writable.ext4")
	if _, err := os.Stat(disk); os.IsNotExist(err) {
		_ = os.WriteFile(disk, []byte("DISK"), 0o600)
	}
	return launchersandbox.Paths{
		APISocket: filepath.Join(m.root, "vm", podspec.LauncherAPISocket),
		FCRoot:    m.root,
		FCMount:   podspec.LauncherWorkMountPath,
	}, nil
}

// newServer wires a Server with a LocalDiskBackend rooted in a
// tempdir and the provided fakeFirecracker.
func newServer(t *testing.T, fc *fakeFirecracker) *Server {
	t.Helper()
	backend := &storage.LocalDiskBackend{Root: t.TempDir()}
	fc.root = t.TempDir()
	return &Server{
		Storage:            backend,
		FirecrackerFactory: func(_ string) firecracker.Client { return fc },
		Machines:           fakeMachine{root: fc.root},
	}
}

// newBufconnClient starts a gRPC server backed by srv on a bufconn
// listener and returns a connected client.
func newBufconnClient(t *testing.T, srv *Server) setecgrpcv1.NodeAgentServiceClient {
	t.Helper()
	lis := bufconn.Listen(1024 * 1024)
	grpcSrv := grpc.NewServer()
	setecgrpcv1.RegisterNodeAgentServiceServer(grpcSrv, srv)
	go func() { _ = grpcSrv.Serve(lis) }()
	t.Cleanup(func() {
		grpcSrv.Stop()
		_ = lis.Close()
	})

	conn, err := grpc.NewClient(
		"passthrough:///bufnet",
		grpc.WithContextDialer(func(_ context.Context, _ string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return setecgrpcv1.NewNodeAgentServiceClient(conn)
}

func TestCreateSnapshot_Happy(t *testing.T) {
	fc := &fakeFirecracker{}
	srv := newServer(t, fc)
	cli := newBufconnClient(t, srv)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := cli.CreateSnapshot(ctx, &setecgrpcv1.CreateSnapshotRequest{
		SandboxId:      "ns/s",
		SnapshotId:     "snap-1",
		StorageBackend: "local-disk",
		SourcePodUid:   testPodUID,
	})
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	if resp.StorageRef != "snap-1" {
		t.Fatalf("storage_ref = %q", resp.StorageRef)
	}
	if resp.SizeBytes <= 0 {
		t.Fatalf("size = %d", resp.SizeBytes)
	}
	if fc.pauseCalls == 0 || !fc.createOK || !fc.resumeOK {
		t.Fatalf("firecracker state: pause=%d create=%v resume=%v", fc.pauseCalls, fc.createOK, fc.resumeOK)
	}

	// The stored stream is a launcher frame: it unpacks to the state,
	// the memory and the writable layer.
	rc, err := srv.Storage.Open(ctx, "snap-1")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = rc.Close() }()
	out := t.TempDir()
	st, mem, disk := filepath.Join(out, "s"), filepath.Join(out, "m"), filepath.Join(out, "d")
	if err := writeLauncherFramedStream(rc, st, mem, disk, nil); err != nil {
		t.Fatalf("unpack: %v", err)
	}
	for path, want := range map[string]string{st: "STATE", mem: "MEMORY-PAYLOAD", disk: "DISK"} {
		if got, _ := os.ReadFile(path); !bytes.HasPrefix(got, []byte(want)) {
			t.Fatalf("%s = %q, want %q", path, got, want)
		}
	}
}

func TestCreateSnapshot_MissingSnapshotID(t *testing.T) {
	fc := &fakeFirecracker{}
	cli := newBufconnClient(t, newServer(t, fc))
	_, err := cli.CreateSnapshot(context.Background(), &setecgrpcv1.CreateSnapshotRequest{
		SourcePodUid: testPodUID,
	})
	if s, _ := status.FromError(err); s.Code() != codes.InvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument", s.Code())
	}
}

func TestCreateSnapshot_MissingSocket(t *testing.T) {
	fc := &fakeFirecracker{}
	cli := newBufconnClient(t, newServer(t, fc))
	_, err := cli.CreateSnapshot(context.Background(), &setecgrpcv1.CreateSnapshotRequest{
		SnapshotId: "s",
	})
	if s, _ := status.FromError(err); s.Code() != codes.InvalidArgument {
		t.Fatalf("code = %v", s.Code())
	}
}

func TestCreateSnapshot_PauseErrorPropagates(t *testing.T) {
	fc := &fakeFirecracker{pauseErr: errors.New("already paused")}
	cli := newBufconnClient(t, newServer(t, fc))
	_, err := cli.CreateSnapshot(context.Background(), &setecgrpcv1.CreateSnapshotRequest{
		SnapshotId: "s", SourcePodUid: testPodUID,
	})
	if s, _ := status.FromError(err); s.Code() != codes.Internal {
		t.Fatalf("code = %v", s.Code())
	}
}

func TestCreateSnapshot_InsufficientStorage(t *testing.T) {
	fc := &fakeFirecracker{}
	srv := newServer(t, fc)
	// Swap backend for one that always returns ErrInsufficientStorage.
	srv.Storage = &stubBackend{saveErr: storage.ErrInsufficientStorage}
	cli := newBufconnClient(t, srv)
	_, err := cli.CreateSnapshot(context.Background(), &setecgrpcv1.CreateSnapshotRequest{
		SnapshotId: "x", SourcePodUid: testPodUID,
	})
	if s, _ := status.FromError(err); s.Code() != codes.ResourceExhausted {
		t.Fatalf("code = %v, want ResourceExhausted", s.Code())
	}
}

func TestRestoreSandbox_MissingArgs(t *testing.T) {
	fc := &fakeFirecracker{}
	cli := newBufconnClient(t, newServer(t, fc))
	_, err := cli.RestoreSandbox(context.Background(), &setecgrpcv1.RestoreSandboxRequest{})
	if s, _ := status.FromError(err); s.Code() != codes.InvalidArgument {
		t.Fatalf("code = %v", s.Code())
	}
	_, err = cli.RestoreSandbox(context.Background(), &setecgrpcv1.RestoreSandboxRequest{StorageRef: "r"})
	if s, _ := status.FromError(err); s.Code() != codes.InvalidArgument {
		t.Fatalf("code = %v", s.Code())
	}
}

func TestRestoreSandbox_NotFound(t *testing.T) {
	fc := &fakeFirecracker{}
	cli := newBufconnClient(t, newServer(t, fc))
	_, err := cli.RestoreSandbox(context.Background(), &setecgrpcv1.RestoreSandboxRequest{
		SnapshotId: "ghost", StorageRef: "ghost", TargetPodUid: testPodUID,
	})
	if s, _ := status.FromError(err); s.Code() != codes.NotFound {
		t.Fatalf("code = %v", s.Code())
	}
}

func TestRestoreSandbox_Corrupted(t *testing.T) {
	fc := &fakeFirecracker{}
	srv := newServer(t, fc)
	srv.Storage = &stubBackend{openErr: storage.ErrCorrupted}
	cli := newBufconnClient(t, srv)
	_, err := cli.RestoreSandbox(context.Background(), &setecgrpcv1.RestoreSandboxRequest{
		SnapshotId: "s", StorageRef: "r", TargetPodUid: testPodUID,
	})
	if s, _ := status.FromError(err); s.Code() != codes.DataLoss {
		t.Fatalf("code = %v, want DataLoss", s.Code())
	}
}

func TestPauseSandbox_Happy(t *testing.T) {
	fc := &fakeFirecracker{}
	cli := newBufconnClient(t, newServer(t, fc))
	resp, err := cli.PauseSandbox(context.Background(), &setecgrpcv1.PauseSandboxRequest{
		SandboxId: "ns/s", TargetPodUid: testPodUID,
	})
	if err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if !resp.Success {
		t.Fatalf("success=false: %s", resp.Error)
	}
	if fc.pauseCalls != 1 {
		t.Fatalf("pause calls = %d", fc.pauseCalls)
	}
}

func TestPauseSandbox_MissingSocket(t *testing.T) {
	cli := newBufconnClient(t, newServer(t, &fakeFirecracker{}))
	_, err := cli.PauseSandbox(context.Background(), &setecgrpcv1.PauseSandboxRequest{})
	if s, _ := status.FromError(err); s.Code() != codes.InvalidArgument {
		t.Fatalf("code = %v", s.Code())
	}
}

func TestPauseSandbox_FirecrackerError(t *testing.T) {
	fc := &fakeFirecracker{pauseErr: errors.New("nope")}
	cli := newBufconnClient(t, newServer(t, fc))
	_, err := cli.PauseSandbox(context.Background(), &setecgrpcv1.PauseSandboxRequest{
		TargetPodUid: testPodUID,
	})
	if s, _ := status.FromError(err); s.Code() != codes.Internal {
		t.Fatalf("code = %v", s.Code())
	}
}

func TestResumeSandbox_Happy(t *testing.T) {
	fc := &fakeFirecracker{}
	cli := newBufconnClient(t, newServer(t, fc))
	resp, err := cli.ResumeSandbox(context.Background(), &setecgrpcv1.ResumeSandboxRequest{
		TargetPodUid: testPodUID,
	})
	if err != nil || !resp.Success {
		t.Fatalf("Resume: %v %v", err, resp)
	}
}

func TestResumeSandbox_MissingSocket(t *testing.T) {
	cli := newBufconnClient(t, newServer(t, &fakeFirecracker{}))
	_, err := cli.ResumeSandbox(context.Background(), &setecgrpcv1.ResumeSandboxRequest{})
	if s, _ := status.FromError(err); s.Code() != codes.InvalidArgument {
		t.Fatalf("code = %v", s.Code())
	}
}

// stubBackend satisfies storage.StorageBackend for tests that want
// Save or Open to surface a specific sentinel.
type stubBackend struct {
	saveErr error
	openErr error
}

func (s *stubBackend) Save(_ context.Context, id string, r io.Reader) (int64, string, error) {
	if s.saveErr != nil {
		return 0, "", s.saveErr
	}
	_, _ = io.Copy(io.Discard, r)
	return 0, id, nil
}
func (s *stubBackend) Open(_ context.Context, _ string) (io.ReadCloser, error) {
	if s.openErr != nil {
		return nil, s.openErr
	}
	return io.NopCloser(bytes.NewReader(nil)), nil
}
func (s *stubBackend) Delete(_ context.Context, _ string) error              { return nil }
func (s *stubBackend) Stat(_ context.Context, _ string) (int64, bool, error) { return 0, false, nil }

// traversalFCRoot points srv and fc at a Firecracker root nested four
// levels under a fresh test directory. Snapshot files are written under
// <fcroot>/setec-snapshots, so a snapshot_id of ../../../../var/lib/kubelet
// would resolve to root/a/b/var/lib/kubelet and never leave the test
// directory. The work directory does not exist yet, so its absence after
// the RPC proves the server made no directory at all.
func traversalFCRoot(t *testing.T, srv *Server, fc *fakeFirecracker) (root, workDir string) {
	t.Helper()
	root = t.TempDir()
	fcRoot := filepath.Join(root, "a", "b", "c", "d", "fcroot")
	fc.root = fcRoot
	srv.Machines = fakeMachine{root: fcRoot}
	return root, filepath.Join(fcRoot, snapshotWorkDir)
}

func assertNoDirCreated(t *testing.T, root, workDir string) {
	t.Helper()
	if _, err := os.Stat(workDir); !os.IsNotExist(err) {
		t.Fatalf("work dir %s exists after a rejected request (err=%v)", workDir, err)
	}
	if _, err := os.Stat(filepath.Join(root, "a", "b", "var")); !os.IsNotExist(err) {
		t.Fatalf("traversal target created under %s (err=%v)", root, err)
	}
}

func TestCreateSnapshot_TraversalSnapshotIDRejected(t *testing.T) {
	for _, id := range []string{"../../../../var/lib/kubelet", "..\\..\\etc", "a/b", "..", ""} {
		t.Run(id, func(t *testing.T) {
			fc := &fakeFirecracker{}
			srv := newServer(t, fc)
			root, workDir := traversalFCRoot(t, srv, fc)
			cli := newBufconnClient(t, srv)

			_, err := cli.CreateSnapshot(context.Background(), &setecgrpcv1.CreateSnapshotRequest{
				SandboxId:      "ns/s",
				SnapshotId:     id,
				StorageBackend: "local-disk",
				SourcePodUid:   testPodUID,
			})
			if s, _ := status.FromError(err); s.Code() != codes.InvalidArgument {
				t.Fatalf("code = %v, want InvalidArgument (err=%v)", s.Code(), err)
			}
			if fc.pauseCalls != 0 {
				t.Fatalf("VM paused %d times for a rejected snapshot_id", fc.pauseCalls)
			}
			assertNoDirCreated(t, root, workDir)
		})
	}
}

func TestRestoreSandbox_TraversalSnapshotIDRejected(t *testing.T) {
	fc := &fakeFirecracker{}
	srv := newServer(t, fc)
	root, workDir := traversalFCRoot(t, srv, fc)
	// A real saved snapshot, so the only thing wrong with the request
	// is the snapshot_id.
	if _, _, err := srv.Storage.Save(context.Background(), "snap-1", framed(t, "STATE", "MEM")); err != nil {
		t.Fatalf("Save: %v", err)
	}
	cli := newBufconnClient(t, srv)

	_, err := cli.RestoreSandbox(context.Background(), &setecgrpcv1.RestoreSandboxRequest{
		SnapshotId:     "../../../../var/lib/kubelet",
		StorageRef:     "snap-1",
		StorageBackend: "local-disk",
		TargetPodUid:   testPodUID,
	})
	if s, _ := status.FromError(err); s.Code() != codes.InvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument (err=%v)", s.Code(), err)
	}
	assertNoDirCreated(t, root, workDir)
}

// CreateDiffSnapshot takes the path of a full snapshot here.
func (f *fakeFirecracker) CreateDiffSnapshot(ctx context.Context, state, mem string) error {
	return f.CreateSnapshot(ctx, state, mem)
}

// TestCreateSnapshot_LeavePausedKeepsTheMachinePaused proves that a
// suspend checkpoint does not resume the machine (setec#193).
func TestCreateSnapshot_LeavePausedKeepsTheMachinePaused(t *testing.T) {
	fc := &fakeFirecracker{}
	srv := newServer(t, fc)
	cli := newBufconnClient(t, srv)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := cli.CreateSnapshot(ctx, &setecgrpcv1.CreateSnapshotRequest{
		SandboxId: "ns/s", SnapshotId: "snap-1", StorageBackend: "local-disk", SourcePodUid: testPodUID, LeavePaused: true,
	}); err != nil {
		t.Fatal(err)
	}
	if fc.pauseCalls == 0 || fc.resumeOK {
		t.Fatalf("pause=%d resume=%v; want a paused machine", fc.pauseCalls, fc.resumeOK)
	}
}

// TestRestoreSandbox_StagesForTheLauncher proves the RPC path of a
// restore: the node agent stages the snapshot for the launcher of the
// target Pod and reports its evidence and the encryption of the store.
func TestRestoreSandbox_StagesForTheLauncher(t *testing.T) {
	fc := &fakeFirecracker{}
	srv := newServer(t, fc)
	keys := t.TempDir()
	srv.Storage = &storage.EncryptedBackend{
		Inner: &storage.LocalDiskBackend{Root: t.TempDir()},
		KEK:   &storage.FileKEKSource{Path: filepath.Join(keys, "node.key")},
		DEKs:  &storage.DirDEKStore{Dir: filepath.Join(keys, "deks")},
	}
	ctx := context.Background()
	if _, _, err := srv.Storage.Save(ctx, "snap-r", framed(t, "STATE", "MEMORY")); err != nil {
		t.Fatalf("Save: %v", err)
	}
	p, err := srv.Machines.Resolve(ctx, testPodUID)
	if err != nil {
		t.Fatal(err)
	}
	saw := fakeLauncher(t, p, podspec.RestoreEvidence{EntropyReseeded: true, Uniquified: true, ClockSet: true})
	resp, err := newBufconnClient(t, srv).RestoreSandbox(ctx, &setecgrpcv1.RestoreSandboxRequest{
		SnapshotId: "snap-r", StorageRef: "snap-r", StorageBackend: "local-disk", TargetPodUid: testPodUID,
	})
	if err != nil || !resp.GetSuccess() || !resp.GetEncryptedAtRest() || !resp.GetEntropyReseeded() {
		t.Fatalf("Restore = %+v, %v", resp, err)
	}
	if got := <-saw; got != "STATE" {
		t.Fatalf("the launcher read the state %q", got)
	}
}
