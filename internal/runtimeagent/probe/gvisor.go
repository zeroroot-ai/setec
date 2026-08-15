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

package probe

import (
	"context"
)

// gvisorHandler is the CRI runtime handler name gVisor registers under. It is
// NOT the probe's own name ("gvisor"), which is the setec backend label — the
// containerd table is `containerd.runtimes.runsc`.
const gvisorHandler = "runsc"

// gvisorProbe checks whether the gVisor (runsc) container runtime is
// available on the host node.
//
// Detection strategy (no subprocess execution):
//  1. LookPath("runsc") must succeed — the runtime binary must be on the node.
//  2. containerd must register the `runsc` CRI runtime handler, verified with
//     the same scanner the Kata probes use (checkContainerdHandler).
//
// IT FAILS CLOSED, AND THAT IS A CHANGE (setec#268). This probe used to log a
// warning and return Available:true on binary presence alone whenever it could
// not read a containerd config, and it matched the bare substring "runsc"
// anywhere in a file — a comment mentioning runsc was enough. Both made the
// node advertise setec.zeroroot.ai/runtime.gvisor=true while containerd would
// reject every RunPodSandbox with `no runtime for "runsc" is configured`, which
// is the exact capability lie setec#243 removed from the Kata probes. A
// capability that cannot be verified is not one that can be scheduled onto.
type gvisorProbe struct {
	cfg Config
}

func newGVisorProbe(cfg Config) Probe {
	return &gvisorProbe{cfg: cfg}
}

// Name implements Probe.
func (p *gvisorProbe) Name() string { return "gvisor" }

// Check implements Probe.
func (p *gvisorProbe) Check(_ context.Context) CapabilityResult {
	lookPath := p.cfg.lookPath()

	binPath, err := lookPath("runsc")
	if err != nil {
		return CapabilityResult{
			Available: false,
			Reason:    "runsc binary not found in PATH; install gVisor and ensure runsc is on the node PATH",
		}
	}

	if hc := checkContainerdHandler(p.cfg.FSRoot, gvisorHandler); !hc.Configured {
		return CapabilityResult{
			Available: false,
			Reason:    "gvisor is not runnable on this node: " + hc.Reason,
			Details: map[string]string{
				"runsc":              binPath,
				"containerd_handler": hc.State,
			},
		}
	}

	return CapabilityResult{
		Available: true,
		Details: map[string]string{
			"runsc":              binPath,
			"containerd_handler": handlerConfigured,
		},
	}
}
