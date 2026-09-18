// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

// Command setec-keepalive is the boot command of a session Sandbox that
// declares no spec.command (setec#7, ADR-0006).
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
	flag.Parse()

	if *install != "" {
		if err := installTo(*install); err != nil {
			fmt.Fprintln(os.Stderr, "setec-keepalive:", err)
			os.Exit(1)
		}
		return
	}

	sigs := make(chan os.Signal, 16)
	signal.Notify(sigs, syscall.SIGCHLD, syscall.SIGTERM, syscall.SIGINT)
	run(sigs)
}
