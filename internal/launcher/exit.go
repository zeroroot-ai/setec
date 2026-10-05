// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package launcher

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
)

// ExitReport is what the guest agent sends on ExitPort when the workload
// ends. It is one JSON line.
// A workload that a signal ended reports 128 plus the signal number, as a
// shell does.
type ExitReport struct {
	ExitCode int `json:"exitCode"`
}

// listenExit listens on the host side of ExitPort. Firecracker connects a
// guest call to host port P to the socket VsockSocket_P in WorkDir.
func listenExit(workDir string) (net.Listener, error) {
	path := filepath.Join(workDir, VsockSocket+"_"+strconv.Itoa(ExitPort))
	_ = os.Remove(path)
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen for the exit report on %s: %w", path, err)
	}
	return l, nil
}

// waitExit returns the first exit report on l. The context ends the wait.
func waitExit(ctx context.Context, l net.Listener) (ExitReport, error) {
	type result struct {
		r   ExitReport
		err error
	}
	out := make(chan result, 1)
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				out <- result{err: err}
				return
			}
			line, err := bufio.NewReader(c).ReadBytes('\n')
			_ = c.Close()
			if err != nil && len(line) == 0 {
				continue
			}
			var r ExitReport
			if err := json.Unmarshal(line, &r); err != nil {
				continue
			}
			if r.ExitCode < 0 || r.ExitCode > 255 {
				r.ExitCode = 255
			}
			out <- result{r: r}
			return
		}
	}()
	select {
	case <-ctx.Done():
		_ = l.Close()
		return ExitReport{}, ctx.Err()
	case res := <-out:
		if res.err != nil && !errors.Is(res.err, net.ErrClosed) {
			return ExitReport{}, res.err
		}
		return res.r, res.err
	}
}
