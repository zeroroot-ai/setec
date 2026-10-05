// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package launcher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// Reason names why a launch failed. It is the first word of the
// termination message of the Pod, so the operator reads a typed reason.
type Reason string

// The failure reasons of a launch.
const (
	ReasonBadSpec      Reason = "BadSpec"
	ReasonNetwork      Reason = "NetworkJoinFailed"
	ReasonDisks        Reason = "DisksFailed"
	ReasonVMMStart     Reason = "VMMStartFailed"
	ReasonSourceFailed Reason = "SourceFailed"
	ReasonVMMExited    Reason = "VMMExitedWithoutReport"
)

// LaunchFailedExit is the exit code of the Pod when the machine never ran
// the workload. The termination message carries the Reason.
const LaunchFailedExit = 125

// Error is a launch failure with its typed reason.
type Error struct {
	Reason Reason
	Err    error
}

func (e *Error) Error() string { return fmt.Sprintf("%s: %v", e.Reason, e.Err) }
func (e *Error) Unwrap() error { return e.Err }

func fail(r Reason, err error) error { return &Error{Reason: r, Err: err} }

// VMM starts and stops one Firecracker process.
type VMM interface {
	// Start runs Firecracker in workDir. With configFile set it boots from
	// that file; without, it waits on its API socket for a snapshot load.
	// Its console goes to console.
	Start(ctx context.Context, workDir, configFile string, console io.Writer) error
	// LoadSnapshot loads state and memory through the API socket and
	// resumes the machine.
	LoadSnapshot(ctx context.Context, workDir, state, memory string) error
	// Wait returns when the Firecracker process has ended.
	Wait() error
	// Stop asks the guest to stop, waits up to grace, then kills the process.
	Stop(grace time.Duration) error
}

// Launcher runs one machine.
type Launcher struct {
	Spec    *Spec
	Net     Network
	VMM     VMM
	Console io.Writer
	// Grace bounds a clean stop of the guest before a kill.
	Grace time.Duration
	// AfterStart runs once the machine runs, with the identity of the Pod.
	// The guest agent of setec#189 applies the address there, and after a
	// snapshot load also the time of the node. Nil does nothing.
	AfterStart func(ctx context.Context, pn PodNet, fromSnapshot bool) error
}

// Run starts the machine, waits for the exit report of the guest, stops
// the machine and removes the network join. It returns the exit code of
// the workload. Each path out of Run leaves no process and no device.
func (l *Launcher) Run(ctx context.Context) (code int, err error) {
	if err := l.Spec.Validate(); err != nil {
		return LaunchFailedExit, fail(ReasonBadSpec, err)
	}
	if err := os.MkdirAll(l.Spec.WorkDir, 0o700); err != nil {
		return LaunchFailedExit, fail(ReasonDisks, err)
	}
	defer func() {
		if lerr := l.Net.Leave(); lerr != nil && err == nil {
			err = fmt.Errorf("remove the network join: %w", lerr)
		}
	}()
	pn, err := l.Net.Join()
	if err != nil {
		return LaunchFailedExit, fail(ReasonNetwork, err)
	}
	if err := l.Spec.prepareDisks(); err != nil {
		return LaunchFailedExit, fail(ReasonDisks, err)
	}
	exitL, err := listenExit(l.Spec.WorkDir)
	if err != nil {
		return LaunchFailedExit, fail(ReasonVMMStart, err)
	}
	defer func() { _ = exitL.Close() }()

	configFile := ""
	if b := l.Spec.Source.Boot; b != nil {
		configFile = filepath.Join(l.Spec.WorkDir, "vm.json")
		raw, err := json.Marshal(l.Spec.bootConfig(pn.MAC))
		if err != nil {
			return LaunchFailedExit, fail(ReasonVMMStart, err)
		}
		if err := os.WriteFile(configFile, raw, 0o600); err != nil {
			return LaunchFailedExit, fail(ReasonVMMStart, err)
		}
	}
	if err := l.VMM.Start(ctx, l.Spec.WorkDir, configFile, l.Console); err != nil {
		return LaunchFailedExit, fail(ReasonVMMStart, err)
	}
	stopped := false
	stop := func() {
		if !stopped {
			stopped = true
			_ = l.VMM.Stop(l.Grace)
		}
	}
	defer stop()

	if s := l.Spec.Source.Snapshot; s != nil {
		if err := l.VMM.LoadSnapshot(ctx, l.Spec.WorkDir, s.State, s.Memory); err != nil {
			return LaunchFailedExit, fail(ReasonSourceFailed, err)
		}
	}
	if l.AfterStart != nil {
		if err := l.AfterStart(ctx, pn, l.Spec.Source.Snapshot != nil); err != nil {
			return LaunchFailedExit, fail(ReasonSourceFailed, err)
		}
	}

	// The exit report ends the run. A Firecracker process that ends with
	// no report, and a stop of the Pod, end it too.
	waitCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	vmmDone := make(chan error, 1)
	go func() { vmmDone <- l.VMM.Wait() }()
	reportC := make(chan ExitReport, 1)
	errC := make(chan error, 1)
	go func() {
		r, err := waitExit(waitCtx, exitL)
		if err != nil {
			errC <- err
			return
		}
		reportC <- r
	}()
	select {
	case r := <-reportC:
		stop()
		return r.ExitCode, nil
	case werr := <-vmmDone:
		stopped = true
		// A late report can still be in the socket.
		select {
		case r := <-reportC:
			return r.ExitCode, nil
		case <-time.After(200 * time.Millisecond):
		}
		return LaunchFailedExit, fail(ReasonVMMExited, errors.Join(errors.New("firecracker ended before the guest reported an exit"), werr))
	case <-ctx.Done():
		stop()
		return LaunchFailedExit, ctx.Err()
	case err := <-errC:
		stop()
		return LaunchFailedExit, fail(ReasonVMMExited, err)
	}
}
