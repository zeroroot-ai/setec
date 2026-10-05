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

	"github.com/mdlayher/vsock"

	"github.com/zeroroot-ai/setec/internal/guestagent"
	"github.com/zeroroot-ai/setec/internal/launcher"
)

// runSupervisor is the launcher machine mode (docs/design/runtime.md). As
// PID 1 the agent prepares the root of the image, reaps orphans, and serves
// the launcher on the control port. A machine of today's backends never
// runs the agent as PID 1, so it never enters this mode.
func runSupervisor(ctx context.Context, logf func(string, ...any)) error {
	if err := guestagent.PrepareRoot(guestagent.KernelArg("setec.lowerfs")); err != nil {
		return fmt.Errorf("prepare the root: %w", err)
	}
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
