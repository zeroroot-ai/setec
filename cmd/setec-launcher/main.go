// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

// Command setec-launcher is the one container of a launcher Pod. It starts
// one Firecracker machine from the spec that the operator writes, sends
// the machine console to stdout so that kubectl logs shows it, and exits
// with the exit code of the workload (docs/design/runtime.md).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/zeroroot-ai/setec/internal/diskbuilder"
	"github.com/zeroroot-ai/setec/internal/guestagent"
	"github.com/zeroroot-ai/setec/internal/launcher"
)

func main() {
	var (
		specPath = flag.String("spec", "/etc/setec/launcher/spec.json", "the launcher spec that the operator writes")
		fcBinary = flag.String("firecracker", "/usr/local/bin/firecracker", "the firecracker executable")
		grace    = flag.Duration("grace", 10*time.Second, "how long a stopping guest may take before a kill")
		termLog  = flag.String("termination-log", "/dev/termination-log", "where the typed failure reason goes")
	)
	flag.Parse()
	// setec-launcher exec -- ARGV runs ARGV in the machine of this Pod. The
	// frontend runs it through pods/exec (docs/design/lifecycles.md).
	if flag.NArg() > 0 && flag.Arg(0) == "exec" {
		os.Exit(execInMachine(*specPath, flag.Args()[1:]))
	}
	os.Exit(run(*specPath, *fcBinary, *grace, *termLog))
}

func run(specPath, fcBinary string, grace time.Duration, termLog string) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	spec, err := launcher.ReadSpec(specPath)
	if err != nil {
		return report(termLog, &launcher.Error{Reason: launcher.ReasonBadSpec, Err: err})
	}
	guest := launcher.NewGuest(spec)
	l := &launcher.Launcher{
		Spec:       spec,
		Net:        launcher.TCNetwork{},
		VMM:        &launcher.FirecrackerVMM{Binary: fcBinary},
		Console:    os.Stdout,
		Grace:      grace,
		AfterStart: guest.AfterStart(spec.Workload),
		CheckDisk: func(s *launcher.Spec) error {
			keys, err := diskbuilder.ParsePublicKeys(s.DiskKeys)
			if err != nil {
				return err
			}
			return diskbuilder.VerifyMounted(s.ImageDisk, s.DiskSignature, s.ImageRef, keys)
		},
	}
	code, err := l.Run(ctx)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			fmt.Fprintln(os.Stderr, "setec-launcher: stopped")
			return code
		}
		return report(termLog, err)
	}
	return code
}

// report writes the typed reason to the termination message of the Pod and
// returns the exit code of a failed launch.
func report(termLog string, err error) int {
	msg := err.Error()
	if le, ok := errors.AsType[*launcher.Error](err); ok {
		msg = fmt.Sprintf("%s: %v", le.Reason, le.Err)
	}
	fmt.Fprintln(os.Stderr, "setec-launcher:", msg)
	_ = os.WriteFile(termLog, []byte(msg), 0o600) //nolint:gosec // the path is a flag of the launcher
	return launcher.LaunchFailedExit
}

// execInMachine relays one command to the guest agent and exits with its
// exit code. A relay failure exits 126, as a shell does for a command that
// cannot run.
func execInMachine(specPath string, args []string) int {
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "setec-launcher exec: no command")
		return 126
	}
	spec, err := launcher.ReadSpec(specPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "setec-launcher exec:", err)
		return 126
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	code, err := launcher.NewGuest(spec).Exec(ctx, guestagent.Process{Argv: args}, os.Stdin, os.Stdout, os.Stderr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "setec-launcher exec:", err)
		return 126
	}
	return code
}
