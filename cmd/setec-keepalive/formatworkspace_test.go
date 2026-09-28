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

// TestDropPrivileges_SameIdentitySetsNoNewPrivs asserts that a process
// already running as the target user skips setuid and still sets
// no_new_privs, which the Pod spec cannot promise for this container.
func TestDropPrivileges_SameIdentitySetsNoNewPrivs(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("runs as a non-root user; root would really change identity")
	}
	if err := dropPrivileges(os.Getuid(), os.Getgid()); err != nil {
		t.Fatalf("dropPrivileges(own uid/gid) = %v, want nil", err)
	}
	// This goroutine's thread: the flag is per thread, and the cgo
	// fallback pins this goroutine to the thread that set it.
	status, err := os.ReadFile("/proc/thread-self/status")
	if err != nil {
		t.Fatalf("read /proc/thread-self/status: %v", err)
	}
	if !strings.Contains(string(status), "NoNewPrivs:\t1") {
		t.Errorf("NoNewPrivs is not 1 after dropPrivileges:\n%s", status)
	}
}

// TestDropPrivileges_NonRootCannotBecomeAnotherUser asserts that the
// drop fails loudly, not silently, when the process lacks the
// capabilities to change identity.
func TestDropPrivileges_NonRootCannotBecomeAnotherUser(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("runs as a non-root user; root holds CAP_SETUID")
	}
	if err := dropPrivileges(os.Getuid()+1, os.Getgid()+1); err == nil {
		t.Fatal("dropPrivileges to another uid/gid as non-root = nil, want an error")
	}
}
