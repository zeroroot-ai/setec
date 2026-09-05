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
// Independent of AllowTCG, containerd must register the "kata-qemu" CRI
// runtime handler. KVM (or TCG) says the node COULD run a VM; the handler
// registration says containerd WILL accept one. A node with hardware support
// and no handler fails every RunPodSandbox with `no runtime for "kata-qemu"
// is configured` (setec#243), so it must not carry the capability label.
//
// No binary lookup is performed: the kata shim lives wherever the handler's
// runtime_type points, and containerd — not this probe — resolves it.
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
	details := map[string]string{}
	// tcgNote is non-empty when the node reaches Available only through the
	// TCG fallback; it is carried into the final Reason so the operator can
	// still see that KVM is absent.
	var tcgNote string

	kvmOK, kvmReason := KVMAvailable(p.cfg.FSRoot)
	if !kvmOK {
		if !p.cfg.AllowTCG {
			return CapabilityResult{
				Available: false,
				Reason:    "kata-qemu requires KVM (set AllowTCG to enable software emulation): " + kvmReason,
			}
		}
		// TCG fallback: QEMU can still run in software emulation mode.
		// Surface a reason so the operator knows KVM is absent.
		details["mode"] = "tcg"
		tcgNote = "KVM absent, TCG fallback: " + kvmReason
	} else if mod, loaded := LoadedKVMModule(p.cfg.FSRoot); !loaded {
		if !p.cfg.AllowTCG {
			return CapabilityResult{
				Available: false,
				Reason: "kata-qemu requires a KVM kernel module: " +
					"none of kvm_intel, kvm_amd, or kvm is loaded in /sys/module/",
			}
		}
		details["mode"] = "tcg"
		tcgNote = "KVM absent, TCG fallback: no KVM kernel module loaded"
	} else {
		details["mode"] = "kvm"
		details["kvm_module"] = mod
	}

	// Hardware is only half the question. Ask containerd whether it will
	// accept the handler before claiming the node can run kata-qemu.
	hc := checkContainerdHandler(p.cfg.FSRoot, p.Name())
	details["containerd_handler"] = hc.State
	if !hc.Configured {
		return CapabilityResult{
			Available: false,
			Reason:    "kata-qemu is not runnable on this node: " + hc.Reason,
			Details:   details,
		}
	}

	return CapabilityResult{
		Available: true,
		Reason:    tcgNote,
		Details:   details,
	}
}
