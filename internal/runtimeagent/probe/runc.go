// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package probe

import "context"

// runcProbe checks whether runc is available on the host node.
//
// runc is the default OCI runtime used by containerd on every standard
// Kubernetes node. It does not require KVM or any other special hardware.
// Detection is limited to a LookPath call — no subprocess is executed.
//
// In practice this probe almost always returns Available=true on a
// Kubernetes node, since runc must be present for containerd to schedule
// regular (non-sandbox) Pods. The probe is included so the node-agent
// produces a complete capability label set covering all four backends.
type runcProbe struct {
	cfg Config
}

func newRuncProbe(cfg Config) Probe {
	return &runcProbe{cfg: cfg}
}

// Name implements Probe.
func (p *runcProbe) Name() string { return "runc" }

// Check implements Probe.
func (p *runcProbe) Check(_ context.Context) CapabilityResult {
	lookPath := p.cfg.lookPath()

	binPath, err := lookPath("runc")
	if err != nil {
		return CapabilityResult{
			Available: false,
			Reason:    "runc binary not found in PATH; this is unexpected on a Kubernetes node",
		}
	}
	return CapabilityResult{
		Available: true,
		Details:   map[string]string{"runc": binPath},
	}
}
