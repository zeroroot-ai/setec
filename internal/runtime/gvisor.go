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

// Package runtime — gvisor.go implements the GVisorDispatcher for the gVisor
// (runsc) isolation backend.
//
// Syscall-filter constraint: gVisor intercepts Linux system calls via its
// Sentry component, which implements a subset of the Linux kernel ABI.  Tool
// authors MUST NOT rely on:
//
//   - ptrace(2) — the Sentry does not support tracing child processes across
//     the sandbox boundary.
//   - BPF-based observability (e.g. bpftrace, Falco's eBPF probe, perf with
//     BPF programs) — /proc/sys/net/core/bpf_jit_enable and the BPF syscall
//     are blocked inside the sandbox.
//   - io_uring — the io_uring(2) interface is not implemented by the Sentry.
//
// See https://gvisor.dev/docs/architecture_guide/security/ for the full list
// of supported and blocked syscalls, and
// https://gvisor.dev/docs/user_guide/compatibility/ for per-syscall
// compatibility notes.

package runtime

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// GVisorDispatcher implements Dispatcher for the gVisor (runsc) backend
// ("gvisor").
//
// Default overhead (when BackendConfig.DefaultOverhead is nil):
//
//	memory: 40Mi
//	cpu:    50m
//
// These values cover the gVisor Sentry process and its initial page-table
// setup.  gVisor's overhead is substantially lower than full-VM backends
// because it shares the host kernel page cache.  Override via
// BackendConfig.DefaultOverhead in Helm values when workloads require
// different reservations.
//
// MutatePod is a no-op: gVisor does not require pod-level annotations beyond
// the RuntimeClass field and the NodeAffinity injected by the reconciler.
type GVisorDispatcher struct {
	cfg BackendConfig
}

// NewGVisorDispatcher returns a GVisorDispatcher configured with cfg.
func NewGVisorDispatcher(cfg BackendConfig) *GVisorDispatcher {
	return &GVisorDispatcher{cfg: cfg}
}

// Name implements Dispatcher.
func (d *GVisorDispatcher) Name() string { return BackendGVisor }

// RuntimeClassName implements Dispatcher.
func (d *GVisorDispatcher) RuntimeClassName() string { return d.cfg.RuntimeClassName }

// NodeAffinity implements Dispatcher.  It requires the node label
// setec.zeroroot.ai/runtime.gvisor=true and kubernetes.io/os=linux.
func (d *GVisorDispatcher) NodeAffinity() *corev1.NodeAffinity {
	return requiredRuntimeNodeAffinity(runtimeAffinityLabel(BackendGVisor))
}

// Overhead implements Dispatcher.  Returns BackendConfig.DefaultOverhead when
// set, otherwise the documented defaults of 40Mi memory and 50m CPU.
func (d *GVisorDispatcher) Overhead() corev1.ResourceList {
	if d.cfg.DefaultOverhead != nil {
		return d.cfg.DefaultOverhead
	}
	return corev1.ResourceList{
		corev1.ResourceMemory: resource.MustParse("40Mi"),
		corev1.ResourceCPU:    resource.MustParse("50m"),
	}
}

// MutatePod implements Dispatcher.  gVisor requires no additional pod
// mutations; this method is a no-op.
func (d *GVisorDispatcher) MutatePod(_ *corev1.Pod, _ map[string]string) error {
	return nil
}
