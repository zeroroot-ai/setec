// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package launcher

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zeroroot-ai/setec/internal/entropy"
	"github.com/zeroroot-ai/setec/internal/guestagent"
)

// Guest talks to the guest agent through the vsock socket of Firecracker.
type Guest struct {
	// UDS is the host side of the vsock device (WorkDir/VsockSocket).
	UDS string
	// Reseeder reseeds the guest after a snapshot load. Nil uses the
	// entropy client of the node agent.
	Reseeder entropy.Reseeder
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
		return nil, nil, fmt.Errorf("vsock CONNECT %d: %q %v", port, line, err)
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
// before anything else (proof 4, setec#183). It always gives the guest the
// identity of this Pod. It starts the workload only after a boot: a loaded
// snapshot already runs its workload.
func (g *Guest) AfterStart(workload *guestagent.Process) func(context.Context, PodNet, bool) error {
	return func(ctx context.Context, pn PodNet, fromSnapshot bool) error {
		rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if err := g.WaitReady(rctx); err != nil {
			return err
		}
		if fromSnapshot {
			rs := g.Reseeder
			if rs == nil {
				rs = entropy.NewVsockReseeder()
			}
			if err := rs.Reseed(rctx, g.UDS); err != nil {
				return fmt.Errorf("reseed after the snapshot load: %w", err)
			}
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
			MTU: pn.MTU, Gateway: pn.Gateway.String(), DNS: dns,
		}); err != nil {
			return err
		}
		if fromSnapshot || workload == nil {
			return nil
		}
		_, err := g.Call(rctx, guestagent.Request{Op: guestagent.OpStart, Process: workload})
		return err
	}
}

// NewGuest returns the Guest of s.
func NewGuest(s *Spec) *Guest {
	return &Guest{UDS: filepath.Join(s.WorkDir, VsockSocket), ResolvConf: "/etc/resolv.conf"}
}
