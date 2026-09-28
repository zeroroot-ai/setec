// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

// execIntoHelperEnv, when set, tells this test binary's own process to
// act as the TestExecInto_ReplacesProcess helper instead of running the
// normal test suite. syscall.Exec replaces the calling process, so the
// only way to observe its effect is from a separate process — the same
// pattern os/exec's own tests use.
const execIntoHelperEnv = "SETEC_KEEPALIVE_EXECINTO_HELPER"

func TestExecInto_ReplacesProcess(t *testing.T) {
	if os.Getenv(execIntoHelperEnv) == "1" {
		// Re-exec'd helper: call the real thing and let it replace us.
		if err := execInto([]string{"/bin/echo", "hello-from-execInto"}); err != nil {
			os.Exit(9)
		}
		os.Exit(9) // execInto only returns on error.
	}

	cmd := exec.Command(os.Args[0], "-test.run=TestExecInto_ReplacesProcess")
	cmd.Env = append(os.Environ(), execIntoHelperEnv+"=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helper process: %v, output=%s", err, out)
	}
	if got := string(out); !strings.Contains(got, "hello-from-execInto") {
		t.Fatalf("helper process output = %q, want it to contain %q", got, "hello-from-execInto")
	}
}

func TestExecInto_UnknownCommandErrors(t *testing.T) {
	if err := execInto([]string{"setec-keepalive-test-does-not-exist"}); err == nil {
		t.Fatalf("execInto(unknown command) = nil error, want an error")
	}
}
