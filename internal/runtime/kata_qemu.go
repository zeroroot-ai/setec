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
	"errors"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// ErrUnknownKataParam is returned by KataQEMUDispatcher.MutatePod when the
// params map contains keys that are not recognised kata-qemu hypervisor
// parameters.  The error message lists all unknown keys.
var ErrUnknownKataParam = errors.New("unknown kata-qemu parameter")

// kataAnnotation maps a SandboxClass runtime.params key to the corresponding
// kata-containers hypervisor annotation.  Only keys present in this map are
// accepted; all others produce ErrUnknownKataParam.
var kataAnnotation = map[string]string{
	"vcpus":  "io.katacontainers.config.hypervisor.default_vcpus",
	"memory": "io.katacontainers.config.hypervisor.default_memory",
}

// KataQEMUDispatcher implements Dispatcher for the Kata Containers + QEMU
// backend ("kata-qemu").
//
// Default overhead (when BackendConfig.DefaultOverhead is nil):
//
//	memory: 128Mi
//	cpu:    250m
//
// These values cover the kata-agent, the QEMU VMM process, and the guest
// kernel measured on a baseline QEMU VM with 1 vCPU and 128 MiB of guest RAM.
// Override via BackendConfig.DefaultOverhead in Helm values when your workloads
// require different reservations.
//
// MutatePod translates the following SandboxClass runtime.params keys into
// kata-containers hypervisor annotations on the Pod:
//
//	vcpus  → io.katacontainers.config.hypervisor.default_vcpus
//	memory → io.katacontainers.config.hypervisor.default_memory
//
// Any unrecognised key causes MutatePod to return ErrUnknownKataParam listing
// all bad keys; the pod is not mutated in that case.  The operation is
// idempotent: calling MutatePod twice with the same params produces the same
// annotation set.
type KataQEMUDispatcher struct {
	cfg BackendConfig
}

// NewKataQEMUDispatcher returns a KataQEMUDispatcher configured with cfg.
func NewKataQEMUDispatcher(cfg BackendConfig) *KataQEMUDispatcher {
	return &KataQEMUDispatcher{cfg: cfg}
}

// Name implements Dispatcher.
func (d *KataQEMUDispatcher) Name() string { return BackendKataQEMU }

// RuntimeClassName implements Dispatcher.
func (d *KataQEMUDispatcher) RuntimeClassName() string { return d.cfg.RuntimeClassName }

// NodeAffinity implements Dispatcher.  It requires the node label
// setec.zeroroot.ai/runtime.kata-qemu=true and kubernetes.io/os=linux.
func (d *KataQEMUDispatcher) NodeAffinity() *corev1.NodeAffinity {
	return requiredRuntimeNodeAffinity(runtimeAffinityLabel(BackendKataQEMU))
}

// Overhead implements Dispatcher.  Returns BackendConfig.DefaultOverhead when
// set, otherwise the documented defaults of 128Mi memory and 250m CPU.
func (d *KataQEMUDispatcher) Overhead() corev1.ResourceList {
	if d.cfg.DefaultOverhead != nil {
		return d.cfg.DefaultOverhead
	}
	return corev1.ResourceList{
		corev1.ResourceMemory: resource.MustParse("128Mi"),
		corev1.ResourceCPU:    resource.MustParse("250m"),
	}
}

// MutatePod implements Dispatcher.  It translates known SandboxClass
// runtime.params into kata-containers hypervisor annotations.  Unknown keys
// cause an ErrUnknownKataParam error; the pod is not mutated in that case.
// MutatePod is idempotent.
func (d *KataQEMUDispatcher) MutatePod(pod *corev1.Pod, params map[string]string) error {
	// Collect unknown keys first so we can return a complete error without
	// partially mutating the pod.
	var unknown []string
	for k := range params {
		if _, ok := kataAnnotation[k]; !ok {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return fmt.Errorf("%w: %s", ErrUnknownKataParam, strings.Join(unknown, ", "))
	}

	if len(params) == 0 {
		return nil
	}

	if pod.Annotations == nil {
		pod.Annotations = make(map[string]string, len(params))
	}
	for k, v := range params {
		pod.Annotations[kataAnnotation[k]] = v
	}
	return nil
}
