// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

//go:build linux

package guestagent

import (
	"bufio"
	"bytes"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// One supervisor for the package: a process has one reaper of SIGCHLD.
var (
	supOnce sync.Once
	sup     *Supervisor
)

func testSupervisor(t *testing.T) *Supervisor {
	t.Helper()
	supOnce.Do(func() {
		// The test process stands in for PID 1: orphans of its children
		// come to it, so the reaper must collect them.
		_ = unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0)
		sup = NewSupervisor("")
	})
	return sup
}

type harness struct {
	addr  string
	exits chan int
}

func startServer(t *testing.T, allow func(net.Conn) bool) *harness {
	t.Helper()
	dir, err := os.MkdirTemp("", "ga")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	addr := filepath.Join(dir, "agent.sock")
	ln, err := net.Listen("unix", addr)
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{addr: addr, exits: make(chan int, 1)}
	console, err := os.CreateTemp(dir, "console")
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{
		Sup: testSupervisor(t), AllowPeer: allow, Console: console,
		ReportExit: func(code int) error { h.exits <- code; return nil },
	}
	go func() { _ = s.Serve(t.Context(), ln) }()
	return h
}

func (h *harness) call(t *testing.T, req Request, body []byte) (*bufio.Reader, net.Conn, Response) {
	t.Helper()
	c, err := net.Dial("unix", h.addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if err := WriteLine(c, req); err != nil {
		t.Fatal(err)
	}
	if body != nil {
		if _, err := c.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	r := bufio.NewReader(c)
	var resp Response
	if err := ReadLine(r, &resp); err != nil {
		t.Fatalf("read response: %v", err)
	}
	return r, c, resp
}

func TestServer_StartReportsTheExitCodeOfTheWorkload(t *testing.T) {
	h := startServer(t, nil)
	_, _, resp := h.call(t, Request{Op: OpStart, Process: &Process{Argv: []string{"sh", "-c", "exit 7"}}}, nil)
	if !resp.OK {
		t.Fatalf("start: %s", resp.Error)
	}
	select {
	case code := <-h.exits:
		if code != 7 {
			t.Fatalf("exit code = %d, want 7", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no exit report")
	}
	_, _, again := h.call(t, Request{Op: OpStart, Process: &Process{Argv: []string{"true"}}}, nil)
	if again.OK {
		t.Fatal("a second start was accepted; a machine runs one workload")
	}
}

func TestServer_AWorkloadThatCannotStartReports127(t *testing.T) {
	h := startServer(t, nil)
	_, _, resp := h.call(t, Request{Op: OpStart, Process: &Process{Argv: []string{"no-such-tool-xyz"}}}, nil)
	if resp.OK {
		t.Fatal("start of a missing tool was reported as OK")
	}
	if code := <-h.exits; code != 127 {
		t.Fatalf("exit code = %d, want 127", code)
	}
}

func TestServer_ExecStreamsAndReturnsTheExitCode(t *testing.T) {
	h := startServer(t, nil)
	r, c, resp := h.call(t, Request{Op: OpExec, Process: &Process{
		Argv: []string{"sh", "-c", "read x; echo got $x; echo to-stderr >&2; exit 3"},
		Env:  []string{"PATH=/usr/bin:/bin"},
	}}, nil)
	if !resp.OK {
		t.Fatalf("exec: %s", resp.Error)
	}
	_ = WriteFrame(c, FrameStdin, []byte("hello\n"))
	_ = WriteFrame(c, FrameStdinEOF, nil)
	var out, errOut bytes.Buffer
	code := -1
	for code < 0 {
		ft, data, err := ReadFrame(r)
		if err != nil {
			t.Fatalf("read frame: %v", err)
		}
		switch ft {
		case FrameStdout:
			out.Write(data)
		case FrameStderr:
			errOut.Write(data)
		case FrameExit:
			code = ExitCode(data)
		case FrameStdin, FrameStdinEOF:
			t.Fatalf("the agent sent a stdin frame %d", ft)
		}
	}
	if code != 3 || out.String() != "got hello\n" || errOut.String() != "to-stderr\n" {
		t.Fatalf("exec: code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
}

func TestServer_CopyInAndOut(t *testing.T) {
	h := startServer(t, nil)
	path := filepath.Join(t.TempDir(), "f.txt")
	data := []byte("the file content\n")
	if _, _, resp := h.call(t, Request{Op: OpCopyIn, Path: path, Size: int64(len(data)), Mode: 0o600}, data); !resp.OK {
		t.Fatalf("copy-in: %s", resp.Error)
	}
	r, _, resp := h.call(t, Request{Op: OpCopyOut, Path: path}, nil)
	if !resp.OK || resp.Size != int64(len(data)) {
		t.Fatalf("copy-out: %+v", resp)
	}
	got := make([]byte, resp.Size)
	if _, err := r.Read(got); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("copy-out bytes = %q, %v", got, err)
	}
	if _, _, resp := h.call(t, Request{Op: OpCopyOut, Path: "relative"}, nil); resp.OK {
		t.Fatal("a relative path was accepted")
	}
}

// TestServer_RefusesAPeerThatIsNotTheLauncher proves the peer check: the
// command allows vsock CID 2 only, so a process in the machine and the
// workload network cannot reach the agent.
func TestServer_RefusesAPeerThatIsNotTheLauncher(t *testing.T) {
	h := startServer(t, func(net.Conn) bool { return false })
	c, err := net.Dial("unix", h.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_ = WriteLine(c, Request{Op: OpPing})
	var resp Response
	if err := ReadLine(bufio.NewReader(c), &resp); err == nil {
		t.Fatalf("a refused peer got an answer: %+v", resp)
	}
}

// TestSupervisor_ReapsOrphans starts a shell that leaves a child behind.
// The child comes to the reaper, which must collect it: no zombie stays.
func TestSupervisor_ReapsOrphans(t *testing.T) {
	s := testSupervisor(t)
	null, _ := os.Open(os.DevNull)
	defer func() { _ = null.Close() }()
	run, err := s.Start(Process{Argv: []string{"sh", "-c", "sleep 0.2 & exit 0"}}, null, null, null)
	if err != nil {
		t.Fatal(err)
	}
	if code := run.Wait(); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if zombies(t) == 0 {
			time.Sleep(400 * time.Millisecond)
			if zombies(t) == 0 {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%d zombie children stay", zombies(t))
}

func zombies(t *testing.T) int {
	t.Helper()
	me := strconv.Itoa(os.Getpid())
	ents, _ := os.ReadDir("/proc")
	n := 0
	for _, e := range ents {
		raw, err := os.ReadFile(filepath.Join("/proc", e.Name(), "stat"))
		if err != nil {
			continue
		}
		f := strings.Fields(string(raw[bytes.LastIndexByte(raw, ')')+1:]))
		if len(f) > 1 && f[0] == "Z" && f[1] == me {
			n++
		}
	}
	return n
}

func TestLookupUser(t *testing.T) {
	root := t.TempDir()
	_ = os.MkdirAll(filepath.Join(root, "etc"), 0o755)
	_ = os.WriteFile(filepath.Join(root, "etc/passwd"), []byte("root:x:0:0::/root:/bin/sh\nrunner:x:999:998::/home/runner:/bin/sh\n"), 0o600)
	_ = os.WriteFile(filepath.Join(root, "etc/group"), []byte("runner:x:998:\ntools:x:500:\n"), 0o600)
	for _, tc := range []struct {
		user     string
		uid, gid uint32
		home     string
		wantErr  bool
	}{
		{"", uint32(os.Getuid()), uint32(os.Getgid()), "/root", false}, //nolint:gosec // ids fit
		{"runner", 999, 998, "/home/runner", false},
		{"999", 999, 998, "/home/runner", false},
		{"runner:tools", 999, 500, "/home/runner", false},
		{"1234", 1234, 1234, "/", false},
		{"nobody-here", 0, 0, "", true},
	} {
		uid, gid, home, err := lookupUser(root, tc.user)
		if (err != nil) != tc.wantErr || uid != tc.uid || gid != tc.gid || home != tc.home {
			t.Errorf("lookupUser(%q) = %d %d %q %v", tc.user, uid, gid, home, err)
		}
	}
}
