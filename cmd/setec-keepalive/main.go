// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

// Command setec-keepalive is the boot command of a session Sandbox that
// declares no spec.command (setec#7, ADR-0006), and also carries the
// workspace-format init container the kata-fc backend runs to prepare
// a session's durable workspace (setec#91).
//
// A session's microVM lives as long as its boot process. When that
// process is the caller's work, the session ends the moment the work
// does, and the next Exec finds nothing to enter. The keepalive is a
// boot process that never finishes on its own: it sleeps on signals,
// reaps every orphaned child an Exec leaves behind, and exits only on
// SIGTERM or SIGINT, which is how the operator tears a session down.
//
// The binary is static. The operator ships it into the Sandbox Pod
// with an init container that runs `setec-keepalive --install DIR`,
// which copies the executable into a shared volume, so the session
// never depends on a shell or a sleep binary in the user's image.
//
// The same static binary also runs as the workspace-format init
// container on kata-fc: `setec-keepalive --format-workspace-device DEV
// --format-workspace-target DIR` formats DEV as ext4 (only if it is not
// already formatted — a session's workspace PVC re-attaches to a fresh
// Pod on every VM restart, and reformatting it would silently destroy
// the workload's corpus) and mounts it at DIR. Reusing this binary
// means a kata-fc session never depends on a shell or mkfs/mount
// binaries in the user's image either.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	install := flag.String("install", "", "copy this executable into DIR as "+binaryName+" and exit")
	device := flag.String("format-workspace-device", "",
		"format this block device as ext4 (only if unformatted) and mount it at --format-workspace-target")
	target := flag.String("format-workspace-target", "", "mount point for --format-workspace-device")
	uid := flag.Int("format-workspace-uid", -1, "chown the mounted workspace to this uid, required with --format-workspace-device")
	gid := flag.Int("format-workspace-gid", -1, "chown the mounted workspace to this gid, required with --format-workspace-device")
	flag.Parse()

	if *install != "" {
		if err := installTo(*install); err != nil {
			fmt.Fprintln(os.Stderr, "setec-keepalive:", err)
			os.Exit(1)
		}
		return
	}

	if *device != "" {
		if *target == "" || *uid < 0 || *gid < 0 {
			fmt.Fprintln(os.Stderr,
				"setec-keepalive: --format-workspace-device requires --format-workspace-target, "+
					"--format-workspace-uid and --format-workspace-gid")
			os.Exit(2)
		}
		if err := formatWorkspace(*device, *target, *uid, *gid); err != nil {
			fmt.Fprintln(os.Stderr, "setec-keepalive:", err)
			os.Exit(1)
		}
		return
	}

	sigs := make(chan os.Signal, 16)
	signal.Notify(sigs, syscall.SIGCHLD, syscall.SIGTERM, syscall.SIGINT)
	run(sigs)
}
