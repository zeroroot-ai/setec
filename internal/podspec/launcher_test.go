// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package podspec

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func launcherOrFatal(t *testing.T) *corev1.Pod {
	t.Helper()
	pod, err := BuildLauncher(newSandbox(), LauncherOptions{Image: "ghcr.io/zeroroot-ai/setec-launcher:test"})
	if err != nil {
		t.Fatalf("BuildLauncher: %v", err)
	}
	return pod
}

// TestBuildLauncher_IsNotPrivileged pins the launcher Pod of setec#187: one
// container, no privilege, nothing from the host, and the two device
// resources.
func TestBuildLauncher_IsNotPrivileged(t *testing.T) {
	t.Parallel()
	pod := launcherOrFatal(t)
	if pod.Spec.HostNetwork || pod.Spec.HostPID || pod.Spec.HostIPC {
		t.Fatalf("host namespaces: network=%t pid=%t ipc=%t", pod.Spec.HostNetwork, pod.Spec.HostPID, pod.Spec.HostIPC)
	}
	for _, v := range pod.Spec.Volumes {
		if v.HostPath != nil {
			t.Fatalf("volume %q is a hostPath volume", v.Name)
		}
	}
	if len(pod.Spec.Containers) != 1 || len(pod.Spec.InitContainers) != 0 {
		t.Fatalf("containers=%d init=%d, want one container", len(pod.Spec.Containers), len(pod.Spec.InitContainers))
	}
	c := pod.Spec.Containers[0]
	sc := c.SecurityContext
	if sc == nil || sc.Privileged != nil && *sc.Privileged {
		t.Fatal("the launcher container is privileged")
	}
	if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
		t.Fatal("the launcher container allows privilege escalation")
	}
	for _, p := range c.Ports {
		if p.HostPort != 0 {
			t.Fatalf("host port %d", p.HostPort)
		}
	}
	for _, r := range []corev1.ResourceName{KVMResource, TunResource} {
		if q := c.Resources.Limits[r]; q.Value() != 1 {
			t.Fatalf("limit %s = %s, want 1", r, q.String())
		}
	}
	if pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken {
		t.Fatal("the launcher Pod mounts a ServiceAccount token")
	}
}

// TestBuildLauncher_HasExactlyOneCapability fails on a second capability.
// The one capability is a named exception of the chart (setec#181).
func TestBuildLauncher_HasExactlyOneCapability(t *testing.T) {
	t.Parallel()
	caps := launcherOrFatal(t).Spec.Containers[0].SecurityContext.Capabilities
	if caps == nil || len(caps.Drop) != 1 || caps.Drop[0] != "ALL" {
		t.Fatalf("drop = %v, want [ALL]", caps)
	}
	if len(caps.Add) != 1 || caps.Add[0] != LauncherCapability {
		t.Fatalf("add = %v, want exactly [%s]", caps.Add, LauncherCapability)
	}
}

func TestBuildLauncher_RefusesAnIncompleteSandbox(t *testing.T) {
	t.Parallel()
	if _, err := BuildLauncher(newSandbox(), LauncherOptions{}); err == nil {
		t.Fatal("BuildLauncher accepted an empty image")
	}
	if _, err := BuildLauncher(nil, LauncherOptions{Image: "x"}); err == nil {
		t.Fatal("BuildLauncher accepted a nil Sandbox")
	}
}
