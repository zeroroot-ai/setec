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
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	setecgrpcv1 "github.com/zeroroot-ai/setec/api/grpc/v1"
	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
	"github.com/zeroroot-ai/setec/internal/firecracker"
	"github.com/zeroroot-ai/setec/internal/nodeagent/pool"
	"github.com/zeroroot-ai/setec/internal/snapshot/storage"
)

// fakeFirecracker records calls and optionally returns errors.
type fakeFirecracker struct {
	mu         sync.Mutex
	pauseCalls int
	resumeOK   bool
	loadCalls  []string
	createOK   bool

	pauseErr  error
	createErr error
	loadErr   error
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
	_ = os.WriteFile(state, []byte("STATE"), 0o600)
	_ = os.WriteFile(mem, []byte("MEMORY-PAYLOAD"), 0o600)
	f.createOK = true
	return nil
}
func (f *fakeFirecracker) LoadSnapshot(_ context.Context, state, mem string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.loadErr != nil {
		return f.loadErr
	}
	f.loadCalls = append(f.loadCalls, state+"|"+mem)
	return nil
}

// newServer wires a Server with a LocalDiskBackend rooted in a
// tempdir and the provided fakeFirecracker.
func newServer(t *testing.T, fc *fakeFirecracker, p *pool.Manager) *Server {
	t.Helper()
	backend := &storage.LocalDiskBackend{Root: t.TempDir()}
	return &Server{
		Storage:            backend,
		FirecrackerFactory: func(_ string) firecracker.Client { return fc },
		Pool:               p,
		TempDir:            t.TempDir(),
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
	srv := newServer(t, fc, nil)
	cli := newBufconnClient(t, srv)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := cli.CreateSnapshot(ctx, &setecgrpcv1.CreateSnapshotRequest{
		SandboxId:        "ns/s",
		SnapshotId:       "snap-1",
		StorageBackend:   "local-disk",
		SourceKataSocket: "/tmp/fc.sock",
	})
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	if resp.StorageRef != "snap-1" {
		t.Fatalf("storage_ref = %q", resp.StorageRef)
	}
	if resp.SizeBytes != int64(frameHeaderSize+len("STATE")+len("MEMORY-PAYLOAD")) {
		t.Fatalf("size = %d", resp.SizeBytes)
	}
	if fc.pauseCalls == 0 || !fc.createOK || !fc.resumeOK {
		t.Fatalf("firecracker state: pause=%d create=%v resume=%v", fc.pauseCalls, fc.createOK, fc.resumeOK)
	}

	// Verify round-trip: Open the ref and confirm the framed stream
	// decodes back to STATE + MEMORY-PAYLOAD.
	rc, err := srv.Storage.Open(ctx, "snap-1")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	all, _ := io.ReadAll(rc)
	_ = rc.Close()
	// The first 16 bytes are the framing header.
	if string(all[frameHeaderSize:frameHeaderSize+5]) != "STATE" {
		t.Fatalf("state bytes wrong: %q", all[frameHeaderSize:frameHeaderSize+5])
	}
}

func TestCreateSnapshot_MissingSnapshotID(t *testing.T) {
	fc := &fakeFirecracker{}
	cli := newBufconnClient(t, newServer(t, fc, nil))
	_, err := cli.CreateSnapshot(context.Background(), &setecgrpcv1.CreateSnapshotRequest{
		SourceKataSocket: "/s",
	})
	if s, _ := status.FromError(err); s.Code() != codes.InvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument", s.Code())
	}
}

func TestCreateSnapshot_MissingSocket(t *testing.T) {
	fc := &fakeFirecracker{}
	cli := newBufconnClient(t, newServer(t, fc, nil))
	_, err := cli.CreateSnapshot(context.Background(), &setecgrpcv1.CreateSnapshotRequest{
		SnapshotId: "s",
	})
	if s, _ := status.FromError(err); s.Code() != codes.InvalidArgument {
		t.Fatalf("code = %v", s.Code())
	}
}

func TestCreateSnapshot_PauseErrorPropagates(t *testing.T) {
	fc := &fakeFirecracker{pauseErr: errors.New("already paused")}
	cli := newBufconnClient(t, newServer(t, fc, nil))
	_, err := cli.CreateSnapshot(context.Background(), &setecgrpcv1.CreateSnapshotRequest{
		SnapshotId: "s", SourceKataSocket: "/s",
	})
	if s, _ := status.FromError(err); s.Code() != codes.Internal {
		t.Fatalf("code = %v", s.Code())
	}
}

func TestCreateSnapshot_InsufficientStorage(t *testing.T) {
	fc := &fakeFirecracker{}
	srv := newServer(t, fc, nil)
	// Swap backend for one that always returns ErrInsufficientStorage.
	srv.Storage = &stubBackend{saveErr: storage.ErrInsufficientStorage}
	cli := newBufconnClient(t, srv)
	_, err := cli.CreateSnapshot(context.Background(), &setecgrpcv1.CreateSnapshotRequest{
		SnapshotId: "x", SourceKataSocket: "/s",
	})
	if s, _ := status.FromError(err); s.Code() != codes.ResourceExhausted {
		t.Fatalf("code = %v, want ResourceExhausted", s.Code())
	}
}

func TestRestoreSandbox_Happy(t *testing.T) {
	fc := &fakeFirecracker{}
	srv := newServer(t, fc, nil)
	cli := newBufconnClient(t, srv)
	ctx := context.Background()

	// Save a framed payload in storage so Restore can open it.
	framed := makeFramedPayload(t, []byte("STATE-BYTES"), []byte("MEM-BYTES"))
	if _, _, err := srv.Storage.Save(ctx, "snap-r", bytes.NewReader(framed)); err != nil {
		t.Fatalf("Save: %v", err)
	}
	resp, err := cli.RestoreSandbox(ctx, &setecgrpcv1.RestoreSandboxRequest{
		SnapshotId:       "snap-r",
		StorageRef:       "snap-r",
		StorageBackend:   "local-disk",
		KataSocketTarget: "/tmp/fc-target.sock",
	})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if !resp.Success {
		t.Fatalf("success = false: %q", resp.Error)
	}
	if len(fc.loadCalls) != 1 {
		t.Fatalf("LoadSnapshot calls = %d", len(fc.loadCalls))
	}
	// The bare LocalDiskBackend does not attest encryption at rest, so
	// the signal for the operator-side invariant gate must be false —
	// never inferred (ADR-0005 invariant 5).
	if resp.GetEncryptedAtRest() {
		t.Fatal("encrypted_at_rest must be false for an unencrypted backend")
	}
}

// TestRestoreSandbox_ReportsEncryptedAtRest pins that the response
// signal tracks the storage backend's AtRestReporter capability: with
// the EncryptedBackend wrapper (the only production write path) the
// node attests encryption at rest per restore.
func TestRestoreSandbox_ReportsEncryptedAtRest(t *testing.T) {
	fc := &fakeFirecracker{}
	srv := newServer(t, fc, nil)
	keys := t.TempDir()
	srv.Storage = &storage.EncryptedBackend{
		Inner: &storage.LocalDiskBackend{Root: t.TempDir()},
		KEK:   &storage.FileKEKSource{Path: filepath.Join(keys, "node.key")},
		DEKs:  &storage.DirDEKStore{Dir: filepath.Join(keys, "deks")},
	}
	cli := newBufconnClient(t, srv)
	ctx := context.Background()

	framed := makeFramedPayload(t, []byte("STATE-BYTES"), []byte("MEM-BYTES"))
	if _, _, err := srv.Storage.Save(ctx, "snap-enc", bytes.NewReader(framed)); err != nil {
		t.Fatalf("Save: %v", err)
	}
	resp, err := cli.RestoreSandbox(ctx, &setecgrpcv1.RestoreSandboxRequest{
		SnapshotId:       "snap-enc",
		StorageRef:       "snap-enc",
		StorageBackend:   "local-disk",
		KataSocketTarget: "/tmp/fc-target.sock",
	})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if !resp.GetEncryptedAtRest() {
		t.Fatal("encrypted_at_rest must be true when serving through the EncryptedBackend")
	}
}

func TestRestoreSandbox_MissingArgs(t *testing.T) {
	fc := &fakeFirecracker{}
	cli := newBufconnClient(t, newServer(t, fc, nil))
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
	cli := newBufconnClient(t, newServer(t, fc, nil))
	_, err := cli.RestoreSandbox(context.Background(), &setecgrpcv1.RestoreSandboxRequest{
		SnapshotId: "ghost", StorageRef: "ghost", KataSocketTarget: "/s",
	})
	if s, _ := status.FromError(err); s.Code() != codes.NotFound {
		t.Fatalf("code = %v", s.Code())
	}
}

func TestRestoreSandbox_Corrupted(t *testing.T) {
	fc := &fakeFirecracker{}
	srv := newServer(t, fc, nil)
	srv.Storage = &stubBackend{openErr: storage.ErrCorrupted}
	cli := newBufconnClient(t, srv)
	_, err := cli.RestoreSandbox(context.Background(), &setecgrpcv1.RestoreSandboxRequest{
		SnapshotId: "s", StorageRef: "r", KataSocketTarget: "/s",
	})
	if s, _ := status.FromError(err); s.Code() != codes.DataLoss {
		t.Fatalf("code = %v, want DataLoss", s.Code())
	}
}

func TestRestoreSandbox_LoadSnapshotError(t *testing.T) {
	fc := &fakeFirecracker{loadErr: errors.New("fc load failed")}
	srv := newServer(t, fc, nil)
	cli := newBufconnClient(t, srv)
	ctx := context.Background()
	framed := makeFramedPayload(t, []byte("S"), []byte("M"))
	if _, _, err := srv.Storage.Save(ctx, "snap-b", bytes.NewReader(framed)); err != nil {
		t.Fatalf("Save: %v", err)
	}
	_, err := cli.RestoreSandbox(ctx, &setecgrpcv1.RestoreSandboxRequest{
		SnapshotId: "snap-b", StorageRef: "snap-b", KataSocketTarget: "/s",
	})
	if s, _ := status.FromError(err); s.Code() != codes.Internal {
		t.Fatalf("code = %v", s.Code())
	}
}

func TestPauseSandbox_Happy(t *testing.T) {
	fc := &fakeFirecracker{}
	cli := newBufconnClient(t, newServer(t, fc, nil))
	resp, err := cli.PauseSandbox(context.Background(), &setecgrpcv1.PauseSandboxRequest{
		SandboxId: "ns/s", KataSocketTarget: "/s",
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
	cli := newBufconnClient(t, newServer(t, &fakeFirecracker{}, nil))
	_, err := cli.PauseSandbox(context.Background(), &setecgrpcv1.PauseSandboxRequest{})
	if s, _ := status.FromError(err); s.Code() != codes.InvalidArgument {
		t.Fatalf("code = %v", s.Code())
	}
}

func TestPauseSandbox_FirecrackerError(t *testing.T) {
	fc := &fakeFirecracker{pauseErr: errors.New("nope")}
	cli := newBufconnClient(t, newServer(t, fc, nil))
	_, err := cli.PauseSandbox(context.Background(), &setecgrpcv1.PauseSandboxRequest{
		KataSocketTarget: "/s",
	})
	if s, _ := status.FromError(err); s.Code() != codes.Internal {
		t.Fatalf("code = %v", s.Code())
	}
}

func TestResumeSandbox_Happy(t *testing.T) {
	fc := &fakeFirecracker{}
	cli := newBufconnClient(t, newServer(t, fc, nil))
	resp, err := cli.ResumeSandbox(context.Background(), &setecgrpcv1.ResumeSandboxRequest{
		KataSocketTarget: "/s",
	})
	if err != nil || !resp.Success {
		t.Fatalf("Resume: %v %v", err, resp)
	}
}

func TestResumeSandbox_MissingSocket(t *testing.T) {
	cli := newBufconnClient(t, newServer(t, &fakeFirecracker{}, nil))
	_, err := cli.ResumeSandbox(context.Background(), &setecgrpcv1.ResumeSandboxRequest{})
	if s, _ := status.FromError(err); s.Code() != codes.InvalidArgument {
		t.Fatalf("code = %v", s.Code())
	}
}

func TestQueryPool_Empty(t *testing.T) {
	// No pool wired — returns empty response.
	cli := newBufconnClient(t, newServer(t, &fakeFirecracker{}, nil))
	resp, err := cli.QueryPool(context.Background(), &setecgrpcv1.QueryPoolRequest{SandboxClass: "x"})
	if err != nil {
		t.Fatalf("QueryPool: %v", err)
	}
	if len(resp.Entries) != 0 {
		t.Fatalf("entries = %d, want 0", len(resp.Entries))
	}
}

func TestQueryPool_ReturnsEntries(t *testing.T) {
	// Wire up a pool with one entry.
	pm := pool.New(
		&storage.LocalDiskBackend{Root: t.TempDir()},
		noPrefetch{},
		func(_ string) firecracker.Client { return &fakeFirecracker{} },
		"node-x",
	)
	pm.MaxConcurrentBoots = 2
	pm.PoolStorageRoot = t.TempDir()
	// Keep socket paths under the test's temp dir — never the
	// production /run/kata-containers default (#119).
	pm.SocketPattern = filepath.Join(t.TempDir(), "pool-%s", "firecracker.socket")
	pm.Launcher = noopLauncher{}
	// Use ReconcilePools to seed the pool through the public API.
	cls := setecv1alpha1.SandboxClass{
		ObjectMeta: metav1.ObjectMeta{Name: "std"},
		Spec: setecv1alpha1.SandboxClassSpec{
			VMM: setecv1alpha1.VMMFirecracker, PreWarmPoolSize: 1, PreWarmImage: "img:v1",
		},
	}
	if err := pm.ReconcilePools(context.Background(), []setecv1alpha1.SandboxClass{cls}); err != nil {
		t.Fatalf("seed pool: %v", err)
	}

	srv := newServer(t, &fakeFirecracker{}, pm)
	cli := newBufconnClient(t, srv)
	resp, err := cli.QueryPool(context.Background(), &setecgrpcv1.QueryPoolRequest{SandboxClass: "std"})
	if err != nil {
		t.Fatalf("QueryPool: %v", err)
	}
	if len(resp.Entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(resp.Entries))
	}
	e := resp.Entries[0]
	if e.ImageRef != "img:v1" || !e.Available {
		t.Fatalf("entry fields: %#v", e)
	}
}

// --- helpers ------------------------------------------------------

// noopLauncher satisfies pool.Launcher without doing any work. The
// pool Manager needs a Launcher instance to reconcile; test entries
// are purely in-memory and do not require a real Firecracker boot.
type noopLauncher struct{}

func (noopLauncher) Launch(_ context.Context, _ pool.LaunchOptions) error { return nil }

// noPrefetch is a zero-behavior ImagePrefetcher used to seed tests
// without making the pool call out to an imagecache.
type noPrefetch struct{}

func (noPrefetch) Prefetch(_ context.Context, _ []string) error { return nil }

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

// makeFramedPayload constructs a 16-byte-framed state+memory payload
// matching the server's on-disk format.
func makeFramedPayload(t *testing.T, state, mem []byte) []byte {
	t.Helper()
	tmp := filepath.Join(t.TempDir(), "framed")
	sp := tmp + ".state"
	mp := tmp + ".mem"
	if err := os.WriteFile(sp, state, 0o600); err != nil {
		t.Fatalf("write state: %v", err)
	}
	if err := os.WriteFile(mp, mem, 0o600); err != nil {
		t.Fatalf("write mem: %v", err)
	}
	rc, err := makeFramedReader(sp, mp)
	if err != nil {
		t.Fatalf("makeFramedReader: %v", err)
	}
	defer func() { _ = rc.Close() }()
	raw, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("readAll: %v", err)
	}
	return raw
}

// --- entropy reseed enforcement (setec#72) --------------------------

// recordingReseeder implements entropy.Reseeder for tests.
type recordingReseeder struct {
	mu    sync.Mutex
	paths []string
	err   error
}

func (r *recordingReseeder) Reseed(_ context.Context, udsPath string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.paths = append(r.paths, udsPath)
	return r.err
}

func (r *recordingReseeder) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.paths...)
}

func TestRestoreSandbox_ReseedSuccessIsReported(t *testing.T) {
	fc := &fakeFirecracker{}
	srv := newServer(t, fc, nil)
	rs := &recordingReseeder{}
	var outcomes []string
	srv.Reseeder = rs
	srv.ReseedObserver = func(outcome string) { outcomes = append(outcomes, outcome) }
	cli := newBufconnClient(t, srv)
	ctx := context.Background()

	framed := makeFramedPayload(t, []byte("S"), []byte("M"))
	if _, _, err := srv.Storage.Save(ctx, "snap-e", bytes.NewReader(framed)); err != nil {
		t.Fatalf("Save: %v", err)
	}
	resp, err := cli.RestoreSandbox(ctx, &setecgrpcv1.RestoreSandboxRequest{
		SnapshotId:       "snap-e",
		StorageRef:       "snap-e",
		KataSocketTarget: "/run/kata-containers/pod-1/firecracker.socket",
	})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if !resp.Success || !resp.EntropyReseeded {
		t.Fatalf("expected success + entropy_reseeded, got %+v", resp)
	}
	if len(rs.seen()) == 0 {
		t.Fatal("reseeder was never invoked")
	}
	if len(outcomes) != 1 || outcomes[0] != "success" {
		t.Fatalf("observer outcomes = %v", outcomes)
	}
}

// TestRestoreSandbox_ReseedFailureFailsClosed is the core fail-closed
// contract of setec#72: when the guest cannot confirm the reseed, the
// restore RPC must fail (so the sandbox is never reported Ready) and
// the restored-but-unreseeded VM must be paused rather than left
// running with cloned RNG state.
func TestRestoreSandbox_ReseedFailureFailsClosed(t *testing.T) {
	fc := &fakeFirecracker{}
	srv := newServer(t, fc, nil)
	rs := &recordingReseeder{err: errors.New("guest never acked")}
	var outcomes []string
	srv.Reseeder = rs
	srv.ReseedObserver = func(outcome string) { outcomes = append(outcomes, outcome) }
	cli := newBufconnClient(t, srv)
	ctx := context.Background()

	framed := makeFramedPayload(t, []byte("S"), []byte("M"))
	if _, _, err := srv.Storage.Save(ctx, "snap-f", bytes.NewReader(framed)); err != nil {
		t.Fatalf("Save: %v", err)
	}
	_, err := cli.RestoreSandbox(ctx, &setecgrpcv1.RestoreSandboxRequest{
		SnapshotId:       "snap-f",
		StorageRef:       "snap-f",
		KataSocketTarget: "/run/kata-containers/pod-2/firecracker.socket",
	})
	if err == nil {
		t.Fatal("restore must FAIL when the reseed cannot be confirmed")
	}
	if s, _ := status.FromError(err); s.Code() != codes.Internal {
		t.Fatalf("code = %v, want Internal", s.Code())
	}
	fc.mu.Lock()
	pauses := fc.pauseCalls
	fc.mu.Unlock()
	if pauses == 0 {
		t.Fatal("the unreseeded VM must be paused (not left running with cloned RNG state)")
	}
	if len(outcomes) != 1 || outcomes[0] != "failure" {
		t.Fatalf("observer outcomes = %v", outcomes)
	}
}

// TestRestoreSandbox_NilReseederSkipsActiveReseed pins the explicit
// opt-out shape (--entropy-reseed=off): restore succeeds but
// entropy_reseeded is reported false so callers can see the passive-
// only posture.
func TestRestoreSandbox_NilReseederSkipsActiveReseed(t *testing.T) {
	fc := &fakeFirecracker{}
	srv := newServer(t, fc, nil)
	cli := newBufconnClient(t, srv)
	ctx := context.Background()

	framed := makeFramedPayload(t, []byte("S"), []byte("M"))
	if _, _, err := srv.Storage.Save(ctx, "snap-n", bytes.NewReader(framed)); err != nil {
		t.Fatalf("Save: %v", err)
	}
	resp, err := cli.RestoreSandbox(ctx, &setecgrpcv1.RestoreSandboxRequest{
		SnapshotId:       "snap-n",
		StorageRef:       "snap-n",
		KataSocketTarget: "/s",
	})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if !resp.Success {
		t.Fatalf("success = false: %q", resp.Error)
	}
	if resp.EntropyReseeded {
		t.Fatal("entropy_reseeded must be false when no reseeder is configured")
	}
}

func TestDefaultReseedVsockPaths(t *testing.T) {
	in := &setecgrpcv1.RestoreSandboxRequest{
		StorageRef:       "/var/lib/setec/pool/entry-1",
		KataSocketTarget: "/run/kata-containers/pod-uid/firecracker.socket",
	}
	got := defaultReseedVsockPaths(in)
	want := []string{
		"/var/lib/setec/pool/entry-1/vsock.sock",
		"/run/kata-containers/pod-uid/vsock.sock",
	}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("candidates = %v, want %v", got, want)
	}

	// A non-absolute storage ref (opaque backend id) contributes no
	// filesystem candidate.
	in.StorageRef = "ns-snap"
	got = defaultReseedVsockPaths(in)
	if len(got) != 1 || got[0] != want[1] {
		t.Fatalf("candidates = %v, want only the kata-socket sibling", got)
	}
}
