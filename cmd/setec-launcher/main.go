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

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/zeroroot-ai/setec/internal/diskbuilder"
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
		FetchDisk: func(ctx context.Context, s *launcher.Spec) error {
			keys, err := diskbuilder.ParsePublicKeys(s.DiskKeys)
			if err != nil {
				return err
			}
			// A registry that is not reachable yet at Pod start is retried.
			// A disk with a bad signature is not: it stays bad.
			var ferr error
			for attempt := range 6 {
				ferr = diskbuilder.Fetch(ctx, s.DiskRepo, s.ImageRef, s.ImageDisk, keys,
					remote.WithAuthFromKeychain(authn.DefaultKeychain))
				if ferr == nil || errors.Is(ferr, diskbuilder.ErrBadSignature) {
					return ferr
				}
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(time.Duration(1<<attempt) * time.Second):
				}
			}
			return ferr
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
