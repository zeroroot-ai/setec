// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

//go:build linux

package guestagent

import (
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

// Supervisor starts processes in the root of the image and is the one
// place that reaps children. A process that it started gets its exit
// status; an orphan that the kernel hands to PID 1 is reaped and dropped.
//
// It never uses exec.Cmd.Wait: a second waiter would race the reaper.
type Supervisor struct {
	// Root is the root of the image, for example /newroot. Empty runs in
	// the root of the agent, which tests use.
	Root string

	mu      sync.Mutex
	waiters map[int]chan syscall.WaitStatus
	// chown changes the owner of a path. Nil uses os.Chown; a test
	// replaces it.
	chown func(path string, uid, gid int) error
}

// NewSupervisor starts the reaper of SIGCHLD for the life of the process.
// A process has one reaper, so it makes one Supervisor.
func NewSupervisor(root string) *Supervisor {
	s := &Supervisor{Root: root, waiters: map[int]chan syscall.WaitStatus{}}
	sig := make(chan os.Signal, 16)
	signal.Notify(sig, syscall.SIGCHLD)
	go func() {
		for range sig {
			s.reap()
		}
	}()
	return s
}

// reap collects each ended child. It holds the lock, so a child is never
// reaped between its start and its registration.
func (s *Supervisor) reap() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for {
		var ws syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &ws, syscall.WNOHANG, nil)
		if pid <= 0 || err != nil {
			return
		}
		if ch, ok := s.waiters[pid]; ok {
			delete(s.waiters, pid)
			ch <- ws
		}
	}
}

// Running is a started process.
type Running struct {
	done chan syscall.WaitStatus
}

// Wait returns the exit code: the code of a normal exit, else 128 plus the
// number of the signal that ended the process, as a shell reports it.
func (r *Running) Wait() int {
	ws := <-r.done
	if ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return ws.ExitStatus()
}

// Start runs p with the given stdin, stdout and stderr. Each file is
// passed to the child, and the caller closes its own copies.
func (s *Supervisor) Start(p Process, stdin, stdout, stderr *os.File) (*Running, error) {
	if len(p.Argv) == 0 {
		return nil, errors.New("guestagent: the process has no argv")
	}
	uid, gid, home, err := lookupUser(s.Root, p.User)
	if err != nil {
		return nil, err
	}
	env := p.Env
	if !hasKey(env, "HOME") {
		env = append(env, "HOME="+home)
	}
	if !hasKey(env, "PATH") {
		env = append(env, "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
	}
	bin, err := s.resolve(p.Argv[0], env)
	if err != nil {
		return nil, err
	}
	dir := p.Dir
	if dir == "" {
		dir = "/"
	}
	attr := &syscall.SysProcAttr{Setsid: true}
	if s.Root != "" {
		attr.Chroot = s.Root
	}
	if uid != uint32(os.Getuid()) || gid != uint32(os.Getgid()) { //nolint:gosec // ids fit
		attr.Credential = &syscall.Credential{Uid: uid, Gid: gid, Groups: []uint32{gid}}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	proc, err := os.StartProcess(bin, p.Argv, &os.ProcAttr{
		Dir: dir, Env: env, Files: []*os.File{stdin, stdout, stderr}, Sys: attr,
	})
	if err != nil {
		return nil, fmt.Errorf("guestagent: start %s: %w", p.Argv[0], err)
	}
	ch := make(chan syscall.WaitStatus, 1)
	s.waiters[proc.Pid] = ch
	_ = proc.Release()
	return &Running{done: ch}, nil
}

// resolve finds argv[0] on the PATH of env, inside Root. The returned
// path is the path inside the root, as the chrooted child sees it.
func (s *Supervisor) resolve(name string, env []string) (string, error) {
	if strings.Contains(name, "/") {
		return name, nil
	}
	path := ""
	for _, e := range env {
		if v, ok := strings.CutPrefix(e, "PATH="); ok {
			path = v
		}
	}
	for _, d := range filepath.SplitList(path) {
		cand := filepath.Join(d, name)
		if fi, err := os.Stat(filepath.Join(s.Root, cand)); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
			return cand, nil
		}
	}
	return "", fmt.Errorf("guestagent: %s is not on the PATH of the image", name)
}

func hasKey(env []string, key string) bool {
	for _, e := range env {
		if strings.HasPrefix(e, key+"=") {
			return true
		}
	}
	return false
}

// lookupUser reads "name", "uid" or "uid:gid" against /etc/passwd of root.
// An empty user keeps the user of the agent, which is root in a machine. A
// numeric uid with no passwd line runs with gid
// equal to uid and home "/", as a container runtime does.
func lookupUser(root, user string) (uid, gid uint32, home string, err error) {
	if user == "" {
		return uint32(os.Getuid()), uint32(os.Getgid()), "/root", nil //nolint:gosec // ids fit
	}
	name, group, hasGroup := strings.Cut(user, ":")
	raw, _ := os.ReadFile(filepath.Join(root, "/etc/passwd")) //nolint:gosec // the passwd of the image
	found := false
	for line := range strings.SplitSeq(string(raw), "\n") {
		f := strings.Split(line, ":")
		if len(f) < 6 || (f[0] != name && f[2] != name) {
			continue
		}
		u, e1 := strconv.ParseUint(f[2], 10, 32)
		g, e2 := strconv.ParseUint(f[3], 10, 32)
		if e1 != nil || e2 != nil {
			continue
		}
		uid, gid, home, found = uint32(u), uint32(g), f[5], true
		break
	}
	if !found {
		u, e := strconv.ParseUint(name, 10, 32)
		if e != nil {
			return 0, 0, "", fmt.Errorf("guestagent: the image has no user %q", name)
		}
		uid, gid, home = uint32(u), uint32(u), "/"
	}
	if hasGroup {
		g, e := strconv.ParseUint(group, 10, 32)
		if e != nil {
			gid, e = lookupGroup(root, group)
			if e != nil {
				return 0, 0, "", e
			}
		} else {
			gid = uint32(g)
		}
	}
	return uid, gid, home, nil
}

func lookupGroup(root, name string) (uint32, error) {
	raw, _ := os.ReadFile(filepath.Join(root, "/etc/group")) //nolint:gosec // the group file of the image
	for line := range strings.SplitSeq(string(raw), "\n") {
		f := strings.Split(line, ":")
		if len(f) >= 3 && f[0] == name {
			g, err := strconv.ParseUint(f[2], 10, 32)
			if err == nil {
				return uint32(g), nil
			}
		}
	}
	return 0, fmt.Errorf("guestagent: the image has no group %q", name)
}

// WorkspaceDir is where a session workspace is mounted in the root.
const WorkspaceDir = "workspace"

// ClaimWorkspace gives a new session workspace to the workload user. A new
// file system belongs to root and holds only lost+found, so a workload that
// runs as another user could not write it. A workspace that holds data, or
// that already belongs to another user, is left as it is.
func (s *Supervisor) ClaimWorkspace(user string) error {
	dir := filepath.Join(s.Root, WorkspaceDir)
	st, err := os.Stat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok || sys.Uid != 0 {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.Name() != "lost+found" {
			return nil
		}
	}
	uid, gid, _, err := lookupUser(s.Root, user)
	if err != nil || uid == 0 {
		return err
	}
	chown := s.chown
	if chown == nil {
		chown = os.Chown
	}
	return chown(dir, int(uid), int(gid))
}
