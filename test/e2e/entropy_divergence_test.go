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

// TestPhase3_RestoredClonesDivergeInRNG is the setec#72 divergence
// acceptance: two microVMs restored from the SAME Snapshot must not
// produce identical random streams. Each restored clone dumps bytes
// from /dev/urandom; identical output would mean the clones resumed
// with shared CSPRNG state (the exact catastrophic condition the
// entropy reseed on restore exists to prevent).
//
// The test exercises whichever reseed posture the harness installed:
// with snapshots.entropyReseed=require (default) a passing run also
// proves the fail-closed active reseed completed (the restores would
// otherwise not have succeeded); with entropyReseed=off it validates
// the passive virtio-rng mechanism alone. Guest images used by the
// harness must bundle setec-guest-agent when running in require mode.
func TestPhase3_RestoredClonesDivergeInRNG(t *testing.T) {
	if !envtestOK(t) {
		t.Skip("Phase 3 E2E requires a Setec-installed cluster")
	}
	if !phase3Enabled(t) {
		t.Skip("Phase 3 disabled (snapshots.enabled=false); skipping RNG divergence test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), defaultWait)
	defer cancel()

	ns := "p3-rng-diverge"
	createTenantNamespace(ctx, t, ns)

	// Source sandbox stays up long enough to snapshot.
	source := &setecv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "rng-source"},
		Spec: setecv1alpha1.SandboxSpec{
			Image:   "docker.io/library/alpine:3.19",
			Command: []string{"sh", "-c", "sleep 120"},
			Resources: setecv1alpha1.Resources{
				VCPU:   1,
				Memory: resource.MustParse("512Mi"),
			},
			Snapshot: &setecv1alpha1.SandboxSnapshotSpec{
				Create: true,
				Name:   "rng-snap",
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
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "rng-snap"}, snap); err == nil {
			if snap.Status.Phase == setecv1alpha1.SnapshotPhaseReady {
				break
			}
		}
		time.Sleep(3 * time.Second)
	}

	// Restore TWO clones from the same snapshot; each prints a
	// delimited hex dump of fresh kernel randomness.
	dumpCmd := "echo RNG-BEGIN && head -c 64 /dev/urandom | od -A n -t x1 | tr -d ' \\n' && echo && echo RNG-END"
	outputs := make([]string, 2)
	for i := range 2 {
		name := fmt.Sprintf("rng-clone-%d", i)
		clone := &setecv1alpha1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
			Spec: setecv1alpha1.SandboxSpec{
				Image:   "docker.io/library/alpine:3.19",
				Command: []string{"sh", "-c", dumpCmd},
				Resources: setecv1alpha1.Resources{
					VCPU:   1,
					Memory: resource.MustParse("512Mi"),
				},
				SnapshotRef: &setecv1alpha1.SandboxSnapshotRef{Name: "rng-snap"},
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
		outputs[i] = extractRNGDump(string(logs))
		if outputs[i] == "" {
			t.Fatalf("no RNG dump found in %s logs: %s", name, logs)
		}
	}

	if outputs[0] == outputs[1] {
		t.Fatalf("restored clones produced IDENTICAL random streams (%s) — shared CSPRNG state across snapshot restores", outputs[0])
	}
}

// extractRNGDump pulls the hex dump between the RNG-BEGIN/RNG-END
// markers out of the pod logs.
func extractRNGDump(logs string) string {
	m := regexp.MustCompile(`(?s)RNG-BEGIN\s*(.*?)\s*RNG-END`).FindStringSubmatch(logs)
	if len(m) != 2 {
		return ""
	}
	return strings.TrimSpace(m[1])
}
