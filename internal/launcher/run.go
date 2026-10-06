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
	"strings"
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

// Start says how a machine started.
type Start int

// The ways a machine starts.
const (
	// Booted is a machine that booted its kernel.
	Booted Start = iota
	// Loaded is a machine that loaded a snapshot. The guest gets fresh
	// randomness before anything else.
	Loaded
	// LoadedNoReseed is a machine that loaded a snapshot while the node
	// agent runs with --entropy-reseed=off. The guest gets no fresh
	// randomness, the machine stays off the Pod network, and the evidence
	// says so. The operator gate then refuses the restore.
	LoadedNoReseed
)

// StagedNoReseed is the content of the staged marker when the node agent
// runs with --entropy-reseed=off. An empty marker asks for a reseed.
const StagedNoReseed = "entropy-reseed=off"

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
	// Format makes the file system of a new writable layer. Nil uses
	// MkfsExt4.
	Format Formatter
	// CheckDisk checks the signature of the image disk before the machine
	// uses it. The disk comes from the image volume of the Pod. Nil checks
	// nothing, which only a test does.
	CheckDisk func(s *Spec) error
	// AfterStart runs once the machine runs, with the identity of the Pod.
	// The guest agent of setec#189 applies the address there, and after a
	// snapshot load also the time of the node. Nil does nothing.
	AfterStart func(ctx context.Context, pn PodNet, start Start) error
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
	// The kubelet pulled the disk into the image volume of the Pod on the
	// node, outside the Pod network. The launcher reaches no registry.
	if l.CheckDisk != nil {
		if err := l.CheckDisk(l.Spec); err != nil {
			return LaunchFailedExit, fail(ReasonDisks, fmt.Errorf("check the image disk: %w", err))
		}
	}
	// A restore waits for the node agent: it stages the state, the memory
	// and the writable layer, so the disks are ready only after that.
	snap := l.Spec.Source.Snapshot
	start := Booted
	if snap != nil {
		start = Loaded
	}
	if snap != nil && snap.Staged != "" {
		wctx, cancel := context.WithTimeout(ctx, stagedWait)
		err := waitFile(wctx, snap.Staged)
		cancel()
		if err != nil {
			return LaunchFailedExit, l.failRestore(fail(ReasonSourceFailed, fmt.Errorf("wait for the staged snapshot: %w", err)))
		}
		marker, err := os.ReadFile(snap.Staged)
		if err != nil {
			return LaunchFailedExit, l.failRestore(fail(ReasonSourceFailed, fmt.Errorf("read the staged marker: %w", err)))
		}
		if strings.TrimSpace(string(marker)) == StagedNoReseed {
			start = LoadedNoReseed
		}
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
	format := l.Format
	if format == nil {
		format = MkfsExt4
	}
	if err := l.Spec.prepareDisks(format); err != nil {
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

	if snap == nil {
		// A booted machine has nothing to hide: it joins the Pod network
		// before the guest agent configures it.
		if err := l.Net.Connect(); err != nil {
			return LaunchFailedExit, fail(ReasonNetwork, err)
		}
	} else if err := l.VMM.LoadSnapshot(ctx, l.Spec.WorkDir, snap.State, snap.Memory); err != nil {
		return LaunchFailedExit, l.failRestore(fail(ReasonSourceFailed, err))
	}
	if l.AfterStart != nil {
		if err := l.AfterStart(ctx, pn, start); err != nil {
			return LaunchFailedExit, l.failRestore(fail(ReasonSourceFailed, err))
		}
	}
	switch start {
	case Loaded:
		// A loaded machine joins the Pod network only after the guest
		// agent confirmed fresh randomness, the clock and a new identity.
		// Until then its frames reach nothing (setec#105).
		if err := l.Net.Connect(); err != nil {
			return LaunchFailedExit, l.failRestore(fail(ReasonNetwork, err))
		}
		if err := writeEvidence(snap.Evidence, RestoreEvidence{EntropyReseeded: true, Uniquified: true, ClockSet: true}); err != nil {
			return LaunchFailedExit, fail(ReasonSourceFailed, err)
		}
	case LoadedNoReseed:
		// No fresh randomness, so no network. The evidence tells the
		// operator gate, which refuses the restore.
		if err := writeEvidence(snap.Evidence, RestoreEvidence{Uniquified: true, ClockSet: true}); err != nil {
			return LaunchFailedExit, fail(ReasonSourceFailed, err)
		}
	case Booted:
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

// failRestore writes the failure of a restore to the evidence file, so the
// node agent ends its wait at once, and returns err.
func (l *Launcher) failRestore(err error) error {
	if snap := l.Spec.Source.Snapshot; snap != nil {
		_ = writeEvidence(snap.Evidence, RestoreEvidence{Error: err.Error()})
	}
	return err
}

// writeEvidence writes ev to path through a temporary file, so the node
// agent never reads a partial file. An empty path writes nothing.
func writeEvidence(path string, ev RestoreEvidence) error {
	if path == "" {
		return nil
	}
	raw, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// stagedPoll is the interval at which the launcher looks for the staged
// marker of a snapshot, and stagedWait the longest wait. A restore that the
// node agent never stages ends the Pod.
const (
	stagedPoll = 50 * time.Millisecond
	stagedWait = 5 * time.Minute
)

// waitFile waits until path exists or ctx ends.
func waitFile(ctx context.Context, path string) error {
	for {
		if _, err := os.Stat(path); err == nil {
			return nil
		} else if !os.IsNotExist(err) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(stagedPoll):
		}
	}
}
