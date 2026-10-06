// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"

	"github.com/mdlayher/vsock"

	"github.com/zeroroot-ai/setec/internal/guestagent"
	"github.com/zeroroot-ai/setec/internal/launcher"
	"github.com/zeroroot-ai/setec/internal/uniquify"
)

// workloadIdentity is where a new identity goes after a snapshot load. In
// a launcher machine the workload runs in the root of the image, so the
// machine-id, the hostname and the boot-id go there. Elsewhere the agent
// and the workload share one root.
func workloadIdentity(pid1 bool) *uniquify.LinuxIdentity {
	id := uniquify.NewLinuxIdentity()
	if pid1 {
		id.MachineIDPath = filepath.Join(guestagent.NewRoot, id.MachineIDPath)
		id.HostnamePath = filepath.Join(guestagent.NewRoot, id.HostnamePath)
		id.BootIDProcPath = filepath.Join(guestagent.NewRoot, id.BootIDProcPath)
	}
	return id
}

// runSupervisor is the launcher machine mode (docs/design/runtime.md). As
// PID 1 the agent prepares the root of the image, reaps orphans, and serves
// the launcher on the control port. A machine of today's backends never
// runs the agent as PID 1, so it never enters this mode.
//
// prepareMachine runs first, before any listener: as PID 1 the agent must
// mount devtmpfs before /dev/vsock exists. The first real boot found that
// order.
func prepareMachine() error {
	if err := guestagent.PrepareRoot(guestagent.KernelArg("setec.lowerfs")); err != nil {
		return fmt.Errorf("prepare the root: %w", err)
	}
	// Ctrl-Alt-Del from the launcher then reaches the agent as SIGINT, and
	// the agent ends the machine itself (endMachine). The first real boot
	// showed that a stop otherwise took the full grace period now and then.
	return syscall.Reboot(syscall.LINUX_REBOOT_CMD_CAD_OFF)
}

// endMachine flushes the writable layer and resets the machine. With the
// boot argument reboot=k, Firecracker then exits. PID 1 must not simply
// exit: the kernel panics when init ends.
func endMachine() {
	syscall.Sync()
	_ = syscall.Reboot(syscall.LINUX_REBOOT_CMD_RESTART)
}

func runSupervisor(ctx context.Context, logf func(string, ...any)) error {
	sup := guestagent.NewSupervisor(guestagent.NewRoot)
	ln, err := vsock.Listen(guestagent.ControlPort, nil)
	if err != nil {
		return fmt.Errorf("listen on vsock port %d: %w", guestagent.ControlPort, err)
	}
	srv := &guestagent.Server{
		Sup:       sup,
		AllowPeer: fromHost,
		ReportExit: func(code int) error {
			c, err := vsock.Dial(guestagent.HostCID, launcher.ExitPort, nil)
			if err != nil {
				return err
			}
			defer func() { _ = c.Close() }()
			return guestagent.WriteLine(c, launcher.ExitReport{ExitCode: code})
		},
		Net:     guestagent.LinkConfigurer{Root: guestagent.NewRoot},
		Console: os.Stdout,
		Logf:    logf,
	}
	logf("setec-guest-agent: supervisor ready on vsock port %d", guestagent.ControlPort)
	if err := srv.Serve(ctx, ln); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

// fromHost allows a connection from the host (vsock CID 2) only. A process
// inside the machine that connects over vsock loopback has a different
// CID, and the agent listens on no IP address.
func fromHost(c net.Conn) bool {
	a, ok := c.RemoteAddr().(*vsock.Addr)
	return ok && a.ContextID == guestagent.HostCID
}
