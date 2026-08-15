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
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// kataFCNodeLabel is the capability label the runtime-agent DaemonSet applies
// to a node whose kata-fc stack probes healthy. It is the cluster-side
// equivalent of "this box has /dev/kvm and can actually boot a microVM".
const kataFCNodeLabel = "setec.zeroroot.ai/runtime.kata-fc"

// TestEnv_KVMPresent is the loud-fail environment guard for the Phase 3
// suite. Every Phase 3 scenario implicitly assumes a Firecracker microVM can
// actually boot, because Kata Containers with Firecracker requires hardware
// virtualisation. Without this guard, an incapable environment makes the
// Phase 3 scenarios all hit t.Skip() and the suite reports PASS with zero
// meaningful coverage — a silent regression hiding underneath green CI.
//
// # Where the check has to look (setec#298)
//
// This guard used to stat /dev/kvm on the host running the test binary. That
// was correct under the old model, where the suite ran on a self-hosted runner
// that WAS the KVM box. Those runners are gone (#161). Today the binary runs in
// an ARC pod in staging while the microVM boots on a separate metal node, so
// /dev/kvm is legitimately absent from the test's own filesystem: the old check
// would have failed every ARC run for the wrong reason, which is why wiring it
// unchanged into CI would have produced a false alarm rather than a guard.
//
// So the guard asks the question where the answer lives:
//
//   - Running against a cluster (the ARC/CI case): at least one node must carry
//     the runtime-agent's kata-fc capability label. This mirrors the
//     `Preflight — a kata-fc-capable node exists` step in e2e.yml, but in Go,
//     so it gates the suite rather than only the workflow.
//   - Running locally on a KVM box (`make e2e`): /dev/kvm must exist. Kept
//     because that is still a real workflow and a missing /dev/kvm there is a
//     genuine misconfiguration.
//
// Set SETEC_E2E_KVM_LOCAL=1 to force the local check (useful when the test
// binary and the sandbox host are deliberately the same machine).
func TestEnv_KVMPresent(t *testing.T) {
	if os.Getenv("SETEC_E2E_KVM_LOCAL") == "1" {
		requireLocalKVM(t)
		return
	}

	// The suite binds k8sClient in TestMain for every cluster-backed run. If it
	// is nil the suite is running without a cluster at all, in which case the
	// local check is the only meaningful one.
	if k8sClient == nil {
		requireLocalKVM(t)
		return
	}
	requireKataFCCapableNode(t)
}

// requireLocalKVM fails when the machine running the test binary has no
// /dev/kvm. Used for `make e2e` on a bare-metal host.
func requireLocalKVM(t *testing.T) {
	t.Helper()
	if _, err := os.Stat("/dev/kvm"); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			t.Fatal("FATAL: /dev/kvm is missing on this host; Phase 3 cannot run. " +
				"Install KVM modules (kvm_intel or kvm_amd) or run the suite " +
				"on a bare-metal host. Do NOT bypass this check. " +
				"(If the sandboxes are meant to run on a REMOTE cluster node, " +
				"unset SETEC_E2E_KVM_LOCAL so this guard checks the cluster instead.)")
		}
		t.Fatalf("stat /dev/kvm: %v", err)
	}
}

// requireKataFCCapableNode fails when no node in the target cluster advertises
// the kata-fc capability label, which is what the runtime-agent sets once the
// node's kata-fc stack probes healthy.
func requireKataFCCapableNode(t *testing.T) {
	t.Helper()
	ctx := context.Background()

	var nodes corev1.NodeList
	if err := k8sClient.List(ctx, &nodes, client.MatchingLabels{kataFCNodeLabel: "true"}); err != nil {
		t.Fatalf("list nodes labelled %s=true: %v", kataFCNodeLabel, err)
	}
	if len(nodes.Items) > 0 {
		return
	}

	// Nothing is capable. Report what the cluster actually looks like, because
	// the usual causes are distinguishable and lead to different places.
	var all corev1.NodeList
	if err := k8sClient.List(ctx, &all); err != nil {
		t.Fatalf("no node carries %s=true, and listing all nodes failed: %v", kataFCNodeLabel, err)
	}
	var detail strings.Builder
	for _, n := range all.Items {
		var runtimeLabels []string
		for k, v := range n.Labels {
			if strings.HasPrefix(k, "setec.zeroroot.ai/runtime.") {
				runtimeLabels = append(runtimeLabels, fmt.Sprintf("%s=%s", k, v))
			}
		}
		if len(runtimeLabels) == 0 {
			runtimeLabels = []string{"<no setec runtime labels>"}
		}
		detail.WriteString(fmt.Sprintf("\n  %s: %s", n.Name, strings.Join(runtimeLabels, " ")))
	}

	t.Fatalf("FATAL: no node carries %s=true, so no Firecracker microVM can boot and "+
		"Phase 3 would silently skip into a green run. Do NOT bypass this check.\n"+
		"Nodes seen (%d):%s\n"+
		"Likely causes: the metal NodePool is scaled to zero and nothing has provisioned it; "+
		"kata-deploy has not run on the node; or the runtime-agent probe has not labelled it yet.",
		kataFCNodeLabel, len(all.Items), detail.String())
}
