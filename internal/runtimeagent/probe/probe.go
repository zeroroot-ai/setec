/*
Copyright 2026 The Setec Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package probe contains read-only capability probes for the Setec node-agent
// DaemonSet. Each probe reports whether a particular container runtime backend
// is available on the host node by inspecting files, kernel modules, and
// binary presence — it never executes subprocesses or mutates any state.
//
// All probes implement the Probe interface and complete within the budget
// imposed by the caller's context (≤ 2 s in production). Results are
// aggregated by the node-agent into Node labels and a SetecRuntimes condition.
package probe

import (
	"context"
	"os/exec"
)

// CapabilityResult is the structured output of a single probe run.
//
// Backend is populated by the caller from the probe's Name() return value
// rather than set by the probe itself, keeping the result self-contained.
// The JSON tag "-" intentionally omits Backend from the condition message
// body — it is used as the map key in the labels.go serialisation instead.
type CapabilityResult struct {
	Backend   string            `json:"-"`
	Available bool              `json:"available"`
	Reason    string            `json:"reason,omitempty"`
	Details   map[string]string `json:"details,omitempty"`
}

// Probe is the single interface all runtime capability checks implement.
// Implementations must be safe for concurrent use and must complete within
// the budget of the supplied context.
type Probe interface {
	// Name returns a short, lowercase, hyphen-separated identifier for the
	// backend, e.g. "kata-fc", "kata-qemu", "gvisor", "runc". The value is
	// used as the label suffix: setec.zeroroot.ai/runtime.<Name()>.
	Name() string

	// Check performs the capability detection and returns a result whose
	// Backend field is left unset (the caller fills it from Name()). Check
	// must not modify any host state and must honour context cancellation.
	Check(ctx context.Context) CapabilityResult
}

// Config carries injectable dependencies so probes can run against a fake
// filesystem root in tests without needing a real Linux host.
type Config struct {
	// FSRoot is prepended to every absolute path the probes read (e.g.
	// "/dev/kvm", "/sys/module/kvm_intel"). In production this is empty
	// so paths resolve to the real host filesystem. In tests it is the
	// path to a temporary directory containing synthetic files.
	FSRoot string

	// LookPath is called by gvisor and runc probes to locate binaries.
	// If nil it falls back to os.LookPath. Injected in tests to avoid
	// requiring real binaries in PATH.
	LookPath func(file string) (string, error)

	// AllowTCG controls whether the kata-qemu probe reports Available=true
	// when KVM is absent but TCG (software emulation) is detected. Defaults
	// to false, which treats a KVM-less node as unavailable for kata-qemu.
	// Set true only in environments that intentionally rely on TCG.
	AllowTCG bool
}

// lookPath returns cfg.LookPath if set, otherwise os.LookPath.
func (c Config) lookPath() func(string) (string, error) {
	if c.LookPath != nil {
		return c.LookPath
	}
	return exec.LookPath
}

// AllProbes returns the complete ordered list of probes Setec checks on every
// node. The order is stable: kata-fc, kata-qemu, gvisor, runc.
func AllProbes(cfg Config) []Probe {
	return []Probe{
		newKataFCProbe(cfg),
		newKataQEMUProbe(cfg),
		newGVisorProbe(cfg),
		newRuncProbe(cfg),
	}
}
