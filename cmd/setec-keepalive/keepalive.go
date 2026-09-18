// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// binaryName is the file name installTo writes. The pod builder runs
// the keepalive by this name under its mount path
// (internal/podspec.KeepalivePath).
const binaryName = "setec-keepalive"

// installTo copies the running executable into dir as binaryName. It
// writes to a temporary name first and renames, so a partial copy is
// never runnable.
func installTo(dir string) error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate executable: %w", err)
	}
	data, err := os.ReadFile(self)
	if err != nil {
		return fmt.Errorf("read executable: %w", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	dst := filepath.Join(dir, binaryName)
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, data, 0o755); err != nil { //nolint:gosec // an executable must be executable
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		return fmt.Errorf("rename %s: %w", tmp, err)
	}
	return nil
}

// run blocks on sigs. SIGCHLD reaps exited children. SIGTERM and SIGINT
// end the process with exit status 0. The loop does no polling: between
// signals the process sleeps in the channel receive.
func run(sigs <-chan os.Signal) int {
	for sig := range sigs {
		switch sig {
		case syscall.SIGCHLD:
			reap()
		case syscall.SIGTERM, syscall.SIGINT:
			return 0
		}
	}
	return 0
}

// reap collects every child that has exited. As PID 1 the keepalive is
// the parent of every orphan an Exec leaves behind; without this they
// stay as zombies for the life of the session.
func reap() {
	for {
		pid, err := syscall.Wait4(-1, nil, syscall.WNOHANG, nil)
		if pid <= 0 || errors.Is(err, syscall.ECHILD) {
			return
		}
	}
}
