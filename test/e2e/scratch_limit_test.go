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
	"github.com/zeroroot-ai/setec/internal/podspec"
)

// TestSandbox_ScratchFullStopsTheSandbox fills the writable layer past the
// scratch size (docs/design/storage.md, setec#172). The writable layer is an
// ext4 disk of the scratch size inside the work volume, so the write fails
// with no space and the workload ends. The work volume of the Pod has a size
// limit, and the node must stay Ready: a full writable layer never fills the
// node. The write goes to /var, which is on the writable layer. /tmp is a
// tmpfs in guest memory and says nothing about the scratch size.
func TestSandbox_ScratchFullStopsTheSandbox(t *testing.T) {
	spec := minimalSpec("/bin/sh", "-c",
		"dd if=/dev/zero of=/var/fill bs=1M count=1536 && sleep 600")
	scratch := resource.MustParse("512Mi")
	spec.Resources.Scratch = &scratch
	sb := newSandbox("e2e-scratch-full", spec)
	createAndCleanup(t, sb)

	key := client.ObjectKeyFromObject(sb)
	// The workload sleeps after the write, so only the full layer can end it
	// before the wait expires.
	final := waitForPhase(t, key, 5*time.Minute, setecv1alpha1.SandboxPhaseFailed)
	t.Logf("the Sandbox stopped: phase=%s reason=%q", final.Status.Phase, final.Status.Reason)

	var pod corev1.Pod
	if err := k8sClient.Get(context.Background(),
		client.ObjectKey{Namespace: sandboxNamespace, Name: sb.Name + "-vm"}, &pod); err == nil {
		limited := false
		for _, v := range pod.Spec.Volumes {
			if v.Name == podspec.LauncherWorkVolume && v.EmptyDir != nil && v.EmptyDir.SizeLimit != nil {
				limited = true
			}
		}
		if !limited {
			t.Errorf("the work volume of the Pod has no size limit")
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
