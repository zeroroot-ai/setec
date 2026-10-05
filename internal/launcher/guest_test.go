// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

//go:build linux

package launcher

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zeroroot-ai/setec/internal/guestagent"
)

// One supervisor for the package: a process has one reaper of SIGCHLD, and
// a second one would take the exit status of a child of the first.
var (
	supOnce sync.Once
	sup     *guestagent.Supervisor
)

func testSupervisor() *guestagent.Supervisor {
	supOnce.Do(func() { sup = guestagent.NewSupervisor("") })
	return sup
}

// fakeMux is the host side of the Firecracker vsock device: it answers
// "CONNECT <port>" and splices the connection to the guest agent.
func fakeMux(t *testing.T, uds, agent string) {
	t.Helper()
	ln, err := net.Listen("unix", uds)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				r := bufio.NewReader(c)
				line, _ := r.ReadString('\n')
				if !strings.HasPrefix(line, "CONNECT ") {
					_ = c.Close()
					return
				}
				a, err := net.Dial("unix", agent)
				if err != nil {
					_ = c.Close()
					return
				}
				_, _ = io.WriteString(c, "OK 1073741824\n")
				go func() { _, _ = io.Copy(a, r) }()
				_, _ = io.Copy(c, a)
				_ = c.Close()
			}()
		}
	}()
}

type recNet struct {
	mu  sync.Mutex
	got []guestagent.Request
}

func (r *recNet) Configure(req guestagent.Request) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, req)
	return nil
}

// TestGuest_AfterStartBootConfiguresTheNetworkThenStartsTheWorkload runs the
// launcher side against the real guest agent server.
func TestGuest_AfterStartBootConfiguresTheNetworkThenStartsTheWorkload(t *testing.T) {
	dir, err := os.MkdirTemp("", "lg")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	agentSock := filepath.Join(dir, "agent.sock")
	ln, err := net.Listen("unix", agentSock)
	if err != nil {
		t.Fatal(err)
	}
	exits := make(chan int, 1)
	rn := &recNet{}
	srv := &guestagent.Server{Sup: testSupervisor(), Net: rn, Console: os.Stderr,
		ReportExit: func(code int) error { exits <- code; return nil }}
	go func() { _ = srv.Serve(t.Context(), ln) }()
	uds := filepath.Join(dir, VsockSocket)
	fakeMux(t, uds, agentSock)

	resolv := filepath.Join(dir, "resolv.conf")
	_ = os.WriteFile(resolv, []byte("nameserver 10.43.0.10\n"), 0o600)
	g := &Guest{UDS: uds, ResolvConf: resolv}
	pn := PodNet{Address: netip.MustParsePrefix("10.42.0.7/32"), MAC: "02:00:00:00:00:07", MTU: 1500,
		Gateway: netip.MustParseAddr("10.42.0.1")}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := g.AfterStart(&guestagent.Process{Argv: []string{"sh", "-c", "exit 4"}})(ctx, pn, false); err != nil {
		t.Fatalf("AfterStart: %v", err)
	}
	if len(rn.got) != 1 || rn.got[0].Address != "10.42.0.7/32" || rn.got[0].Gateway != "10.42.0.1" ||
		string(rn.got[0].DNS) != "nameserver 10.43.0.10\n" {
		t.Fatalf("configure-net = %+v", rn.got)
	}
	select {
	case code := <-exits:
		if code != 4 {
			t.Fatalf("exit = %d, want 4", code)
		}
	case <-ctx.Done():
		t.Fatal("the workload did not report an exit")
	}
}

// TestGuest_ExecRelaysStdioAndTheExitCode runs the relay of
// `setec-launcher exec` against the real guest agent server.
func TestGuest_ExecRelaysStdioAndTheExitCode(t *testing.T) {
	dir, err := os.MkdirTemp("", "lx")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	agentSock := filepath.Join(dir, "agent.sock")
	ln, err := net.Listen("unix", agentSock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &guestagent.Server{Sup: testSupervisor(), Console: os.Stderr,
		ReportExit: func(int) error { return nil }}
	go func() { _ = srv.Serve(t.Context(), ln) }()
	uds := filepath.Join(dir, VsockSocket)
	fakeMux(t, uds, agentSock)

	var out, errOut bytes.Buffer
	code, err := (&Guest{UDS: uds}).Exec(t.Context(),
		guestagent.Process{Argv: []string{"sh", "-c", "read x; echo got $x; echo e >&2; exit 5"}},
		strings.NewReader("hi\n"), &out, &errOut)
	if err != nil || code != 5 || out.String() != "got hi\n" || errOut.String() != "e\n" {
		t.Fatalf("Exec = %d, %v, stdout=%q stderr=%q", code, err, out.String(), errOut.String())
	}
}
