// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package launcher

import (
	"context"
	"errors"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
)

// policy models a network plugin that enforces its policy after a number of
// rounds. Before that, each destination answers, as it does with no policy.
// After it, the blocked destinations drop and the allowed ones answer.
type policy struct {
	calls          atomic.Int32
	enforceAfter   int32
	blockedAddrs   map[string]bool
	allowedDropped bool // the allow side is not programmed yet
}

func (p *policy) enforced() bool { return p.calls.Load() > p.enforceAfter }

func (p *policy) dial(_ context.Context, address string) (net.Conn, error) {
	p.calls.Add(1)
	if p.enforced() && p.blockedAddrs[address] {
		return nil, errors.New("i/o timeout")
	}
	c, _ := net.Pipe()
	return c, nil
}

func (p *policy) query(_ context.Context, address string) error {
	if p.allowedDropped {
		return errors.New("i/o timeout")
	}
	return nil
}

func testGate(t *testing.T, p *policy, wait int) *Gate {
	t.Helper()
	return &Gate{
		Spec: PolicyGateSpec{
			WaitSeconds: wait,
			Blocked:     []string{"tcp://10.43.0.1:443"},
			Allowed:     []string{"dns://10.0.0.2:53"},
		},
		Marker:   t.TempDir() + "/policy-enforced",
		Interval: time.Millisecond, ProbeTimeout: 50 * time.Millisecond,
		Dial: p.dial, Query: p.query,
	}
}

// TestGateWaitsForTheDenyAndPasses: while the blocked destination still
// answers, the gate holds. When the plugin enforces, it passes and writes the
// marker.
func TestGateWaitsForTheDenyAndPasses(t *testing.T) {
	t.Parallel()
	p := &policy{enforceAfter: 5, blockedAddrs: map[string]bool{"10.43.0.1:443": true}}
	g := testGate(t, p, 5)
	if err := g.Wait(t.Context()); err != nil {
		t.Fatalf("Wait = %v; want pass after the plugin enforces", err)
	}
	if got := p.calls.Load(); got <= 5 {
		t.Errorf("the gate passed after %d probes, before the policy was in force", got)
	}
	if _, err := os.Stat(g.Marker); err != nil {
		t.Errorf("the marker is missing: %v", err)
	}
}

// TestGateEndsWithoutProof: a blocked destination that stays reachable ends the
// wait at the bound, with the typed reason and the name of the destination, and
// leaves no marker.
func TestGateEndsWithoutProof(t *testing.T) {
	t.Parallel()
	p := &policy{enforceAfter: 1 << 30, blockedAddrs: map[string]bool{"10.43.0.1:443": true}}
	g := testGate(t, p, 1)
	begin := time.Now()
	err := g.Wait(t.Context())
	var le *Error
	if !errors.As(err, &le) || le.Reason != ReasonPolicyNotEnforced {
		t.Fatalf("Wait = %v; want the reason %s", err, ReasonPolicyNotEnforced)
	}
	if !strings.Contains(err.Error(), "tcp://10.43.0.1:443 is reachable") {
		t.Errorf("the message does not name the destination: %v", err)
	}
	if d := time.Since(begin); d < 900*time.Millisecond || d > 5*time.Second {
		t.Errorf("the wait took %s; want about the bound of 1s", d)
	}
	if _, serr := os.Stat(g.Marker); serr == nil {
		t.Error("a marker exists after a failed gate")
	}
}

// TestGateGoesOnWithoutTheAllowSide: with the block in force and no allowed
// destination that answers, the gate waits for the grace, logs it, and passes. A
// cluster that reaches none of its resolvers, such as an air gap, is not stuck.
func TestGateGoesOnWithoutTheAllowSide(t *testing.T) {
	t.Parallel()
	p := &policy{blockedAddrs: map[string]bool{"10.43.0.1:443": true}, allowedDropped: true}
	g := testGate(t, p, 5)
	g.AllowGrace = 50 * time.Millisecond
	var log strings.Builder
	g.Log = &log
	begin := time.Now()
	if err := g.Wait(t.Context()); err != nil {
		t.Fatalf("Wait = %v; want a pass after the grace", err)
	}
	if d := time.Since(begin); d < 50*time.Millisecond {
		t.Errorf("the gate passed after %s, before the grace", d)
	}
	if !strings.Contains(log.String(), "no allowed destination answered") {
		t.Errorf("the log does not say that the allow side is not proven: %q", log.String())
	}
	if _, err := os.Stat(g.Marker); err != nil {
		t.Errorf("the marker is missing: %v", err)
	}
}

// TestGateNeverPassesWithoutTheBlock: the grace never covers a block that is not
// proven.
func TestGateNeverPassesWithoutTheBlock(t *testing.T) {
	t.Parallel()
	p := &policy{enforceAfter: 1 << 30, blockedAddrs: map[string]bool{"10.43.0.1:443": true}, allowedDropped: true}
	g := testGate(t, p, 1)
	g.AllowGrace = time.Millisecond
	if err := g.Wait(t.Context()); err == nil {
		t.Fatal("Wait passed with a reachable blocked destination")
	}
}

// TestGateStopsWithTheContext: a stop of the Pod ends the wait and gives no
// proof.
func TestGateStopsWithTheContext(t *testing.T) {
	t.Parallel()
	p := &policy{enforceAfter: 1 << 30, blockedAddrs: map[string]bool{"10.43.0.1:443": true}}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err := testGate(t, p, 60).Wait(ctx)
	var le *Error
	if err == nil || errors.As(err, &le) {
		t.Fatalf("Wait = %v; want a plain cancel, not the policy reason", err)
	}
}

func TestGateSpecIsChecked(t *testing.T) {
	t.Parallel()
	for name, s := range map[string]PolicyGateSpec{
		"no wait":           {Blocked: []string{"tcp://10.0.0.1:1"}},
		"not a url":         {WaitSeconds: 1, Blocked: []string{"10.0.0.1:1"}},
		"unknown scheme":    {WaitSeconds: 1, Blocked: []string{"udp://10.0.0.1:1"}},
		"no port":           {WaitSeconds: 1, Allowed: []string{"dns://10.0.0.1"}},
		"dns must be allow": {WaitSeconds: 1, Blocked: []string{"dns://10.0.0.1:53"}},
	} {
		if err := s.validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	ok := PolicyGateSpec{WaitSeconds: 1, Blocked: []string{"tcp://10.0.0.1:1"}, Allowed: []string{"dns://[fd00::2]:53"}}
	if err := ok.validate(); err != nil {
		t.Errorf("a good spec is refused: %v", err)
	}
}

// TestReadinessNeedsTheMarker: a Pod with a gate is not ready before the
// marker, and a Pod with no gate is.
func TestReadinessNeedsTheMarker(t *testing.T) {
	t.Parallel()
	s := testSpec(t)
	if !GatePassed(s) {
		t.Fatal("a spec with no gate is not ready")
	}
	s.PolicyGate = &PolicyGateSpec{WaitSeconds: 1}
	if err := os.MkdirAll(s.WorkDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if GatePassed(s) {
		t.Fatal("a spec with a gate is ready with no marker")
	}
	if err := os.WriteFile(GateMarkerPath(s), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if !GatePassed(s) {
		t.Fatal("a spec with a gate is not ready after the marker")
	}
}

// TestRunHoldsTheMachineOffTheNetwork: the machine joins the Pod network only
// after the gate passed, and a gate that fails ends the run with its reason and
// no connection.
func TestRunHoldsTheMachineOffTheNetwork(t *testing.T) {
	t.Parallel()
	t.Run("passes", func(t *testing.T) {
		t.Parallel()
		p := &policy{enforceAfter: 3, blockedAddrs: map[string]bool{"10.43.0.1:443": true}}
		g := testGate(t, p, 5)
		nw := &fakeNet{}
		l := &Launcher{Spec: testSpec(t), Net: nw, VMM: &fakeVMM{report: 0}, Console: &syncBuf{}, Grace: time.Second, Format: noFormat, Gate: g}
		if code, err := l.Run(t.Context()); err != nil || code != 0 {
			t.Fatalf("Run = %d, %v", code, err)
		}
		if nw.connected != 1 || !p.enforced() {
			t.Fatalf("connected=%d enforced=%t; want a connection only after the proof", nw.connected, p.enforced())
		}
	})
	t.Run("fails", func(t *testing.T) {
		t.Parallel()
		p := &policy{enforceAfter: 1 << 30, blockedAddrs: map[string]bool{"10.43.0.1:443": true}}
		g := testGate(t, p, 1)
		nw, vmm := &fakeNet{}, &fakeVMM{report: 0}
		l := &Launcher{Spec: testSpec(t), Net: nw, VMM: vmm, Console: &syncBuf{}, Grace: time.Second, Format: noFormat, Gate: g}
		code, err := l.Run(t.Context())
		var le *Error
		if !errors.As(err, &le) || le.Reason != ReasonPolicyNotEnforced || code != LaunchFailedExit {
			t.Fatalf("Run = %d, %v; want %s", code, err, ReasonPolicyNotEnforced)
		}
		if nw.connected != 0 {
			t.Fatalf("the machine joined the network %d times with no proof", nw.connected)
		}
		if !vmm.stopped {
			t.Fatal("the machine was not stopped")
		}
	})
}

// TestReasonMatchesTheCondition keeps the launcher reason and the reason of the
// condition on the Sandbox equal.
func TestReasonMatchesTheCondition(t *testing.T) {
	t.Parallel()
	if string(ReasonPolicyNotEnforced) != setecv1alpha1.ReasonNetworkPolicyNotEnforced {
		t.Fatalf("launcher %q, api %q", ReasonPolicyNotEnforced, setecv1alpha1.ReasonNetworkPolicyNotEnforced)
	}
}

func TestQueryDNSTalksToAServer(t *testing.T) {
	t.Parallel()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Skip("no loopback UDP:", err)
	}
	defer func() { _ = pc.Close() }()
	go func() {
		buf := make([]byte, 512)
		n, addr, err := pc.ReadFrom(buf)
		if err != nil || n < 12 {
			return
		}
		buf[2] |= 0x80 // QR: a response
		_, _ = pc.WriteTo(buf[:n], addr)
	}()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if err := queryDNS(ctx, pc.LocalAddr().String()); err != nil {
		t.Fatalf("queryDNS = %v", err)
	}
	// Nothing listens: a timeout, so unreachable.
	dead, _ := net.ListenPacket("udp", "127.0.0.1:0")
	addr := dead.LocalAddr().String()
	_ = dead.Close()
	ctx2, cancel2 := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel2()
	if err := queryDNS(ctx2, addr); err == nil {
		t.Fatal("queryDNS to a closed port reported an answer")
	}
}
