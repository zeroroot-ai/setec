/*
Copyright 2026 The Setec Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/zeroroot-ai/setec/internal/firecracker"
)

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

type fakeProcess struct {
	mu       sync.Mutex
	pid      int
	signals  []os.Signal
	exited   atomic.Bool
	waitCh   chan struct{}
	waitErr  error
	onSignal func(os.Signal)
}

func newFakeProcess(pid int) *fakeProcess {
	return &fakeProcess{pid: pid, waitCh: make(chan struct{})}
}

func (p *fakeProcess) Signal(sig os.Signal) error {
	p.mu.Lock()
	p.signals = append(p.signals, sig)
	onSig := p.onSignal
	p.mu.Unlock()
	if onSig != nil {
		onSig(sig)
	}
	// SIGKILL always terminates.
	if sig == syscall.SIGKILL {
		p.finish(nil)
	}
	return nil
}

func (p *fakeProcess) finish(err error) {
	if p.exited.CompareAndSwap(false, true) {
		p.mu.Lock()
		p.waitErr = err
		p.mu.Unlock()
		close(p.waitCh)
	}
}

func (p *fakeProcess) Wait() error {
	<-p.waitCh
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.waitErr
}

func (p *fakeProcess) Pid() int { return p.pid }

func (p *fakeProcess) sentSignal(sig os.Signal) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Contains(p.signals, sig)
}

type fakeSpawner struct {
	socketPath     string
	startErr       error
	startCalled    atomic.Int32
	produceSocket  bool
	onStart        func()
	handler        http.Handler
	process        *fakeProcess
	listener       net.Listener
	serverShutdown chan struct{}
}

func (s *fakeSpawner) Start(ctx context.Context, binary string, args []string) (SpawnedProcess, error) {
	s.startCalled.Add(1)
	if s.onStart != nil {
		s.onStart()
	}
	if s.startErr != nil {
		return nil, s.startErr
	}

	// If asked, stand up a Unix-domain HTTP server at the socket path so
	// waitForSocket and the extraClient bring-up calls succeed.
	if s.produceSocket {
		_ = os.Remove(s.socketPath)
		ln, err := net.Listen("unix", s.socketPath)
		if err != nil {
			return nil, fmt.Errorf("fake: listen unix: %w", err)
		}
		s.listener = ln
		s.serverShutdown = make(chan struct{})
		go func() {
			defer close(s.serverShutdown)
			server := &http.Server{Handler: s.handler, ReadHeaderTimeout: 5 * time.Second}
			_ = server.Serve(ln)
		}()
	}

	s.process = newFakeProcess(4242)
	return s.process, nil
}

func (s *fakeSpawner) closeListener() {
	if s.listener != nil {
		_ = s.listener.Close()
	}
}

// defaultHandler satisfies every Firecracker bring-up endpoint with 204.
func defaultHandler() http.Handler {
	mux := http.NewServeMux()
	ok := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }
	mux.HandleFunc("/boot-source", ok)
	mux.HandleFunc("/drives/rootfs", ok)
	mux.HandleFunc("/machine-config", ok)
	mux.HandleFunc("/actions", ok)
	return mux
}

type fakeFC struct {
	pauseErr    error
	snapshotErr error
	// If snapshotWriter is non-nil, the fake writes sentinel bytes to the
	// state+mem paths so verifySnapshotFiles passes.
	snapshotWriter func(state, mem string) error

	pauseCalled    atomic.Int32
	snapshotCalled atomic.Int32
}

func (f *fakeFC) Pause(_ context.Context) error {
	f.pauseCalled.Add(1)
	return f.pauseErr
}
func (f *fakeFC) Resume(_ context.Context) error { return nil }
func (f *fakeFC) CreateSnapshot(_ context.Context, statePath, memPath string) error {
	f.snapshotCalled.Add(1)
	if f.snapshotWriter != nil {
		if err := f.snapshotWriter(statePath, memPath); err != nil {
			return err
		}
	}
	return f.snapshotErr
}
func (f *fakeFC) LoadSnapshot(_ context.Context, _, _ string) error { return nil }

func goodSnapshotWriter(state, mem string) error {
	if err := os.WriteFile(state, []byte("fake-state-data"), 0o644); err != nil {
		return err
	}
	return os.WriteFile(mem, []byte("fake-memory-data"), 0o644)
}

func zeroSnapshotWriter(state, mem string) error {
	if err := os.WriteFile(state, []byte{}, 0o644); err != nil {
		return err
	}
	return os.WriteFile(mem, []byte{}, 0o644)
}

func tempOpts(t *testing.T) Options {
	t.Helper()
	root := t.TempDir()
	socketDir := t.TempDir()
	opts := Options{
		ImageRef:           "docker.io/library/alpine:3",
		KernelPath:         filepath.Join(root, "vmlinux"),
		RootfsPath:         filepath.Join(root, "rootfs.ext4"),
		VCPUs:              1,
		MemoryMiB:          256,
		SocketPath:         filepath.Join(socketDir, "fc.sock"),
		StorageRoot:        filepath.Join(root, "pool"),
		PoolEntryID:        "entry-one",
		FirecrackerBinary:  "/bin/true",
		BootReadyTimeout:   2 * time.Second,
		ShutdownGracePause: 100 * time.Millisecond,
		BootArgs:           "console=ttyS0",
	}
	_ = os.WriteFile(opts.KernelPath, []byte{0}, 0o644)
	_ = os.WriteFile(opts.RootfsPath, []byte{0}, 0o644)
	return opts
}

func newSpawnerWithSocket(opts Options) *fakeSpawner {
	return &fakeSpawner{
		socketPath:    opts.SocketPath,
		produceSocket: true,
		handler:       defaultHandler(),
	}
}

func factoryReturning(f *fakeFC) ClientFactory {
	return func(_ string) firecracker.Client { return f }
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestRunLauncher_HappyPath(t *testing.T) {
	opts := tempOpts(t)
	spawner := newSpawnerWithSocket(opts)
	defer spawner.closeListener()
	fc := &fakeFC{snapshotWriter: goodSnapshotWriter}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := runLauncher(ctx, opts, spawner, factoryReturning(fc)); err != nil {
		t.Fatalf("runLauncher: unexpected error: %v", err)
	}

	// Firecracker should still be "alive" (no SIGTERM/SIGKILL sent).
	if spawner.process == nil {
		t.Fatal("process not started")
	}
	if spawner.process.sentSignal(syscall.SIGTERM) || spawner.process.sentSignal(syscall.SIGKILL) {
		t.Fatalf("happy path must not terminate firecracker: %+v", spawner.process.signals)
	}

	// Snapshot files present and non-empty.
	for _, f := range []string{stateFileName, memFileName} {
		fi, err := os.Stat(filepath.Join(opts.StorageRoot, opts.PoolEntryID, f))
		if err != nil {
			t.Fatalf("missing %s: %v", f, err)
		}
		if fi.Size() == 0 {
			t.Fatalf("%s is empty", f)
		}
	}
	if fc.pauseCalled.Load() != 1 {
		t.Fatalf("expected exactly one Pause call, got %d", fc.pauseCalled.Load())
	}
	if fc.snapshotCalled.Load() != 1 {
		t.Fatalf("expected exactly one CreateSnapshot call, got %d", fc.snapshotCalled.Load())
	}
}

func TestRunLauncher_SpawnFailure(t *testing.T) {
	opts := tempOpts(t)
	spawner := &fakeSpawner{socketPath: opts.SocketPath, startErr: errors.New("exec: not found")}
	fc := &fakeFC{}

	ctx := context.Background()
	err := runLauncher(ctx, opts, spawner, factoryReturning(fc))
	if err == nil {
		t.Fatal("expected error from spawn failure")
	}
	// Entry dir must not exist (cleanup) or must be empty.
	if _, statErr := os.Stat(filepath.Join(opts.StorageRoot, opts.PoolEntryID)); !os.IsNotExist(statErr) {
		t.Fatalf("partial entry dir not cleaned: %v", statErr)
	}
}

func TestRunLauncher_SocketTimeout(t *testing.T) {
	opts := tempOpts(t)
	opts.BootReadyTimeout = 150 * time.Millisecond

	// Spawner starts but never creates the socket.
	spawner := &fakeSpawner{socketPath: opts.SocketPath, produceSocket: false}
	defer spawner.closeListener()
	fc := &fakeFC{}

	ctx := context.Background()
	err := runLauncher(ctx, opts, spawner, factoryReturning(fc))
	if err == nil {
		t.Fatal("expected error when socket never becomes ready")
	}
	if spawner.process == nil || !spawner.process.sentSignal(syscall.SIGTERM) {
		t.Fatal("firecracker must be SIGTERM'd on socket timeout")
	}
	if _, statErr := os.Stat(filepath.Join(opts.StorageRoot, opts.PoolEntryID)); !os.IsNotExist(statErr) {
		t.Fatalf("entry dir should be cleaned on socket timeout")
	}
}

func TestRunLauncher_PauseFailure(t *testing.T) {
	opts := tempOpts(t)
	spawner := newSpawnerWithSocket(opts)
	defer spawner.closeListener()
	fc := &fakeFC{pauseErr: errors.New("pause refused")}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	err := runLauncher(ctx, opts, spawner, factoryReturning(fc))
	if err == nil {
		t.Fatal("expected error on Pause failure")
	}
	if spawner.process == nil || !spawner.process.sentSignal(syscall.SIGTERM) {
		t.Fatal("firecracker must be SIGTERM'd when Pause fails")
	}
	if _, statErr := os.Stat(filepath.Join(opts.StorageRoot, opts.PoolEntryID)); !os.IsNotExist(statErr) {
		t.Fatalf("entry dir should be cleaned on pause failure")
	}
}

func TestRunLauncher_SnapshotFailure(t *testing.T) {
	opts := tempOpts(t)
	spawner := newSpawnerWithSocket(opts)
	defer spawner.closeListener()
	fc := &fakeFC{snapshotErr: errors.New("disk full")}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	err := runLauncher(ctx, opts, spawner, factoryReturning(fc))
	if err == nil {
		t.Fatal("expected error on snapshot failure")
	}
	if !spawner.process.sentSignal(syscall.SIGTERM) {
		t.Fatal("firecracker must be SIGTERM'd on snapshot failure")
	}
	if _, statErr := os.Stat(filepath.Join(opts.StorageRoot, opts.PoolEntryID)); !os.IsNotExist(statErr) {
		t.Fatalf("entry dir should be cleaned on snapshot failure")
	}
}

func TestRunLauncher_ZeroLengthSnapshot(t *testing.T) {
	opts := tempOpts(t)
	spawner := newSpawnerWithSocket(opts)
	defer spawner.closeListener()
	fc := &fakeFC{snapshotWriter: zeroSnapshotWriter}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	err := runLauncher(ctx, opts, spawner, factoryReturning(fc))
	if err == nil {
		t.Fatal("expected zero-length snapshot to be rejected")
	}
	if !spawner.process.sentSignal(syscall.SIGTERM) {
		t.Fatal("firecracker must be SIGTERM'd when snapshot files are empty")
	}
}

func TestRunLauncher_BringUpHTTPError(t *testing.T) {
	opts := tempOpts(t)
	// Responder returns 500 for /actions.
	mux := http.NewServeMux()
	ok := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }
	mux.HandleFunc("/boot-source", ok)
	mux.HandleFunc("/drives/rootfs", ok)
	mux.HandleFunc("/machine-config", ok)
	mux.HandleFunc("/actions", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"fault_message":"InstanceStart refused"}`, http.StatusBadRequest)
	})
	spawner := &fakeSpawner{
		socketPath:    opts.SocketPath,
		produceSocket: true,
		handler:       mux,
	}
	defer spawner.closeListener()

	fc := &fakeFC{snapshotWriter: goodSnapshotWriter}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	err := runLauncher(ctx, opts, spawner, factoryReturning(fc))
	if err == nil {
		t.Fatal("expected error when /actions returns 4xx")
	}
	if !spawner.process.sentSignal(syscall.SIGTERM) {
		t.Fatal("firecracker must be SIGTERM'd when bring-up fails")
	}
}

func TestParseFlags_MissingRequired(t *testing.T) {
	_, err := parseFlags([]string{"--kernel-path", "/k"})
	if err == nil {
		t.Fatal("expected error listing missing required flags")
	}
}

func TestParseFlags_HappyPath(t *testing.T) {
	args := []string{
		"--kernel-path", "/k",
		"--rootfs-path", "/r",
		"--socket-path", "/tmp/fc.sock",
		"--storage-root", "/tmp/pool",
		"--pool-entry-id", "abc",
	}
	o, err := parseFlags(args)
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if o.PoolEntryID != "abc" {
		t.Fatalf("got %+v", o)
	}
}
