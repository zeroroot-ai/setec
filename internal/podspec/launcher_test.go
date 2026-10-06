// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package podspec

import (
	"encoding/json"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
	"github.com/zeroroot-ai/setec/internal/launcher"
)

func launcherOrFatal(t *testing.T) *corev1.Pod {
	t.Helper()
	pod, err := BuildLauncher(launcherSandbox(), launcherOpts())
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

// TestBuildLauncher_MountsTheDiskAsAnImageVolume proves that the kubelet
// pulls the disk of the image digest on the node. The Pod network policy
// then never has to allow the disk registry.
func TestBuildLauncher_MountsTheDiskAsAnImageVolume(t *testing.T) {
	t.Parallel()
	pod := launcherOrFatal(t)
	want := "ghcr.io/zeroroot-ai/setec-disks:" + strings.Repeat("7", 64)
	var found bool
	for _, v := range pod.Spec.Volumes {
		if v.Name == launcherDiskVolume {
			found = v.Image != nil && v.Image.Reference == want && v.Image.PullPolicy == corev1.PullIfNotPresent
		}
	}
	if !found {
		t.Fatalf("no image volume %q of %s in %+v", launcherDiskVolume, want, pod.Spec.Volumes)
	}
	var ro bool
	for _, m := range pod.Spec.Containers[0].VolumeMounts {
		if m.Name == launcherDiskVolume {
			ro = m.ReadOnly && m.MountPath == launcherDiskMountPath
		}
	}
	if !ro {
		t.Fatal("the disk volume is not mounted read-only at " + launcherDiskMountPath)
	}
}

func TestBuildLauncher_RefusesAnIncompleteSandbox(t *testing.T) {
	t.Parallel()
	if _, err := BuildLauncher(launcherSandbox(), LauncherOptions{DiskRepo: "r"}); err == nil {
		t.Fatal("BuildLauncher accepted an empty image")
	}
	if _, err := BuildLauncher(newSandbox(), launcherOpts()); err == nil {
		t.Fatal("BuildLauncher accepted an image with no digest")
	}
	if _, err := BuildLauncher(nil, LauncherOptions{Image: "x"}); err == nil {
		t.Fatal("BuildLauncher accepted a nil Sandbox")
	}
}

func launcherSandbox() *setecv1alpha1.Sandbox {
	return newSandbox(func(sb *setecv1alpha1.Sandbox) {
		sb.Spec.Image = "ghcr.io/zeroroot-ai/gibson-executor@sha256:" + strings.Repeat("7", 64)
		sb.Spec.Command = []string{"nmap", "-h"}
		sb.Spec.Env = []corev1.EnvVar{{Name: "MISSION", Value: "7"}}
	})
}

func launcherOpts() LauncherOptions {
	return LauncherOptions{
		Image: "ghcr.io/zeroroot-ai/setec-launcher:test", DiskRepo: "ghcr.io/zeroroot-ai/setec-disks",
		DiskKeys:    []string{"MCowBQYDK2VwAyEAexampleexampleexampleexampleexampleexa="},
		ResolverIPs: []string{"1.1.1.1"},
	}
}

// TestBuildLauncher_CarriesTheSpec pins the spec that the operator hands to
// the launcher: the Sandbox limits, the image digest, the scratch size and
// the command of the Sandbox.
func TestBuildLauncher_CarriesTheSpec(t *testing.T) {
	t.Parallel()
	pod := launcherOrFatal(t)
	c := pod.Spec.Containers[0]
	if len(c.Env) != 1 || c.Env[0].Name != LauncherSpecEnv {
		t.Fatalf("env = %v", c.Env)
	}
	var s launcherSpec
	if err := json.Unmarshal([]byte(c.Env[0].Value), &s); err != nil {
		t.Fatal(err)
	}
	if s.VCPU != 2 || s.MemoryMiB != 2048 || s.WritableBytes != 10<<30 || s.ImageDisk != "/disk/disk.sqfs" ||
		s.DiskSignature != "/disk/disk.sig.json" ||
		!strings.HasSuffix(s.ImageRef, strings.Repeat("7", 64)) || s.Workload.Argv[0] != "nmap" || s.Workload.Env[0] != "MISSION=7" {
		t.Fatalf("spec = %+v", s)
	}
	if pod.Spec.DNSPolicy != corev1.DNSNone || pod.Spec.DNSConfig.Nameservers[0] != "1.1.1.1" {
		t.Fatalf("dns = %v %v", pod.Spec.DNSPolicy, pod.Spec.DNSConfig)
	}
}

// TestBuildLauncher_SpecIsTheLauncherSpec ties the two sides together: the
// launcher reads the JSON that the operator writes, and the checks of the
// launcher accept it. The operator does not link the launcher, so this test
// is what stops the two copies from drifting.
func TestBuildLauncher_SpecIsTheLauncherSpec(t *testing.T) {
	pod := launcherOrFatal(t)
	if LauncherSpecEnv != launcher.SpecEnv {
		t.Fatalf("env names differ: %s and %s", LauncherSpecEnv, launcher.SpecEnv)
	}
	t.Setenv(launcher.SpecEnv, pod.Spec.Containers[0].Env[0].Value)
	s, err := launcher.ReadSpec("")
	if err != nil {
		t.Fatalf("the launcher refuses the spec of the operator: %v", err)
	}
	if s.VCPU != 2 || s.MemoryMiB != 2048 || s.Source.Boot == nil || s.Workload == nil || s.Workload.Argv[0] != "nmap" ||
		s.ImageRef == "" || s.DiskSignature == "" || len(s.DiskKeys) != 1 {
		t.Fatalf("the launcher read %+v", s)
	}
}
