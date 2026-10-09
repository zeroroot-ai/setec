// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

//go:build e2e

package e2e

import (
	"context"
	"os"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
)

// instanceTypeLabel is the well-known label of the instance type of a node.
const instanceTypeLabel = "node.kubernetes.io/instance-type"

// TestLauncher_CPUTemplateSnapshotLoads loads a snapshot in a class with a
// custom CPU template that the launcher image ships (setec#239). The class
// names the template and the instance type of the template, so each machine
// of the class shows the guest the same CPU features. A snapshot of one
// machine then loads as a new machine of the class.
//
// The template matches one CPU. The test runs on the nodes of that
// instance family (SETEC_E2E_CPU_TEMPLATE, default m8i), and it fails when
// the cluster has none: the CI runners of the e2e workflow are not m8i, so
// the workflow names this test in SKIP_PATTERN. `make e2e-cpu-template` runs
// it on a cluster with m8i nodes.
func TestLauncher_CPUTemplateSnapshotLoads(t *testing.T) {
	family := os.Getenv("SETEC_E2E_CPU_TEMPLATE")
	if family == "" {
		family = "m8i"
	}
	instanceType := nodeInstanceType(t, family)

	cls := newSandboxClass("e2e-cpu-"+family+"-"+testNamespace, setecv1alpha1.SandboxClassSpec{
		Runtime:      &setecv1alpha1.SandboxClassRuntime{Backend: backendLauncher},
		CPUTemplate:  family,
		NodeSelector: map[string]string{instanceTypeLabel: instanceType},
	})
	if err := k8sClient.Create(context.Background(), cls); err != nil {
		t.Fatalf("create the class: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), cls) })
	inClass := func(spec *setecv1alpha1.SandboxSpec) { spec.SandboxClassName = cls.Name }

	src := launcherSandbox("ct-src", "echo before-snapshot > /tmp/marker; sleep 3600")
	inClass(&src.Spec)
	createAndCleanup(t, src)
	waitRunning(t, src, defaultWait)
	before := readGuestIdentity(t, sandboxNamespace, src.Name)

	takeSnapshot(t, src, setecv1alpha1.SandboxSnapshotSpec{Name: "ct-snap"})
	if err := k8sClient.Delete(context.Background(), src); err != nil {
		t.Fatalf("delete ct-src: %v", err)
	}
	waitGone(t, client.ObjectKeyFromObject(src), defaultWait)

	resumed := restoreFrom(t, "ct-src", "ct-snap", inClass)
	took := waitRunning(t, resumed, launcherRestoreWait)
	t.Logf("load of ct-snap with the %s template Running in %s", family, took)
	if got := mustLauncherExec(t, sandboxNamespace, "ct-src", "cat", "/tmp/marker"); got != "before-snapshot" {
		t.Errorf("the state did not survive the load: /tmp/marker = %q", got)
	}
	assertRestoredGuest(t, sandboxNamespace, "ct-src", before)
}

// nodeInstanceType returns the instance type of a node of family that can
// run a Sandbox. It fails the test when no such node exists.
func nodeInstanceType(t *testing.T, family string) string {
	t.Helper()
	capable := map[string]bool{}
	for _, n := range sandboxCapableNodes(t) {
		capable[n] = true
	}
	nodes := &corev1.NodeList{}
	if err := k8sClient.List(context.Background(), nodes); err != nil {
		t.Fatalf("list nodes: %v", err)
	}
	for _, n := range nodes.Items {
		if it := n.Labels[instanceTypeLabel]; capable[n.Name] && strings.HasPrefix(it, family+".") {
			return it
		}
	}
	t.Fatalf("no node that can run a Sandbox has an instance type of the %s family (label %s)", family, instanceTypeLabel)
	return ""
}
