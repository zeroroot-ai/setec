// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package netpol

import (
	"fmt"
	"net"
	"net/netip"
	"time"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
)

// DefaultGateWait is the default bound of the policy gate.
const DefaultGateWait = 30 * time.Second

// GateConfig is the cluster-wide setting of the policy gate: the wait of the
// launcher for the network plugin to enforce the policy of a Sandbox
// (docs/design/network-policy.md).
type GateConfig struct {
	// Enabled turns the gate on. With it off, no launcher waits, and a
	// Sandbox gets no condition NetworkPolicyEnforced.
	Enabled bool
	// Wait bounds the wait of a launcher. Zero takes DefaultGateWait.
	Wait time.Duration
	// APIServer is host:port of the Kubernetes API Service, such as
	// 10.96.0.1:443. It is the destination that the gate checks for a block
	// when the policy must block it, because it listens in each cluster. Empty
	// says that the operator does not know it.
	APIServer string
	// ExtraBlocked and ExtraAllowed are further ip:port destinations of an
	// operator. A TCP connect to a blocked one must fail, and a TCP connect to
	// an allowed one must work. The destination must accept a connection when
	// no policy blocks it.
	ExtraBlocked, ExtraAllowed []string
}

// Gate is the probe list for the launcher Pod of one Sandbox. It is the content
// of the policy gate in the launcher spec.
type Gate struct {
	Wait    time.Duration
	Blocked []string
	Allowed []string
}

// Validate reports the first problem of the setting. It runs at start of the
// operator, so a mistyped destination stops the operator and is not a failure of
// each Sandbox.
func (g GateConfig) Validate() error {
	for _, a := range append(append([]string{}, g.ExtraBlocked...), g.ExtraAllowed...) {
		if _, err := parseHostPort(a); err != nil {
			return err
		}
	}
	if g.APIServer != "" {
		if _, err := parseHostPort(g.APIServer); err != nil {
			return err
		}
	}
	if g.Wait < 0 {
		return fmt.Errorf("netpol: the policy gate wait must not be negative, got %s", g.Wait)
	}
	return nil
}

func parseHostPort(s string) (netip.AddrPort, error) {
	host, port, err := net.SplitHostPort(s)
	if err != nil {
		return netip.AddrPort{}, fmt.Errorf("netpol: policy gate destination %q is not ip:port: %w", s, err)
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return netip.AddrPort{}, fmt.Errorf("netpol: policy gate destination %q: the host must be an IP address: %w", s, err)
	}
	ap, err := netip.ParseAddrPort(net.JoinHostPort(addr.String(), port))
	if err != nil {
		return netip.AddrPort{}, fmt.Errorf("netpol: policy gate destination %q: %w", s, err)
	}
	return ap, nil
}

// GateFor returns the probes for the Pod of sb, or nil when the gate is off or
// no probe exists.
//
// The blocked destination is the Kubernetes API Service, when the policy
// blocks it: under mode none each destination is blocked, and under the
// permissive modes the reserved ranges are blocked, so the Service is blocked
// when its address is in them. The allowed destinations are the resolvers that
// the policy lets the Pod reach, in each permissive mode, because the policy
// always allows them. Under mode none nothing is allowed.
func (c Config) GateFor(g GateConfig, sb *setecv1alpha1.Sandbox, class *setecv1alpha1.SandboxClass) (*Gate, error) {
	if !g.Enabled {
		return nil, nil
	}
	mode, _ := effectivePosture(sb, class)
	var exempt []string
	if class != nil {
		exempt = class.Spec.EgressExemptCIDRs
	}
	reserved, err := subtractExempt(c.ReservedCIDRs, exempt)
	if err != nil {
		return nil, err
	}
	out := &Gate{Wait: g.Wait}
	if out.Wait == 0 {
		out.Wait = DefaultGateWait
	}
	if g.APIServer != "" {
		ap, perr := parseHostPort(g.APIServer)
		if perr != nil {
			return nil, perr
		}
		if mode == setecv1alpha1.NetworkModeNone || inPrefixes(ap.Addr(), reserved) {
			out.Blocked = append(out.Blocked, "tcp://"+ap.String())
		}
	}
	for _, b := range g.ExtraBlocked {
		out.Blocked = append(out.Blocked, "tcp://"+b)
	}
	if mode != setecv1alpha1.NetworkModeNone {
		split, rerr := c.resolverSplit(class, reserved)
		if rerr != nil {
			return nil, rerr
		}
		for _, ip := range split.pod {
			out.Allowed = append(out.Allowed, "dns://"+net.JoinHostPort(ip, "53"))
		}
		for _, a := range g.ExtraAllowed {
			out.Allowed = append(out.Allowed, "tcp://"+a)
		}
	}
	if len(out.Blocked) == 0 && len(out.Allowed) == 0 {
		return nil, nil
	}
	return out, nil
}

// inPrefixes reports whether addr is in one of the CIDRs.
func inPrefixes(addr netip.Addr, cidrs []string) bool {
	addr = addr.Unmap()
	for _, c := range cidrs {
		if p, err := netip.ParsePrefix(c); err == nil && p.Masked().Contains(addr) {
			return true
		}
	}
	return false
}
