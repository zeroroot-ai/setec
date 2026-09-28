// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

// Command setec-keepalive is the boot command of a session Sandbox that
// declares no spec.command (setec#7, ADR-0006), and also the command
// every kata-fc session boots first to prepare its durable workspace
// before running its own command, if it has one (setec#91).
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
// The same static binary is also the workload command of every kata-fc
// session (setec#91), because kata-fc's workspace PVC is a raw block
// device (Firecracker has no virtio-fs) and something has to turn it
// into a mounted filesystem before the session's own command can use
// it:
//
//	setec-keepalive --format-workspace-device DEV --format-workspace-target DIR \
//	  --format-workspace-uid UID --format-workspace-gid GID [-- CMD ARGS...]
//
// formats DEV as ext4 — only if it is not already formatted, since a
// session's workspace PVC re-attaches to a fresh Pod on every VM
// restart, and reformatting it would silently destroy the workload's
// corpus — mounts it at DIR, chowns it to UID:GID, drops to UID:GID, and then either execs
// CMD (a session with its own spec.command) or, with no trailing
// command, falls into the same reap loop as a plain `setec-keepalive`
// invocation (a session with none). Doing the format, mount and exec
// from inside the one container that will run the workload — rather
// than a separate init container sharing its mount via Bidirectional
// propagation — is what lets this run without `privileged: true`, which
// the chart's own admission policy forbids in a Sandbox namespace
// (setec#159): Kubernetes requires a privileged container for
// Bidirectional propagation, but a container mounting into its own
// mount namespace, for its own later exec, needs no propagation at all.
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
	uid := flag.Int("format-workspace-uid", -1, "chown the mounted workspace to this uid and run as it afterwards, required with --format-workspace-device")
	gid := flag.Int("format-workspace-gid", -1, "chown the mounted workspace to this gid and run as it afterwards, required with --format-workspace-device")
	ready := flag.String("workspace-ready", "", "exit 0 only if DIR is the mounted ext4 workspace; the kata-fc readiness probe")
	flag.Parse()

	if *ready != "" {
		if err := workspaceReady(*ready); err != nil {
			fmt.Fprintln(os.Stderr, "setec-keepalive:", err)
			os.Exit(1)
		}
		return
	}

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
		if err := dropPrivileges(*uid, *gid); err != nil {
			fmt.Fprintln(os.Stderr, "setec-keepalive:", err)
			os.Exit(1)
		}
		if cmdArgs := flag.Args(); len(cmdArgs) > 0 {
			// The Sandbox declared its own command: hand off to it. On
			// success execInto never returns — the Sandbox's command
			// replaces this process as PID 1, exactly as it would
			// without the workspace-format step on any other backend.
			if err := execInto(cmdArgs); err != nil {
				fmt.Fprintln(os.Stderr, "setec-keepalive:", err)
				os.Exit(1)
			}
		}
		// No trailing command: this is a session that declared none
		// (setec#7), so fall into the same reap loop below as a plain
		// `setec-keepalive` invocation.
	}

	sigs := make(chan os.Signal, 16)
	signal.Notify(sigs, syscall.SIGCHLD, syscall.SIGTERM, syscall.SIGINT)
	run(sigs)
}
