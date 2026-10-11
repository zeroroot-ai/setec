// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

//go:build e2e

package e2e

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
// the cluster has none. The CPU of a GitHub-hosted runner is never m8i
// (setec#271), so exit-test-launcher names this test in SKIP_PATTERN. `make
// e2e-cpu-template` runs it against the setec installed on a cluster with m8i
// nodes (SETEC_E2E_INSTALLED_NAMESPACE), and the hosted exit test
// exit-test-setec-cpu-template runs that on staging each day.
func TestLauncher_CPUTemplateSnapshotLoads(t *testing.T) {
	family := os.Getenv("SETEC_E2E_CPU_TEMPLATE")
	if family == "" {
		family = "m8i"
	}
	instanceType := nodeInstanceType(t, family)

	// The names are fixed. On a cluster that the suite does not own
	// (installed mode) a killed run leaves them behind, so each run first
	// deletes what an earlier run left.
	clsName := "e2e-cpu-" + family + "-" + testNamespace
	deleteCPUTemplateLeftovers(t, clsName)
	t.Cleanup(func() {
		snap := &setecv1alpha1.Snapshot{}
		snap.Namespace, snap.Name = sandboxNamespace, "ct-snap"
		_ = k8sClient.Delete(context.Background(), snap)
	})

	cls := newSandboxClass(clsName, setecv1alpha1.SandboxClassSpec{
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

// deleteCPUTemplateLeftovers deletes the Sandbox ct-src, the Snapshot
// ct-snap and the class clsName that an earlier run left, and waits until
// they are gone.
func deleteCPUTemplateLeftovers(t *testing.T, clsName string) {
	t.Helper()
	ctx := context.Background()
	sb := &setecv1alpha1.Sandbox{}
	sb.Namespace, sb.Name = sandboxNamespace, "ct-src"
	snap := &setecv1alpha1.Snapshot{}
	snap.Namespace, snap.Name = sandboxNamespace, "ct-snap"
	cls := &setecv1alpha1.SandboxClass{}
	cls.Name = clsName
	objs := []client.Object{sb, snap, cls}
	for _, obj := range objs {
		if err := k8sClient.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
			t.Fatalf("delete the leftover %T %s: %v", obj, client.ObjectKeyFromObject(obj), err)
		}
	}
	deadline := time.Now().Add(defaultWait)
	for _, obj := range objs {
		for {
			err := k8sClient.Get(ctx, client.ObjectKeyFromObject(obj), obj)
			if apierrors.IsNotFound(err) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("the leftover %T %s still exists after %s", obj, client.ObjectKeyFromObject(obj), defaultWait)
			}
			time.Sleep(defaultPoll)
		}
	}
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
