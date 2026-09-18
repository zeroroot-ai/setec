// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestInstallTo_CopiesExecutable(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "setec")
	if err := installTo(dir); err != nil {
		t.Fatalf("installTo: %v", err)
	}
	dst := filepath.Join(dir, binaryName)
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("stat %s: %v", dst, err)
	}
	if info.Mode()&0o111 == 0 {
		t.Fatalf("%s is not executable: %v", dst, info.Mode())
	}
	self, _ := os.Executable()
	want, _ := os.ReadFile(self)
	got, _ := os.ReadFile(dst)
	if !bytes.Equal(want, got) {
		t.Fatalf("installed file differs from the running executable (%d vs %d bytes)", len(got), len(want))
	}
	if _, err := os.Stat(dst + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("temporary file left behind (err=%v)", err)
	}
}

func TestRun_ExitsOnTerm(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGINT} {
		sigs := make(chan os.Signal, 2)
		sigs <- syscall.SIGCHLD
		sigs <- sig
		done := make(chan struct{})
		go func() { run(sigs); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("run did not return on %v", sig)
		}
	}
}

func TestReap_CollectsExitedChild(t *testing.T) {
	const bin = "/bin/true"
	if _, err := os.Stat(bin); err != nil {
		t.Skipf("%s not present: %v", bin, err)
	}
	pid, err := syscall.ForkExec(bin, []string{"true"}, &syscall.ProcAttr{Env: os.Environ()})
	if err != nil {
		t.Fatalf("ForkExec: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		reap()
		_, werr := syscall.Wait4(pid, nil, syscall.WNOHANG, nil)
		if errors.Is(werr, syscall.ECHILD) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("child %d was not reaped", pid)
}
