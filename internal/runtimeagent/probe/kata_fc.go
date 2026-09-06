// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package probe

import "context"

// kataFCProbe checks whether the kata-fc (Kata Containers + Firecracker VMM)
// runtime is available on the host node.
//
// Requirements, all mandatory:
//   - /dev/kvm must exist (hardware virtualisation device node).
//   - A KVM kernel module must be loaded: /sys/module/kvm_intel or
//     /sys/module/kvm_amd on x86, or the built-in /sys/module/kvm entry on
//     arm64 hosts (e.g. AWS Graviton bare metal), where KVM is compiled
//     into the kernel and no vendor module exists.
//   - containerd must register the "kata-fc" CRI runtime handler.
//
// Firecracker does not support TCG (software) emulation, so the hardware
// conditions cannot be traded away. The handler condition is what makes the
// label mean what the operator reads it to mean: KVM says the node COULD run
// a microVM, containerd's registration says it WILL accept one. A node with
// KVM and no handler fails every RunPodSandbox with `no runtime for "kata-fc"
// is configured` (setec#243), so it must not carry the capability label.
//
// No binary lookup is performed: the kata shim lives wherever the handler's
// runtime_type points, and containerd — not this probe — resolves it.
type kataFCProbe struct {
	cfg Config
}

func newKataFCProbe(cfg Config) Probe {
	return &kataFCProbe{cfg: cfg}
}

// Name implements Probe.
func (p *kataFCProbe) Name() string { return "kata-fc" }

// Check implements Probe. It returns Available=true only when the KVM device
// node, a KVM kernel module directory, and a containerd "kata-fc" runtime
// handler registration are all present.
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

	if hc := checkContainerdHandler(p.cfg.FSRoot, p.Name()); !hc.Configured {
		return CapabilityResult{
			Available: false,
			Reason:    "kata-fc is not runnable on this node: " + hc.Reason,
			Details:   map[string]string{"kvm_module": mod, "containerd_handler": hc.State},
		}
	}

	return CapabilityResult{
		Available: true,
		Details:   map[string]string{"kvm_module": mod, "containerd_handler": handlerConfigured},
	}
}
