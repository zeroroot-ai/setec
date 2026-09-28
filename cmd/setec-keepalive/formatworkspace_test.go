// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWaitForDevice_AlreadyPresent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dev")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := waitForDevice(path, time.Second); err != nil {
		t.Fatalf("waitForDevice: %v", err)
	}
}

func TestWaitForDevice_AppearsLate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dev")
	go func() {
		time.Sleep(50 * time.Millisecond)
		_ = os.WriteFile(path, nil, 0o600)
	}()
	if err := waitForDevice(path, 2*time.Second); err != nil {
		t.Fatalf("waitForDevice: %v", err)
	}
}

func TestWaitForDevice_TimesOut(t *testing.T) {
	path := filepath.Join(t.TempDir(), "never-appears")
	if err := waitForDevice(path, 100*time.Millisecond); err == nil {
		t.Fatalf("waitForDevice: got nil error, want a timeout error")
	}
}
