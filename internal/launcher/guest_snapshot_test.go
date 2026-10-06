// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

//go:build linux

package launcher

import (
	"bufio"
	"context"
	"errors"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/zeroroot-ai/setec/internal/guestagent"
	"github.com/zeroroot-ai/setec/internal/uniquify"
)

// steps records the order of the calls to a loaded guest.
type steps struct {
	mu  sync.Mutex
	got []string
}

func (s *steps) add(v string) { s.mu.Lock(); s.got = append(s.got, v); s.mu.Unlock() }
func (s *steps) list() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.got)
}

type stepReseeder struct{ s *steps }

func (r stepReseeder) Reseed(context.Context, string) error { r.s.add("reseed"); return nil }

type stepUniquifier struct {
	s    *steps
	err  error
	spec *uniquify.Spec
}

func (u *stepUniquifier) Uniquify(_ context.Context, _ string, spec uniquify.Spec) (uniquify.Report, error) {
	u.s.add("uniquify")
	u.spec = &spec
	return uniquify.Report{}, u.err
}

// fakeAgent answers each control request with OK and records its op.
func fakeAgent(t *testing.T, sock string, s *steps) {
	t.Helper()
	ln, err := net.Listen("unix", sock)
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
				defer func() { _ = c.Close() }()
				var req guestagent.Request
				if err := guestagent.ReadLine(bufio.NewReader(c), &req); err != nil {
					return
				}
				if req.Op != guestagent.OpPing {
					s.add(string(req.Op))
				}
				_ = guestagent.WriteLine(c, guestagent.Response{OK: true})
			}()
		}
	}()
}

// TestGuest_AfterStartSnapshotConfirmsTheGuestInOrder pins the order after a
// load: fresh randomness and the clock first, then the Pod address, then a
// new identity that checks the address. No workload starts.
func TestGuest_AfterStartSnapshotConfirmsTheGuestInOrder(t *testing.T) {
	dir, err := os.MkdirTemp("", "ls")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	s := &steps{}
	agent := filepath.Join(dir, "agent.sock")
	fakeAgent(t, agent, s)
	uds := filepath.Join(dir, VsockSocket)
	fakeMux(t, uds, agent)
	u := &stepUniquifier{s: s}
	g := &Guest{UDS: uds, Reseeder: stepReseeder{s}, Uniquifier: u, Hostname: "work-vm"}
	pn := PodNet{Address: netip.MustParsePrefix("10.42.0.9/32"), MAC: "02:00:00:00:00:09", MTU: 1500,
		Gateway: netip.MustParseAddr("10.42.0.1")}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	// The load of a Sandbox snapshot: its workload already runs.
	if err := g.AfterStart(nil)(ctx, pn, true); err != nil {
		t.Fatalf("AfterStart: %v", err)
	}
	want := make([]string, 0, 5)
	want = append(want, "reseed", string(guestagent.OpSetTime), string(guestagent.OpConfigureNet), "uniquify")
	if got := s.list(); !slices.Equal(got, want) {
		t.Fatalf("steps = %v, want %v", got, want)
	}
	// The load of a warm pool base: the workload of the Sandbox starts
	// last, after the new identity (setec#103).
	s.mu.Lock()
	s.got = nil
	s.mu.Unlock()
	if err := g.AfterStart(&guestagent.Process{Argv: []string{"true"}})(ctx, pn, true); err != nil {
		t.Fatalf("AfterStart of a base: %v", err)
	}
	want = append(want, string(guestagent.OpStart))
	if got := s.list(); !slices.Equal(got, want) {
		t.Fatalf("steps of a base load = %v, want %v", got, want)
	}
	if u.spec.PodIP != "10.42.0.9" || u.spec.Hostname != "work-vm" || u.spec.MachineID == "" {
		t.Fatalf("identity = %+v", u.spec)
	}

	// A guest that refuses the identity fails the restore.
	s2 := &steps{}
	agent2 := filepath.Join(dir, "agent2.sock")
	fakeAgent(t, agent2, s2)
	uds2 := filepath.Join(dir, "v2.sock")
	fakeMux(t, uds2, agent2)
	g2 := &Guest{UDS: uds2, Reseeder: stepReseeder{s2}, Uniquifier: &stepUniquifier{s: s2, err: errors.New("no")}}
	if err := g2.AfterStart(nil)(ctx, pn, true); err == nil {
		t.Fatal("AfterStart accepted a guest that refused its identity")
	}
}
