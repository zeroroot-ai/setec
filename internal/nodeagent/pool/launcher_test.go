// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package pool

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakePoolVM writes an executable script that stands in for
// setec-pool-vm, prints output, and exits with code.
func fakePoolVM(t *testing.T, output string, code int) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "setec-pool-vm")
	script := "#!/bin/sh\necho '" + output + "'\nexit " + string(rune('0'+code)) + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

func launchOpts() LaunchOptions {
	return LaunchOptions{
		ClassName: "std", EntryID: "e1",
		SocketPath: "/run/x/firecracker.socket", StorageRoot: "/var/lib/setec/pool",
	}
}

// TestExecLauncher_RunsTheBinary asserts that a launch actually runs
// setec-pool-vm. The launcher once set Stdout before calling
// CombinedOutput, which refuses that, so every launch failed with
// "exec: Stdout already set" and no pool entry ever booted (setec#19).
func TestExecLauncher_RunsTheBinary(t *testing.T) {
	l := &ExecLauncher{BinaryPath: fakePoolVM(t, "booted", 0)}
	if err := l.Launch(context.Background(), launchOpts()); err != nil {
		t.Fatalf("Launch: %v", err)
	}
}

// TestExecLauncher_FailureCarriesTheOutput asserts that a failed launch
// reports the child's output.
func TestExecLauncher_FailureCarriesTheOutput(t *testing.T) {
	l := &ExecLauncher{BinaryPath: fakePoolVM(t, "kvm unavailable", 3)}
	err := l.Launch(context.Background(), launchOpts())
	if err == nil || !strings.Contains(err.Error(), "kvm unavailable") {
		t.Fatalf("Launch = %v, want an error carrying the child's output", err)
	}
}
