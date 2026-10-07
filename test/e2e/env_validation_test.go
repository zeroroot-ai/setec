// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

//go:build e2e

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

package e2e

import (
	"errors"
	"os"
	"testing"
	"time"
)

// kvmNodeWait bounds how long the guard waits for the device plugin to
// offer the KVM device on a node. The device plugin registers with the
// kubelet after it starts, so an instant check races it.
const kvmNodeWait = 3 * time.Minute

// TestEnv_KVMPresent is the loud-fail environment guard of the suite. Each
// scenario assumes a Firecracker machine can boot, and Firecracker needs
// hardware virtualization. Without this guard, an incapable environment
// makes the scenarios skip and the suite reports PASS with no coverage.
//
// Against a cluster, at least one node must offer the KVM device of the
// device plugin: a launcher Pod runs nowhere else. Run locally on a KVM box
// (SETEC_E2E_KVM_LOCAL=1, or no cluster), /dev/kvm must exist.
func TestEnv_KVMPresent(t *testing.T) {
	if os.Getenv("SETEC_E2E_KVM_LOCAL") == "1" || k8sClient == nil {
		requireLocalKVM(t)
		return
	}
	requireLauncherNode(t)
}

// requireLauncherNode fails unless a node offers the KVM device of the
// device plugin within kvmNodeWait.
func requireLauncherNode(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(kvmNodeWait)
	for {
		if nodes := sandboxCapableNodes(t); len(nodes) > 0 {
			t.Logf("nodes that offer %s: %v", launcherKVMResource, nodes)
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("FATAL: no node offers %s after %s. The device plugin found no /dev/kvm, "+
				"so no launcher Sandbox can run. Do NOT bypass this check.", launcherKVMResource, kvmNodeWait)
		}
		time.Sleep(10 * time.Second)
	}
}

// requireLocalKVM fails when the machine running the test binary has no
// /dev/kvm. Used for `make e2e` on a bare-metal host.
func requireLocalKVM(t *testing.T) {
	t.Helper()
	if _, err := os.Stat("/dev/kvm"); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			t.Fatal("FATAL: /dev/kvm is missing on this host; no Sandbox can run. " +
				"Install KVM modules (kvm_intel or kvm_amd) or run the suite " +
				"on a bare-metal host. Do NOT bypass this check. " +
				"(If the sandboxes are meant to run on a REMOTE cluster node, " +
				"unset SETEC_E2E_KVM_LOCAL so this guard checks the cluster instead.)")
		}
		t.Fatalf("stat /dev/kvm: %v", err)
	}
}
