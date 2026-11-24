// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package launcher

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/zeroroot-ai/setec/internal/errwrap"
)

// PolicyGateSpec makes the launcher hold the machine off the network until a
// probe shows that the network plugin enforces the NetworkPolicy of the Pod
// (docs/design/network-policy.md). The operator writes it only for the Pod of a
// Sandbox.
//
// A probe is a URL: tcp://host:port or dns://host:port. Each host is an IP
// literal.
type PolicyGateSpec struct {
	// WaitSeconds bounds the wait. The Pod fails after it.
	WaitSeconds int `json:"waitSeconds"`
	// Blocked are destinations that the policy must block. A TCP connect to
	// each must fail. The destination must accept a connection when no policy
	// blocks it, such as a service that listens, because a plugin may answer a
	// blocked connection with a refusal.
	Blocked []string `json:"blocked,omitempty"`
	// Allowed are destinations that the policy must allow. The allow side is
	// proven when at least one of them answers. A dns probe sends a DNS query
	// over UDP and takes any answer. A tcp probe takes a connection. The launcher
	// waits for an answer only for AllowGrace after the block is proven, because
	// a cluster may not reach any of them for a reason that is not the policy,
	// such as an air gap. It then goes on and logs that no answer came.
	Allowed []string `json:"allowed,omitempty"`
}

// ReasonPolicyNotEnforced is the Reason of a Pod whose policy gate ended
// without proof. It is the value of the condition NetworkPolicyEnforced on the
// Sandbox (api/v1alpha1.ReasonNetworkPolicyNotEnforced), and a test keeps the
// two equal.
const ReasonPolicyNotEnforced Reason = "NetworkPolicyNotEnforced"

// GateMarkerFile is the file in the work directory that the launcher writes when
// the gate has passed. The readiness probe of the launcher container needs it,
// so the Pod is not Ready, and the Sandbox is not Running, before the proof.
const GateMarkerFile = "policy-enforced"

// The schemes of a probe.
const (
	schemeTCP = "tcp"
	schemeDNS = "dns"
)

// DefaultAllowGrace is the default of Gate.AllowGrace.
const DefaultAllowGrace = 10 * time.Second

// Gate probes the network until the policy is proven in force.
type Gate struct {
	Spec PolicyGateSpec
	// Marker is the file that Wait writes when the gate passes.
	Marker string
	// AllowGrace is how long the gate waits for an allowed destination to answer
	// after the block is proven. Zero takes DefaultAllowGrace.
	AllowGrace time.Duration
	// Log receives the line that says the allow side is not proven. Nil uses
	// standard error.
	Log io.Writer
	// Interval is the time between two rounds. Zero takes 500 ms.
	Interval time.Duration
	// ProbeTimeout bounds one probe. Zero takes one second.
	ProbeTimeout time.Duration
	// Dial opens a TCP connection. Nil uses a net.Dialer.
	Dial func(ctx context.Context, address string) (net.Conn, error)
	// Query sends one DNS query to address and returns nil on any answer.
	// Nil sends a real query over UDP.
	Query func(ctx context.Context, address string) error
}

// NewGate returns the gate of spec, or nil when the spec has none.
func NewGate(s *Spec) *Gate {
	if s.PolicyGate == nil {
		return nil
	}
	return &Gate{Spec: *s.PolicyGate, Marker: GateMarkerPath(s)}
}

// GateMarkerPath is the path of the marker of the gate of s.
func GateMarkerPath(s *Spec) string { return filepath.Join(s.WorkDir, GateMarkerFile) }

// GatePassed reports whether the readiness of s may pass: the spec has no gate,
// or the gate wrote its marker.
func GatePassed(s *Spec) bool {
	if s.PolicyGate == nil {
		return true
	}
	_, err := os.Stat(GateMarkerPath(s))
	return err == nil
}

func (g *Gate) interval() time.Duration {
	if g.Interval > 0 {
		return g.Interval
	}
	return 500 * time.Millisecond
}

func (g *Gate) probeTimeout() time.Duration {
	if g.ProbeTimeout > 0 {
		return g.ProbeTimeout
	}
	return time.Second
}

// Wait returns nil when each blocked destination is blocked and an allowed
// destination answers, and it then writes the marker. When the block is proven
// and no allowed destination answers within AllowGrace, it also returns nil, and
// logs that the allow side is not proven. A gate with no blocked destination
// takes the grace from its start. It returns a *Error with
// ReasonPolicyNotEnforced when the bound ends before the block is proven. The
// message says which destination failed its check in the last round.
func (g *Gate) Wait(ctx context.Context) error {
	if err := g.Spec.validate(); err != nil {
		return fail(ReasonBadSpec, err)
	}
	wait := time.Duration(g.Spec.WaitSeconds) * time.Second
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	grace := g.AllowGrace
	if grace <= 0 {
		grace = DefaultAllowGrace
	}
	start := time.Now()
	var since time.Time
	var last []string
	for {
		var denyOK, allowOK bool
		denyOK, allowOK, last = g.round(ctx)
		switch {
		case !denyOK:
			since = time.Time{}
		case since.IsZero():
			since = time.Now()
			if len(g.Spec.Blocked) == 0 {
				since = start
			}
		}
		if denyOK && allowOK {
			return g.pass()
		}
		if denyOK && time.Since(since) >= grace {
			g.logf("setec-launcher: policy gate: the block is proven, and no allowed destination answered within %s (%s). "+
				"The machine starts. Check that the cluster reaches them.", grace, strings.Join(g.Spec.Allowed, ", "))
			return g.pass()
		}
		select {
		case <-ctx.Done():
			if errors.Is(context.Cause(ctx), context.DeadlineExceeded) {
				return fail(ReasonPolicyNotEnforced, fmt.Errorf(
					"the network policy was not proven in force after %s, so the machine got no network access: %s",
					wait, strings.Join(last, "; ")))
			}
			return errwrap.Wrap(context.Cause(ctx), "context.Cause")
		case <-time.After(g.interval()):
		}
	}
}

func (g *Gate) logf(format string, args ...any) {
	w := g.Log
	if w == nil {
		w = os.Stderr
	}
	_, _ = fmt.Fprintf(w, format+"\n", args...)
}

// pass writes the marker.
func (g *Gate) pass() error {
	if g.Marker == "" {
		return nil
	}
	if err := os.WriteFile(g.Marker, nil, 0o600); err != nil {
		return fail(ReasonPolicyNotEnforced, fmt.Errorf("write the gate marker: %w", err))
	}
	return nil
}

// round checks each destination once. It says whether each blocked destination
// is blocked, whether an allowed one answers (true with none listed), and a line
// for each check that failed.
func (g *Gate) round(ctx context.Context) (denyOK, allowOK bool, failed []string) {
	denyOK = true
	for _, p := range g.Spec.Blocked {
		if g.reachable(ctx, p) {
			denyOK = false
			failed = append(failed, p+" is reachable and the policy must block it")
		}
	}
	allowOK = len(g.Spec.Allowed) == 0
	for _, p := range g.Spec.Allowed {
		if g.reachable(ctx, p) {
			allowOK = true
			break
		}
	}
	if !allowOK {
		failed = append(failed, fmt.Sprintf("none of the destinations that the policy must allow answered (%s)",
			strings.Join(g.Spec.Allowed, ", ")))
	}
	return denyOK, allowOK, failed
}

// reachable reports whether the destination p answers.
func (g *Gate) reachable(ctx context.Context, p string) bool {
	scheme, addr, _ := splitProbe(p)
	ctx, cancel := context.WithTimeout(ctx, g.probeTimeout())
	defer cancel()
	if scheme == schemeDNS {
		q := g.Query
		if q == nil {
			q = queryDNS
		}
		return q(ctx, addr) == nil
	}
	dial := g.Dial
	if dial == nil {
		dial = func(ctx context.Context, address string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, schemeTCP, address)
		}
	}
	c, err := dial(ctx, addr)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

// validate reports the first problem of the gate spec.
func (g PolicyGateSpec) validate() error {
	if g.WaitSeconds < 1 {
		return errors.New("launcher: policyGate.waitSeconds must be at least 1")
	}
	for _, p := range append(append([]string{}, g.Blocked...), g.Allowed...) {
		scheme, _, err := splitProbe(p)
		if err != nil {
			return err
		}
		if scheme == schemeDNS && slices.Contains(g.Blocked, p) {
			return fmt.Errorf("launcher: policyGate: %q is a dns probe, and a blocked destination is a tcp probe", p)
		}
	}
	return nil
}

// splitProbe reads a probe URL into its scheme and its host:port address.
func splitProbe(p string) (scheme, addr string, err error) {
	u, err := url.Parse(p)
	if err != nil || (u.Scheme != schemeTCP && u.Scheme != schemeDNS) || u.Host == "" || u.Port() == "" {
		return "", "", fmt.Errorf("launcher: policyGate probe %q is not tcp://host:port or dns://host:port", p)
	}
	return u.Scheme, u.Host, nil
}

// queryDNS sends a query for the root name servers to address over UDP and
// returns nil on a well formed answer. The content of the answer does not
// matter: any answer shows that the packet went out and came back.
func queryDNS(ctx context.Context, address string) error {
	var d net.Dialer
	c, err := d.DialContext(ctx, "udp", address)
	if err != nil {
		return errwrap.Wrap(err, "net.Dialer.DialContext")
	}
	defer func() { _ = c.Close() }()
	if dl, ok := ctx.Deadline(); ok {
		_ = c.SetDeadline(dl)
	}
	id := make([]byte, 2)
	if _, err := rand.Read(id); err != nil {
		return errwrap.Wrap(err, "rand.Read")
	}
	// Header: id, flags (recursion desired), one question. Question: the root
	// name, type NS, class IN.
	q := []byte{id[0], id[1], 0x01, 0x00, 0, 1, 0, 0, 0, 0, 0, 0, 0, 0, 2, 0, 1}
	if _, err := c.Write(q); err != nil {
		return errwrap.Wrap(err, "net.Conn.Write")
	}
	buf := make([]byte, 512)
	n, err := c.Read(buf)
	if err != nil {
		return errwrap.Wrap(err, "net.Conn.Read")
	}
	if n < 12 || buf[0] != id[0] || buf[1] != id[1] || buf[2]&0x80 == 0 {
		return errors.New("not a DNS answer")
	}
	return nil
}
