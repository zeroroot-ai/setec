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

package runtime

import (
	corev1 "k8s.io/api/core/v1"
)

// isolationLabel is the pod label that MutatePod adds to pods scheduled on
// the runc backend to make the isolation level visible to admission policies,
// audit logs, and security scanners.
const isolationLabel = "setec.zeroroot.ai/isolation"

// isolationContainerOnly is the value written to isolationLabel for runc pods.
// It signals that the workload has container-only isolation (no hypervisor or
// kernel-level boundary), allowing downstream tooling to apply stricter policy.
const isolationContainerOnly = "container-only"

// RuncDispatcher implements Dispatcher for the standard runc OCI runtime
// ("runc").
//
// Default overhead (when BackendConfig.DefaultOverhead is nil):
//
//	zero (empty ResourceList)
//
// runc shares the host kernel and adds no significant per-pod overhead beyond
// the container process itself.  Override via BackendConfig.DefaultOverhead in
// Helm values if accounting requires it.
//
// MutatePod adds the label setec.zeroroot.ai/isolation=container-only to
// pod.ObjectMeta.Labels (creating the map if it is nil).  This label is
// idempotent and intentionally visible to the Kubernetes API server so that
// admission webhooks, OPA policies, and audit systems can identify workloads
// running with container-only isolation.
//
// The devOnly gate (restricting runc to namespaces carrying
// setec.zeroroot.ai/allow-dev-runtimes=true) is enforced by the admission
// webhook (task 13), not here.
type RuncDispatcher struct {
	cfg BackendConfig
}

// NewRuncDispatcher returns a RuncDispatcher configured with cfg.
func NewRuncDispatcher(cfg BackendConfig) *RuncDispatcher {
	return &RuncDispatcher{cfg: cfg}
}

// Name implements Dispatcher.
func (d *RuncDispatcher) Name() string { return BackendRunc }

// RuntimeClassName implements Dispatcher.
func (d *RuncDispatcher) RuntimeClassName() string { return d.cfg.RuntimeClassName }

// NodeAffinity implements Dispatcher.  It requires the node label
// setec.zeroroot.ai/runtime.runc=true and kubernetes.io/os=linux.
func (d *RuncDispatcher) NodeAffinity() *corev1.NodeAffinity {
	return requiredRuntimeNodeAffinity(runtimeAffinityLabel(BackendRunc))
}

// Overhead implements Dispatcher.  Returns BackendConfig.DefaultOverhead when
// set, otherwise an empty ResourceList (zero overhead).
func (d *RuncDispatcher) Overhead() corev1.ResourceList {
	if d.cfg.DefaultOverhead != nil {
		return d.cfg.DefaultOverhead
	}
	return corev1.ResourceList{}
}

// MutatePod implements Dispatcher.  It adds the label
// setec.zeroroot.ai/isolation=container-only to pod.ObjectMeta.Labels,
// creating the labels map if it is nil.  The operation is idempotent.
func (d *RuncDispatcher) MutatePod(pod *corev1.Pod, _ map[string]string) error {
	if pod.Labels == nil {
		pod.Labels = make(map[string]string, 1)
	}
	pod.Labels[isolationLabel] = isolationContainerOnly
	return nil
}
