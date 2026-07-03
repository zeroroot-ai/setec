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

import "context"

// kataFCProbe checks whether the kata-fc (Kata Containers + Firecracker VMM)
// runtime is available on the host node.
//
// Requirements:
//   - /dev/kvm must exist (hardware virtualisation device node).
//   - A KVM kernel module must be loaded: /sys/module/kvm_intel or
//     /sys/module/kvm_amd on x86, or the built-in /sys/module/kvm entry on
//     arm64 hosts (e.g. AWS Graviton bare metal), where KVM is compiled
//     into the kernel and no vendor module exists.
//
// Firecracker does not support TCG (software) emulation, so both conditions
// are mandatory. No binary lookup is performed: kata-fc availability is
// determined entirely by hardware/kernel state.
type kataFCProbe struct {
	cfg Config
}

func newKataFCProbe(cfg Config) Probe {
	return &kataFCProbe{cfg: cfg}
}

// Name implements Probe.
func (p *kataFCProbe) Name() string { return "kata-fc" }

// Check implements Probe. It returns Available=true only when both the KVM
// device node and a KVM kernel module directory are present.
func (p *kataFCProbe) Check(_ context.Context) CapabilityResult {
	kvmOK, kvmReason := KVMAvailable(p.cfg.FSRoot)
	if !kvmOK {
		return CapabilityResult{
			Available: false,
			Reason:    "kata-fc requires KVM: " + kvmReason,
		}
	}

	mod, loaded := LoadedKVMModule(p.cfg.FSRoot)
	if !loaded {
		return CapabilityResult{
			Available: false,
			Reason: "kata-fc requires a KVM kernel module: " +
				"none of kvm_intel, kvm_amd, or kvm is loaded in /sys/module/",
		}
	}

	return CapabilityResult{
		Available: true,
		Details:   map[string]string{"kvm_module": mod},
	}
}
