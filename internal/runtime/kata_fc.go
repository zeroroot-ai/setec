// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package runtime

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// KataFCDispatcher implements Dispatcher for the Kata Containers + Firecracker
// backend ("kata-fc").
//
// Default overhead (when BackendConfig.DefaultOverhead is nil):
//
//	memory: 128Mi
//	cpu:    250m
//
// These values cover the kata-agent, the Firecracker VMM process, and the
// guest kernel, measured on a baseline Firecracker microVM with 1 vCPU and
// 128 MiB of guest RAM.  Override via the BackendConfig.DefaultOverhead field
// in the operator Helm values if your workloads require different reservations.
type KataFCDispatcher struct {
	cfg BackendConfig
}

// NewKataFCDispatcher returns a KataFCDispatcher configured with cfg.
func NewKataFCDispatcher(cfg BackendConfig) *KataFCDispatcher {
	return &KataFCDispatcher{cfg: cfg}
}

// Name implements Dispatcher.
func (d *KataFCDispatcher) Name() string { return BackendKataFC }

// RuntimeClassName implements Dispatcher.
func (d *KataFCDispatcher) RuntimeClassName() string { return d.cfg.RuntimeClassName }

// NodeAffinity implements Dispatcher.  It requires the node label
// setec.zeroroot.ai/runtime.kata-fc=true and kubernetes.io/os=linux.
func (d *KataFCDispatcher) NodeAffinity() *corev1.NodeAffinity {
	return requiredRuntimeNodeAffinity(runtimeAffinityLabel(BackendKataFC))
}

// Overhead implements Dispatcher.  Returns BackendConfig.DefaultOverhead when
// set, otherwise the documented defaults of 128Mi memory and 250m CPU.
func (d *KataFCDispatcher) Overhead() corev1.ResourceList {
	if !d.cfg.Install {
		// Externally-managed RuntimeClass: we don't control its overhead, so
		// omit it and let RuntimeClass admission apply the class's own (setec#78).
		return nil
	}
	if d.cfg.DefaultOverhead != nil {
		return d.cfg.DefaultOverhead
	}
	return corev1.ResourceList{
		corev1.ResourceMemory: resource.MustParse("128Mi"),
		corev1.ResourceCPU:    resource.MustParse("250m"),
	}
}

// MutatePod implements Dispatcher.  Kata Containers + Firecracker does not
// require any additional pod mutations beyond what the RuntimeClass and
// NodeAffinity provide; this method is a no-op.
func (d *KataFCDispatcher) MutatePod(_ *corev1.Pod, _ map[string]string) error {
	return nil
}
