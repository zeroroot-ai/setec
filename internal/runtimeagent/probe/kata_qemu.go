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

// kataQEMUProbe checks whether the kata-qemu (Kata Containers + QEMU VMM)
// runtime is available on the host node.
//
// Unlike kata-fc, QEMU supports TCG software emulation in addition to
// hardware-accelerated KVM. The probe behaviour depends on Config.AllowTCG:
//
//   - AllowTCG=false (default): the KVM device and a KVM kernel module
//     (kvm_intel/kvm_amd on x86, built-in kvm on arm64) must both be
//     present. Without KVM the result is Available=false.
//
//   - AllowTCG=true: if KVM is absent the probe still returns Available=true
//     with Details["mode"]="tcg" and a Reason noting the fallback. This is
//     useful for CI environments or developer workstations that intentionally
//     use software emulation, but should be disabled in production.
//
// No binary lookup is performed: kata-qemu availability is determined by
// hardware/kernel state alone.
type kataQEMUProbe struct {
	cfg Config
}

func newKataQEMUProbe(cfg Config) Probe {
	return &kataQEMUProbe{cfg: cfg}
}

// Name implements Probe.
func (p *kataQEMUProbe) Name() string { return "kata-qemu" }

// Check implements Probe.
func (p *kataQEMUProbe) Check(_ context.Context) CapabilityResult {
	kvmOK, kvmReason := KVMAvailable(p.cfg.FSRoot)

	if !kvmOK {
		if p.cfg.AllowTCG {
			// TCG fallback: QEMU can still run in software emulation mode.
			// Surface a reason so the operator knows KVM is absent.
			return CapabilityResult{
				Available: true,
				Reason:    "KVM absent, TCG fallback: " + kvmReason,
				Details:   map[string]string{"mode": "tcg"},
			}
		}
		return CapabilityResult{
			Available: false,
			Reason:    "kata-qemu requires KVM (set AllowTCG to enable software emulation): " + kvmReason,
		}
	}

	mod, loaded := LoadedKVMModule(p.cfg.FSRoot)
	if !loaded {
		if p.cfg.AllowTCG {
			return CapabilityResult{
				Available: true,
				Reason:    "KVM absent, TCG fallback: no KVM kernel module loaded",
				Details:   map[string]string{"mode": "tcg"},
			}
		}
		return CapabilityResult{
			Available: false,
			Reason: "kata-qemu requires a KVM kernel module: " +
				"none of kvm_intel, kvm_amd, or kvm is loaded in /sys/module/",
		}
	}

	return CapabilityResult{
		Available: true,
		Details:   map[string]string{"mode": "kvm", "kvm_module": mod},
	}
}
