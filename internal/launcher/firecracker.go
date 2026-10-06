// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package launcher

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/zeroroot-ai/setec/internal/firecracker"
)

// FirecrackerVMM runs the firecracker binary as a child of the launcher.
type FirecrackerVMM struct {
	// Binary is the path of the firecracker executable.
	Binary string

	cmd     *exec.Cmd
	socket  string
	waitErr error
	done    chan struct{}
	once    sync.Once
}

// Start implements VMM.
func (f *FirecrackerVMM) Start(ctx context.Context, workDir, configFile string, console io.Writer) error {
	f.socket = filepath.Join(workDir, APISocket)
	_ = os.Remove(f.socket)
	args := []string{"--api-sock", APISocket}
	if configFile != "" {
		args = append(args, "--config-file", filepath.Base(configFile))
	}
	// Not CommandContext: a stop of the Pod goes through Stop, which gives
	// the guest its grace period first.
	f.cmd = exec.Command(f.Binary, args...) //nolint:gosec // the binary is a flag of the launcher
	f.cmd.Dir = workDir
	f.cmd.Stdout = console
	f.cmd.Stderr = console
	if err := f.cmd.Start(); err != nil {
		return fmt.Errorf("start firecracker: %w", err)
	}
	f.done = make(chan struct{})
	go func() {
		f.waitErr = f.cmd.Wait()
		close(f.done)
	}()
	// The API socket appears when Firecracker is ready for calls.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(f.socket); err == nil {
			return nil
		}
		select {
		case <-f.done:
			return fmt.Errorf("firecracker ended at start: %w", f.waitErr)
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
	return errors.New("the firecracker API socket did not appear in 5s")
}

// LoadSnapshot implements VMM.
func (f *FirecrackerVMM) LoadSnapshot(ctx context.Context, _, state, memory string) error {
	return firecracker.LoadSnapshotTrackingDirtyPages(ctx, f.socket, state, memory)
}

// Wait implements VMM.
func (f *FirecrackerVMM) Wait() error {
	<-f.done
	return f.waitErr
}

// Stop implements VMM.
func (f *FirecrackerVMM) Stop(grace time.Duration) error {
	var err error
	f.once.Do(func() {
		if f.cmd == nil || f.cmd.Process == nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = firecracker.SendCtrlAltDel(ctx, f.socket)
		cancel()
		select {
		case <-f.done:
		case <-time.After(grace):
			err = f.cmd.Process.Kill()
			<-f.done
		}
		_ = os.Remove(f.socket)
	})
	return err
}
