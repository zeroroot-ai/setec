// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package main

import (
	"fmt"
	"os"
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
		return err
	}
	if err := os.Chown(target, uid, gid); err != nil {
		return fmt.Errorf("chown %s to %d:%d: %w", target, uid, gid, err)
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
