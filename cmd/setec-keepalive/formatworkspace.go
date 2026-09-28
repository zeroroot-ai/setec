// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/zeroroot-ai/setec/internal/workspace"
)

// deviceWaitTimeout bounds how long formatWorkspace waits for the
// workspace block device to appear before giving up. Kubernetes
// attaches a Pod's VolumeDevices before starting any container, but the
// guest kernel enumerating the virtio-blk device kata attached is not
// synchronized with container start the way a host bind mount is.
const deviceWaitTimeout = 30 * time.Second

// devicePollInterval is how often waitForDevice re-checks.
const devicePollInterval = 200 * time.Millisecond

// formatWorkspace is the --format-workspace-device entry point
// (setec#91). Kata Containers + Firecracker has no virtio-fs, so a
// session's durable workspace PVC reaches the guest as a raw block
// device instead of a mounted filesystem; this formats it as ext4 —
// only if it is not already formatted, so a session VM that restarts
// against the same PVC never loses its corpus — mounts it at target,
// and chowns it to uid:gid so the unprivileged workload container can
// write to it.
func formatWorkspace(device, target string, uid, gid int) error {
	if err := waitForDevice(device, deviceWaitTimeout); err != nil {
		return fmt.Errorf("wait for workspace device: %w", err)
	}
	if err := workspace.FormatAndMount(device, target); err != nil {
		logCapabilityDiagnostics(device)
		return err
	}
	if err := os.Chown(target, uid, gid); err != nil {
		return fmt.Errorf("chown %s to %d:%d: %w", target, uid, gid, err)
	}
	return nil
}

// logCapabilityDiagnostics prints this process's own capability sets
// (from /proc/self/status) and the device's mode/owner to stderr. Only
// called on a FormatAndMount failure — a temporary, low-cost aid for
// diagnosing setec#91's "open ...: permission denied" on a real
// kata-fc cluster, where CAP_DAC_OVERRIDE is granted in the Pod spec
// but the guest kernel's actual behavior needs confirming empirically.
func logCapabilityDiagnostics(device string) {
	if status, err := os.ReadFile("/proc/self/status"); err == nil {
		for _, line := range strings.Split(string(status), "\n") {
			if strings.HasPrefix(line, "Cap") || strings.HasPrefix(line, "Uid") || strings.HasPrefix(line, "Gid") {
				fmt.Fprintln(os.Stderr, "setec-keepalive: diagnostics:", line)
			}
		}
	}
	if fi, err := os.Stat(device); err == nil {
		fmt.Fprintf(os.Stderr, "setec-keepalive: diagnostics: %s mode=%s\n", device, fi.Mode())
		if st, ok := fi.Sys().(*syscall.Stat_t); ok {
			fmt.Fprintf(os.Stderr, "setec-keepalive: diagnostics: %s uid=%d gid=%d rdev=%d\n", device, st.Uid, st.Gid, st.Rdev)
		}
	} else {
		fmt.Fprintln(os.Stderr, "setec-keepalive: diagnostics: stat", device, "failed:", err)
	}
}

// execInto replaces the current process with args[0] (resolved against
// PATH), passing args as its argv and the current environment. On
// success it never returns.
//
// This is how a kata-fc session hands off from the format/mount step to
// the Sandbox's own command (setec#91) without leaving a supervisor
// process in between: the Sandbox's command becomes PID 1 directly,
// exactly as it would on any other backend. It also means any
// capability this process held (CAP_SYS_ADMIN, for the mount(2) call)
// does not reach the Sandbox's command: per the Linux capability model,
// an exec'd binary with no file capabilities of its own — true of an
// ordinary user command — starts with an empty effective/permitted
// capability set regardless of what the exec'ing process held, unless
// the exec'ing process populated its ambient set, which nothing here
// does.
func execInto(args []string) error {
	path, err := exec.LookPath(args[0])
	if err != nil {
		return fmt.Errorf("look up %s: %w", args[0], err)
	}
	if err := syscall.Exec(path, args, os.Environ()); err != nil { //nolint:gosec // args come from the operator-built Pod spec, not external input
		return fmt.Errorf("exec %s: %w", path, err)
	}
	return nil
}

// waitForDevice polls until path exists, or returns an error once
// timeout elapses. Any stat error other than "not found" is returned
// immediately.
func waitForDevice(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if _, err := os.Stat(path); err == nil {
			return nil
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("stat %s: %w", path, err)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("device %s did not appear within %s", path, timeout)
		}
		time.Sleep(devicePollInterval)
	}
}
