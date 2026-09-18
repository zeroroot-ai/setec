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
	"os/exec"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// TestInstaller_Converges asserts the portable node installer DaemonSet
// (ADR-0003, setec#187) reached a deliberate outcome on every node it
// targets. installChart renders it under SETEC_E2E_INSTALLER=1 and
// waitForInstallReady already waited for every pod to be Ready, which the
// readiness probe grants only on a deliberate outcome; this reads the
// outcome line each pod logged so a silently wedged loop cannot pass.
//
// Three outcomes are correct, and which one a node reports says what the
// node is:
//
//   - converged: a pristine KVM node the installer prepared end to end;
//   - converged-devmapper: a kata-deploy node, whose fc handler asks for
//     the devmapper snapshotter nobody configured (setec#9); the installer
//     supplied the thin-pool and the snapshotter and left the handler and
//     the kata payload to kata-deploy;
//   - idle-foreign-owner: a node whose owner supplies both, such as one
//     booted from the baked AMI.
//
// A node that reports no KVM is wrong on a suite that pre-warms a metal
// node, and a pod with no outcome line at all is a wedged installer.
func TestInstaller_Converges(t *testing.T) {
	if !installerEnabled {
		t.Skip("SETEC_E2E_INSTALLER != 1; the release was installed without the installer DaemonSet (its image must exist in the cluster runtime)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	var ds appsv1.DaemonSet
	if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: testNamespace, Name: chartFullname + "-installer"}, &ds); err != nil {
		t.Fatalf("get installer DaemonSet: %v", err)
	}
	if ds.Status.DesiredNumberScheduled == 0 {
		t.Fatalf("installer DaemonSet targets no node; a suite that pre-warms a metal node must have one")
	}

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
		case strings.Contains(logs, "outcome=converged-devmapper"):
			t.Logf("%s: supplied the devmapper thin-pool and snapshotter beside a foreign kata-fc handler", pod.Name)
		case strings.Contains(logs, "outcome=converged"):
			t.Logf("%s: converged", pod.Name)
		case strings.Contains(logs, "outcome=idle-foreign-owner"):
			t.Logf("%s: stood down (kata-fc and its snapshotter owned externally on this node)", pod.Name)
		case strings.Contains(logs, "outcome=idle-no-kvm"):
			t.Errorf("%s: reports no KVM on an e2e host that must have KVM", pod.Name)
		default:
			t.Errorf("%s: no deliberate outcome in logs:\n%s", pod.Name, logs)
		}
	}
}
