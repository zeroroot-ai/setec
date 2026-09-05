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
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
)

// TestPhase3_RestoredClonesHaveUniqueIdentity is the setec#189 /
// ADR-0005 invariant-2 acceptance: two sandboxes restored from the
// SAME snapshot template must each observe a distinct machine
// identity (machine-id, boot-id, hostname) and their own CNI-assigned
// Pod IP — no stale network state from snapshot time. Each restored
// clone dumps its identity between ID-BEGIN/ID-END markers; the test
// asserts the two dumps differ field by field and that each clone's
// observed IP matches its Pod's status.podIP.
//
// The vsock-CID uniqueness half of the invariant is enforced
// node-side (the restore fails closed on a collision, so two Ready
// clones imply distinct CIDs) and is additionally pinned by unit
// tests on the node-agent CID registry.
//
// The test exercises whichever uniquify posture the harness
// installed: with snapshots.restoreUniquify=require (default) a
// passing run also proves the fail-closed uniquification completed
// (the restores would otherwise not have succeeded). Guest images
// used by the harness must bundle setec-guest-agent when running in
// require mode.
func TestPhase3_RestoredClonesHaveUniqueIdentity(t *testing.T) {
	if !envtestOK(t) {
		t.Skip("Phase 3 E2E requires a Setec-installed cluster")
	}
	if !phase3Enabled(t) {
		t.Skip("Phase 3 disabled (snapshots.enabled=false); skipping restore-uniquification test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), defaultWait)
	defer cancel()

	ns := "p3-uniquify"
	createTenantNamespace(ctx, t, ns)

	// Source sandbox stays up long enough to snapshot.
	source := &setecv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "uniq-source"},
		Spec: setecv1alpha1.SandboxSpec{
			Image:   "docker.io/library/alpine:3.19",
			Command: []string{"sh", "-c", "sleep 120"},
			Resources: setecv1alpha1.Resources{
				VCPU:   1,
				Memory: resource.MustParse("512Mi"),
			},
			Snapshot: &setecv1alpha1.SandboxSnapshotSpec{
				Create: true,
				Name:   "uniq-snap",
			},
		},
	}
	if err := k8sClient.Create(ctx, source); err != nil {
		t.Fatalf("create source sandbox: %v", err)
	}

	// Wait for the Snapshot CR to go Ready.
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		snap := &setecv1alpha1.Snapshot{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "uniq-snap"}, snap); err == nil {
			if snap.Status.Phase == setecv1alpha1.SnapshotPhaseReady {
				break
			}
		}
		time.Sleep(3 * time.Second)
	}

	// Restore TWO clones from the same snapshot; each prints its
	// machine identity plus the addresses it observes.
	dumpCmd := "echo ID-BEGIN" +
		" && echo machine-id=$(cat /etc/machine-id)" +
		" && echo boot-id=$(cat /proc/sys/kernel/random/boot_id)" +
		" && echo hostname=$(hostname)" +
		" && echo ips=$(ip -o -4 addr show scope global | awk '{print $4}' | cut -d/ -f1 | tr '\\n' ',')" +
		" && echo ID-END"
	type identity struct {
		machineID, bootID, hostname, ips string
	}
	ids := make([]identity, 2)
	for i := range 2 {
		name := fmt.Sprintf("uniq-clone-%d", i)
		clone := &setecv1alpha1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
			Spec: setecv1alpha1.SandboxSpec{
				Image:   "docker.io/library/alpine:3.19",
				Command: []string{"sh", "-c", dumpCmd},
				Resources: setecv1alpha1.Resources{
					VCPU:   1,
					Memory: resource.MustParse("512Mi"),
				},
				SnapshotRef: &setecv1alpha1.SandboxSnapshotRef{Name: "uniq-snap"},
			},
		}
		if err := k8sClient.Create(ctx, clone); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		waitForPhaseCtx(ctx, t, ns, name, setecv1alpha1.SandboxPhaseCompleted, 2*time.Minute)

		logs, err := exec.Command("kubectl", "-n", ns, "logs", name+"-vm").CombinedOutput()
		if err != nil {
			t.Fatalf("kubectl logs %s: %v (%s)", name, err, logs)
		}
		dump := extractIdentityDump(string(logs))
		if dump == "" {
			t.Fatalf("no identity dump found in %s logs: %s", name, logs)
		}
		ids[i] = identity{
			machineID: identityField(dump, "machine-id"),
			bootID:    identityField(dump, "boot-id"),
			hostname:  identityField(dump, "hostname"),
			ips:       identityField(dump, "ips"),
		}
		for field, v := range map[string]string{
			"machine-id": ids[i].machineID,
			"boot-id":    ids[i].bootID,
			"hostname":   ids[i].hostname,
			"ips":        ids[i].ips,
		} {
			if v == "" {
				t.Fatalf("%s: empty %s in identity dump: %s", name, field, dump)
			}
		}

		// The clone must observe its own CNI-assigned Pod IP — no
		// stale snapshot-time address.
		podIP, err := exec.Command("kubectl", "-n", ns, "get", "pod", name+"-vm",
			"-o", "jsonpath={.status.podIP}").CombinedOutput()
		if err != nil {
			t.Fatalf("kubectl get pod %s-vm: %v (%s)", name, err, podIP)
		}
		if ip := strings.TrimSpace(string(podIP)); ip == "" || !strings.Contains(ids[i].ips, ip) {
			t.Fatalf("%s: guest observes IPs %q but the Pod's CNI-assigned IP is %q (stale network identity)",
				name, ids[i].ips, ip)
		}
	}

	// Every identity field must differ between the two clones.
	if ids[0].machineID == ids[1].machineID {
		t.Fatalf("restored clones share machine-id %q", ids[0].machineID)
	}
	if ids[0].bootID == ids[1].bootID {
		t.Fatalf("restored clones share boot-id %q", ids[0].bootID)
	}
	if ids[0].hostname == ids[1].hostname {
		t.Fatalf("restored clones share hostname %q", ids[0].hostname)
	}
	if ids[0].ips == ids[1].ips {
		t.Fatalf("restored clones observe identical addresses %q", ids[0].ips)
	}
}

// extractIdentityDump pulls the text between the ID-BEGIN/ID-END
// markers out of the pod logs.
func extractIdentityDump(logs string) string {
	m := regexp.MustCompile(`(?s)ID-BEGIN\s*(.*?)\s*ID-END`).FindStringSubmatch(logs)
	if len(m) != 2 {
		return ""
	}
	return strings.TrimSpace(m[1])
}

// identityField extracts "key=value" lines from an identity dump.
func identityField(dump, key string) string {
	m := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(key) + `=(.*)$`).FindStringSubmatch(dump)
	if len(m) != 2 {
		return ""
	}
	return strings.TrimSpace(m[1])
}
