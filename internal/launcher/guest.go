// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package launcher

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zeroroot-ai/setec/internal/entropy"
	"github.com/zeroroot-ai/setec/internal/guestagent"
	"github.com/zeroroot-ai/setec/internal/uniquify"
)

// Guest talks to the guest agent through the vsock socket of Firecracker.
type Guest struct {
	// UDS is the host side of the vsock device (WorkDir/VsockSocket).
	UDS string
	// Reseeder reseeds the guest after a snapshot load. Nil uses the
	// entropy client of the node agent.
	Reseeder entropy.Reseeder
	// Uniquifier gives the guest a new machine-id, boot-id and hostname
	// after a snapshot load, and checks that it sees the Pod address. Nil
	// uses the uniquify client of the node agent.
	Uniquifier uniquify.Uniquifier
	// Hostname is the hostname that a loaded guest takes: the name of the
	// Pod.
	Hostname string
	// TakenAtFile holds the time of a loaded state in Unix nanoseconds.
	// The guest tells the workload about it. Empty or missing says
	// nothing.
	TakenAtFile string
	// ResolvConf is the resolv.conf of the Pod that the guest takes.
	ResolvConf string
}

// dial opens a connection to guest port through the Firecracker socket.
func (g *Guest) dial(ctx context.Context, port int) (net.Conn, *bufio.Reader, error) {
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", g.UDS)
	if err != nil {
		return nil, nil, err
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = c.SetDeadline(dl)
	}
	if _, err := fmt.Fprintf(c, "CONNECT %d\n", port); err != nil {
		_ = c.Close()
		return nil, nil, err
	}
	r := bufio.NewReader(c)
	line, err := r.ReadString('\n')
	if err != nil || !strings.HasPrefix(line, "OK ") {
		_ = c.Close()
		return nil, nil, fmt.Errorf("vsock CONNECT %d: %q %w", port, line, err)
	}
	return c, r, nil
}

// Call sends one request and reads the response.
func (g *Guest) Call(ctx context.Context, req guestagent.Request) (guestagent.Response, error) {
	c, r, err := g.dial(ctx, guestagent.ControlPort)
	if err != nil {
		return guestagent.Response{}, err
	}
	defer func() { _ = c.Close() }()
	if err := guestagent.WriteLine(c, req); err != nil {
		return guestagent.Response{}, err
	}
	var resp guestagent.Response
	if err := guestagent.ReadLine(r, &resp); err != nil {
		return guestagent.Response{}, err
	}
	if !resp.OK {
		return resp, fmt.Errorf("guest agent %s: %s", req.Op, resp.Error)
	}
	return resp, nil
}

// WaitReady pings the guest agent until it answers or ctx ends.
func (g *Guest) WaitReady(ctx context.Context) error {
	for {
		cctx, cancel := context.WithTimeout(ctx, time.Second)
		_, err := g.Call(cctx, guestagent.Request{Op: guestagent.OpPing})
		cancel()
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("the guest agent did not answer: %w", err)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// AfterStart is the Launcher.AfterStart of a launcher machine. After a
// snapshot load it reseeds the guest and sets its clock to the node time
// before anything else (proof 4, setec#183). A load with LoadedNoReseed
// skips the reseed only. It always gives the guest the
// address of this Pod. After a load it then gives the guest a new identity
// and checks that the guest sees the Pod address. It starts the workload
// when there is one: after a boot, and after the load of a warm pool base.
func (g *Guest) AfterStart(workload *guestagent.Process) func(context.Context, PodNet, Start) error {
	return func(ctx context.Context, pn PodNet, start Start) error {
		fromSnapshot := start != Booted
		rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if err := g.WaitReady(rctx); err != nil {
			return err
		}
		if start == Loaded {
			rs := g.Reseeder
			if rs == nil {
				rs = entropy.NewVsockReseeder()
			}
			if err := rs.Reseed(rctx, g.UDS); err != nil {
				return fmt.Errorf("reseed after the snapshot load: %w", err)
			}
		}
		if fromSnapshot {
			if _, err := g.Call(rctx, guestagent.Request{Op: guestagent.OpSetTime, UnixNano: time.Now().UnixNano()}); err != nil {
				return err
			}
		}
		var dns []byte
		if g.ResolvConf != "" {
			dns, _ = os.ReadFile(g.ResolvConf) //nolint:gosec // the resolv.conf of the Pod
		}
		if _, err := g.Call(rctx, guestagent.Request{
			Op: guestagent.OpConfigureNet, Address: pn.Address.String(), MAC: pn.MAC,
			MTU: pn.MTU, Gateway: pn.Gateway.String(), DNS: dns, Hostname: g.Hostname,
		}); err != nil {
			return err
		}
		if fromSnapshot {
			if err := g.uniquify(rctx, pn); err != nil {
				return err
			}
			if err := g.tellResumed(rctx); err != nil {
				return err
			}
		}
		// A loaded Sandbox snapshot already runs its workload and gets
		// none. A loaded base runs none yet and gets the workload of the
		// Sandbox (setec#103).
		if workload == nil {
			return nil
		}
		_, err := g.Call(rctx, guestagent.Request{Op: guestagent.OpStart, Process: workload})
		return err
	}
}

// uniquify gives a loaded guest a new identity (ADR-0145 invariant 2).
func (g *Guest) uniquify(ctx context.Context, pn PodNet) error {
	u := g.Uniquifier
	if u == nil {
		u = uniquify.NewVsockUniquifier()
	}
	spec, err := uniquify.NewSpec(g.Hostname, pn.Address.Addr().String())
	if err != nil {
		return err
	}
	if _, err := u.Uniquify(ctx, g.UDS, spec); err != nil {
		return fmt.Errorf("a new identity after the snapshot load: %w", err)
	}
	return nil
}

// tellResumed gives the guest the time of the loaded state, when the node
// agent wrote it.
func (g *Guest) tellResumed(ctx context.Context) error {
	if g.TakenAtFile == "" {
		return nil
	}
	raw, err := os.ReadFile(g.TakenAtFile)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	ns, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
	if err != nil {
		return fmt.Errorf("the time of the state: %w", err)
	}
	_, err = g.Call(ctx, guestagent.Request{Op: guestagent.OpResumed, UnixNano: ns})
	return err
}

// NewGuest returns the Guest of s. A loaded guest takes the hostname of
// the Pod.
func NewGuest(s *Spec) *Guest {
	host, _ := os.Hostname()
	g := &Guest{UDS: filepath.Join(s.WorkDir, VsockSocket), ResolvConf: "/etc/resolv.conf", Hostname: host}
	if s.Source.Snapshot != nil {
		g.TakenAtFile = s.Source.Snapshot.TakenAt
	}
	return g
}

// Exec runs p in the machine through the guest agent: stdin goes in as
// frames, stdout and stderr come back, and the exit code of p is returned.
// It is the relay of `setec-launcher exec`, which the frontend runs in the
// launcher container (docs/design/lifecycles.md).
func (g *Guest) Exec(ctx context.Context, p guestagent.Process, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	c, r, err := g.dial(ctx, guestagent.ControlPort)
	if err != nil {
		return 0, err
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Time{})
	if err := guestagent.WriteLine(c, guestagent.Request{Op: guestagent.OpExec, Process: &p}); err != nil {
		return 0, err
	}
	var resp guestagent.Response
	if err := guestagent.ReadLine(r, &resp); err != nil {
		return 0, err
	}
	if !resp.OK {
		return 0, fmt.Errorf("guest agent exec: %s", resp.Error)
	}
	var wmu sync.Mutex
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, rerr := stdin.Read(buf)
			if n > 0 {
				wmu.Lock()
				werr := guestagent.WriteFrame(c, guestagent.FrameStdin, buf[:n])
				wmu.Unlock()
				if werr != nil {
					return
				}
			}
			if rerr != nil {
				wmu.Lock()
				_ = guestagent.WriteFrame(c, guestagent.FrameStdinEOF, nil)
				wmu.Unlock()
				return
			}
		}
	}()
	for {
		t, data, err := guestagent.ReadFrame(r)
		if err != nil {
			return 0, fmt.Errorf("the exec stream ended before its exit code: %w", err)
		}
		switch t {
		case guestagent.FrameStdout:
			if _, err := stdout.Write(data); err != nil {
				return 0, err
			}
		case guestagent.FrameStderr:
			if _, err := stderr.Write(data); err != nil {
				return 0, err
			}
		case guestagent.FrameExit:
			return guestagent.ExitCode(data), nil
		}
	}
}
