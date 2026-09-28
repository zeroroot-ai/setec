// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package main

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

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
// and chowns it to uid:gid so the sandbox user can write to it after
// dropPrivileges.
func formatWorkspace(device, target string, uid, gid int) error {
	if err := waitForDevice(device, deviceWaitTimeout); err != nil {
		return fmt.Errorf("wait for workspace device: %w", err)
	}
	if err := workspace.FormatAndMount(device, target); err != nil {
		return err
	}
	if err := os.Chown(target, uid, gid); err != nil {
		return fmt.Errorf("chown %s to %d:%d: %w", target, uid, gid, err)
	}
	return nil
}

// dropPrivileges makes this process the unprivileged sandbox user,
// uid:gid with gid as its only group, and sets no_new_privs. A kata-fc
// session's wrapper runs as root only for the format-and-mount step
// (setec#91). The setuid(2) from root to a non-zero UID clears the
// permitted and effective capability sets, so the Sandbox's command,
// or the reap loop, runs with no capability. no_new_privs then stops
// the command from regaining one through a setuid or file-capability
// binary, which Kubernetes cannot promise for this container: it
// refuses allowPrivilegeEscalation: false next to CAP_SYS_ADMIN.
//
// Go applies each call to every thread of the process (Go 1.16+).
func dropPrivileges(uid, gid int) error {
	if os.Getuid() != uid || os.Getgid() != gid {
		if err := syscall.Setgroups([]int{gid}); err != nil {
			return fmt.Errorf("setgroups [%d]: %w", gid, err)
		}
		if err := syscall.Setgid(gid); err != nil {
			return fmt.Errorf("setgid %d: %w", gid, err)
		}
		if err := syscall.Setuid(uid); err != nil {
			return fmt.Errorf("setuid %d: %w", uid, err)
		}
	}
	return setNoNewPrivs()
}

// setNoNewPrivs sets no_new_privs on every thread. prctl(2) acts on the
// calling thread only, and the Go scheduler may run the later exec on
// another one. AllThreadsSyscall reaches them all, but only in a
// binary built without cgo, which the static keepalive image is. With
// cgo (a test binary, for example) it returns ENOTSUP, and the fallback
// pins this goroutine to its thread for the rest of the process, so the
// exec runs on the thread that holds the flag.
func setNoNewPrivs() error {
	_, _, errno := syscall.AllThreadsSyscall(syscall.SYS_PRCTL, unix.PR_SET_NO_NEW_PRIVS, 1, 0)
	if errno == 0 {
		return nil
	}
	if errno != syscall.ENOTSUP {
		return fmt.Errorf("set no_new_privs: %w", errno)
	}
	runtime.LockOSThread()
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("set no_new_privs: %w", err)
	}
	return nil
}

// execInto replaces the current process with args[0] (resolved against
// PATH), passing args as its argv and the current environment. On
// success it never returns.
//
// This is how a kata-fc session hands off from the format/mount step to
// the Sandbox's own command (setec#91) without leaving a supervisor
// process in between: the Sandbox's command becomes PID 1 directly,
// exactly as it would on any other backend. dropPrivileges runs first,
// so the command starts as the sandbox user with no capability.
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

// ext4SuperMagic is statfs(2)'s f_type for an ext2/3/4 filesystem.
const ext4SuperMagic = 0xEF53

// workspaceReady is the --workspace-ready readiness probe of a kata-fc
// session (setec#91). The container starts, and so the Pod reports
// Running, before the wrapper has formatted and mounted the workspace
// device over the emptyDir at dir. A write in that window lands in the
// emptyDir, and the mount then hides it for good. The Sandbox reports
// Running only once this probe passes, which is once dir is the ext4
// filesystem on the durable device.
func workspaceReady(dir string) error {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return fmt.Errorf("statfs %s: %w", dir, err)
	}
	if st.Type != ext4SuperMagic {
		return fmt.Errorf("%s is not the mounted workspace yet (filesystem type 0x%x, want ext4 0x%x)", dir, st.Type, ext4SuperMagic)
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
