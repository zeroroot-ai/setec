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
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// TestInstaller_Converges exercises the portable node installer DaemonSet
// (ADR-0003, setec#187): it enables the installer on the deployed release
// with the locally-built image, waits for the DaemonSet to become Ready
// on every targeted node, and asserts each pod reports a deliberate
// outcome (converged, or a stand-down on a node whose kata-fc is owned by
// kata-deploy — the usual state of the k3s dev host).
//
// Gated behind SETEC_E2E_INSTALLER=1 on top of the e2e build tag: the
// installer image (ghcr.io/zeroroot-ai/setec-installer:<tag>) must have
// been built from the working tree (Dockerfile.installer) and imported
// into the cluster runtime, which the standard e2e flow does not do.
//
// The full fresh-node acceptance — a Sandbox with backend kata-fc
// reaching Running on a node that had NO kata components pre-installed —
// requires a pristine KVM node and is exercised by running this suite
// (plus the standard Sandbox scenarios) against such a node with
// SETEC_E2E_INSTALLER=1; on the kata-deploy-owned dev host this test
// proves the stand-down half of the contract instead.
func TestInstaller_Converges(t *testing.T) {
	if os.Getenv("SETEC_E2E_INSTALLER") != "1" {
		t.Skip("SETEC_E2E_INSTALLER != 1; skipping installer DaemonSet e2e (needs the locally-built setec-installer image imported into the cluster runtime)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	// Enable the installer on the existing release. pullPolicy Never so a
	// missed image import fails loud, matching every other component.
	upgrade := exec.Command("helm", "upgrade", helmReleaseName, chartPath,
		"--namespace", testNamespace,
		"--reuse-values",
		"--set", "installer.enabled=true",
		"--set", fmt.Sprintf("installer.image.tag=%s", imageTag),
		"--set", "installer.image.pullPolicy=Never",
		"--wait", "--timeout", "5m",
	)
	upgrade.Stdout = os.Stdout
	upgrade.Stderr = os.Stderr
	if err := upgrade.Run(); err != nil {
		t.Fatalf("helm upgrade enabling installer: %v", err)
	}
	t.Cleanup(func() {
		disable := exec.Command("helm", "upgrade", helmReleaseName, chartPath,
			"--namespace", testNamespace,
			"--reuse-values",
			"--set", "installer.enabled=false",
			"--wait", "--timeout", "3m",
		)
		disable.Stdout = os.Stdout
		disable.Stderr = os.Stderr
		if err := disable.Run(); err != nil {
			t.Logf("cleanup: disabling installer failed: %v", err)
		}
	})

	// DaemonSet fully Ready: every targeted node converged or stood down
	// (the readiness probe only passes on a deliberate outcome).
	dsName := helmReleaseName + "-installer"
	deadline := time.Now().Add(5 * time.Minute)
	for {
		var ds appsv1.DaemonSet
		err := k8sClient.Get(ctx, client.ObjectKey{Namespace: testNamespace, Name: dsName}, &ds)
		if err == nil && ds.Status.DesiredNumberScheduled > 0 &&
			ds.Status.NumberReady == ds.Status.DesiredNumberScheduled {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("installer DaemonSet %s not fully Ready in time (err=%v)", dsName, err)
		}
		time.Sleep(5 * time.Second)
	}

	// Every installer pod must log a deliberate outcome. "converged" and
	// "idle-foreign-owner" are both correct depending on the node's
	// pre-existing ownership; an error state would have kept the pod
	// NotReady above, but assert the outcome line anyway so a silently
	// wedged loop cannot pass.
	var pods corev1.PodList
	if err := k8sClient.List(ctx, &pods,
		client.InNamespace(testNamespace),
		client.MatchingLabels{"app.kubernetes.io/component": "installer"},
	); err != nil {
		t.Fatalf("list installer pods: %v", err)
	}
	if len(pods.Items) == 0 {
		t.Fatal("no installer pods found")
	}
	for _, pod := range pods.Items {
		out, err := exec.Command("kubectl", "logs",
			"--namespace", testNamespace, pod.Name).CombinedOutput()
		if err != nil {
			t.Fatalf("logs for %s: %v (%s)", pod.Name, err, out)
		}
		logs := string(out)
		switch {
		case strings.Contains(logs, "outcome=converged"):
			t.Logf("%s: converged", pod.Name)
		case strings.Contains(logs, "outcome=idle-foreign-owner"):
			t.Logf("%s: stood down (kata-fc owned externally on this node) — correct on a kata-deploy host", pod.Name)
		case strings.Contains(logs, "outcome=idle-no-kvm"):
			t.Errorf("%s: reports no KVM on an e2e host that must have KVM", pod.Name)
		default:
			t.Errorf("%s: no deliberate outcome in logs:\n%s", pod.Name, logs)
		}
	}
}
