// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

//go:build linux

package guestagent

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/zeroroot-ai/setec/internal/errwrap"
)

// NetConfigurer applies the identity of the Pod to the machine.
type NetConfigurer interface {
	Configure(req Request) error
}

// Server answers the launcher on ControlPort.
type Server struct {
	Sup *Supervisor
	// AllowPeer reports whether a connection comes from the launcher. The
	// command allows vsock CID 2 only.
	AllowPeer func(net.Conn) bool
	// ReportExit sends the exit code of the workload to the launcher.
	ReportExit func(code int) error
	Net        NetConfigurer
	// Console receives the output of the workload, so the launcher log
	// shows it.
	Console *os.File
	Logf    func(string, ...any)

	startOnce sync.Once
}

// Serve accepts until ctx ends.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	go func() { <-ctx.Done(); _ = ln.Close() }()
	for {
		c, err := ln.Accept()
		if err != nil {
			// A closed listener after the end of ctx is a clean stop.
			if ctx.Err() != nil && errors.Is(err, net.ErrClosed) {
				return errwrap.Wrap(ctx.Err(), "context.Context.Err")
			}
			return errwrap.Wrap(err, "net.Listener.Accept")
		}
		go s.serveConn(c)
	}
}

func (s *Server) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}

func (s *Server) serveConn(c net.Conn) {
	defer func() { _ = c.Close() }()
	if s.AllowPeer != nil && !s.AllowPeer(c) {
		s.logf("guestagent: refused a connection from %v", c.RemoteAddr())
		return
	}
	r := bufio.NewReaderSize(c, MaxLine)
	var req Request
	if err := ReadLine(r, &req); err != nil {
		_ = WriteLine(c, Response{Error: err.Error()})
		return
	}
	var err error
	switch req.Op {
	case OpPing:
	case OpConfigureNet:
		if s.Net == nil {
			err = errors.New("no network configurer")
		} else {
			err = s.Net.Configure(req)
		}
	case OpSetTime:
		tv := syscall.NsecToTimeval(req.UnixNano)
		err = syscall.Settimeofday(&tv)
	case OpResumed:
		err = WriteResumed(s.Sup.Root, time.Unix(0, req.UnixNano), time.Now())
	case OpStart:
		err = s.startWorkload(req.Process)
	case OpExec:
		s.exec(c, r, req.Process)
		return
	case OpCopyIn:
		err = s.copyIn(r, req)
	case OpCopyOut:
		s.copyOut(c, req)
		return
	default:
		err = fmt.Errorf("unknown op %q", req.Op)
	}
	resp := Response{OK: err == nil}
	if err != nil {
		resp.Error = err.Error()
	}
	_ = WriteLine(c, resp)
}

// startWorkload starts the one workload of the machine and reports its exit.
func (s *Server) startWorkload(p *Process) error {
	if p == nil {
		return errors.New("start: no process")
	}
	err := errors.New("start: the workload already started")
	s.startOnce.Do(func() {
		devnull, oerr := os.Open(os.DevNull)
		if oerr != nil {
			err = oerr
			return
		}
		defer func() { _ = devnull.Close() }()
		proc := WithImageDefaults(s.Sup.Root, *p)
		if cerr := s.Sup.ClaimWorkspace(proc.User); cerr != nil && s.Logf != nil {
			s.Logf("setec-guest-agent: give the new workspace to the workload user: %v", cerr)
		}
		run, serr := s.Sup.Start(proc, devnull, s.Console, s.Console)
		if serr != nil {
			err = serr
			// The launcher waits for an exit report, so a workload that
			// cannot start reports 127, as a shell does.
			go s.report(127)
			return
		}
		err = nil
		go func() { s.report(run.Wait()) }()
	})
	return err
}

func (s *Server) report(code int) {
	for i := range 50 {
		if err := s.ReportExit(code); err == nil {
			return
		} else if i == 49 {
			s.logf("guestagent: report the exit code %d: %v", code, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// exec runs one process with streamed stdin, stdout and stderr.
func (s *Server) exec(c net.Conn, r *bufio.Reader, p *Process) {
	if p == nil {
		_ = WriteLine(c, Response{Error: "exec: no process"})
		return
	}
	inR, inW, err1 := os.Pipe()
	outR, outW, err2 := os.Pipe()
	errR, errW, err3 := os.Pipe()
	if err := errors.Join(err1, err2, err3); err != nil {
		_ = WriteLine(c, Response{Error: err.Error()})
		return
	}
	run, err := s.Sup.Start(WithImageDefaults(s.Sup.Root, *p), inR, outW, errW)
	_ = inR.Close()
	_ = outW.Close()
	_ = errW.Close()
	if err != nil {
		_ = inW.Close()
		_ = outR.Close()
		_ = errR.Close()
		_ = WriteLine(c, Response{Error: err.Error()})
		return
	}
	if err := WriteLine(c, Response{OK: true}); err != nil {
		return
	}

	var wmu sync.Mutex
	send := func(t FrameType, b []byte) {
		wmu.Lock()
		defer wmu.Unlock()
		_ = WriteFrame(c, t, b)
	}
	var wg sync.WaitGroup
	for t, f := range map[FrameType]*os.File{FrameStdout: outR, FrameStderr: errR} {
		wg.Go(func() {
			buf := make([]byte, 32*1024)
			for {
				n, err := f.Read(buf)
				if n > 0 {
					send(t, buf[:n])
				}
				if err != nil {
					_ = f.Close()
					return
				}
			}
		})
	}
	go func() {
		defer func() { _ = inW.Close() }()
		for {
			t, data, err := ReadFrame(r)
			if err != nil || t == FrameStdinEOF {
				return
			}
			if t == FrameStdin {
				if _, err := inW.Write(data); err != nil {
					return
				}
			}
		}
	}()
	code := run.Wait()
	wg.Wait()
	send(FrameExit, ExitFrame(code))
}

func (s *Server) inRoot(p string) (string, error) {
	if !filepath.IsAbs(p) {
		return "", fmt.Errorf("path %q is not absolute", p)
	}
	return filepath.Join(s.Sup.Root, filepath.Clean(p)), nil
}

func (s *Server) copyIn(r *bufio.Reader, req Request) error {
	path, err := s.inRoot(req.Path)
	if err != nil {
		return err
	}
	mode := os.FileMode(req.Mode & 0o7777)
	if mode == 0 {
		mode = 0o644
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode) //nolint:gosec // a path the launcher names
	if err != nil {
		return errwrap.Wrap(err, "os.OpenFile")
	}
	n, err := io.CopyN(f, r, req.Size)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("copy-in: wrote %d of %d bytes: %w", n, req.Size, err)
	}
	return nil
}

func (s *Server) copyOut(c net.Conn, req Request) {
	path, err := s.inRoot(req.Path)
	var f *os.File
	var fi os.FileInfo
	if err == nil {
		f, err = os.Open(path) //nolint:gosec // a path the launcher names
	}
	if err == nil {
		defer func() { _ = f.Close() }()
		fi, err = f.Stat()
	}
	if err == nil && !fi.Mode().IsRegular() {
		err = fmt.Errorf("copy-out: %s is not a regular file", req.Path)
	}
	if err != nil {
		_ = WriteLine(c, Response{Error: err.Error()})
		return
	}
	if err := WriteLine(c, Response{OK: true, Size: fi.Size()}); err != nil {
		return
	}
	_, _ = io.CopyN(c, f, fi.Size())
}
