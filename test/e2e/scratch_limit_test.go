//go:build e2e

// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package e2e

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/controller-runtime/pkg/client"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
)

// TestSandbox_ScratchFullStopsTheSandbox fills the scratch volume past its
// size limit (ADR-0146, setec#172). The kubelet must stop the Sandbox, and
// the node must stay Ready: a full scratch volume never fills the node.
func TestSandbox_ScratchFullStopsTheSandbox(t *testing.T) {
	spec := minimalSpec("/bin/sh", "-c",
		"dd if=/dev/zero of=/tmp/fill bs=1M count=1536 && sleep 600")
	scratch := resource.MustParse("512Mi")
	spec.Resources.Scratch = &scratch
	sb := newSandbox("e2e-scratch-full", spec)
	createAndCleanup(t, sb)

	key := client.ObjectKeyFromObject(sb)
	// The workload sleeps after the write, so only the limit can end it
	// before the wait expires.
	final := waitForPhase(t, key, 5*time.Minute, setecv1alpha1.SandboxPhaseFailed)
	t.Logf("the Sandbox stopped: phase=%s reason=%q", final.Status.Phase, final.Status.Reason)

	var pod corev1.Pod
	if err := k8sClient.Get(context.Background(),
		client.ObjectKey{Namespace: sandboxNamespace, Name: sb.Name + "-vm"}, &pod); err == nil {
		limited := false
		for _, v := range pod.Spec.Volumes {
			if v.Name == "scratch" && v.EmptyDir != nil && v.EmptyDir.SizeLimit != nil {
				limited = true
			}
		}
		if !limited {
			t.Errorf("the scratch volume of the Pod has no size limit")
		}
		t.Logf("pod phase=%s reason=%q message=%q", pod.Status.Phase, pod.Status.Reason, pod.Status.Message)
	}

	var nodes corev1.NodeList
	if err := k8sClient.List(context.Background(), &nodes); err != nil {
		t.Fatalf("list nodes: %v", err)
	}
	if len(nodes.Items) == 0 {
		t.Fatal("the cluster has no node")
	}
	for _, n := range nodes.Items {
		for _, c := range n.Status.Conditions {
			if c.Type == corev1.NodeReady && c.Status != corev1.ConditionTrue {
				t.Errorf("node %s is not Ready after the scratch volume filled: %s", n.Name, c.Message)
			}
			if c.Type == corev1.NodeDiskPressure && c.Status == corev1.ConditionTrue {
				t.Errorf("node %s has disk pressure after the scratch volume filled", n.Name)
			}
		}
	}
}
