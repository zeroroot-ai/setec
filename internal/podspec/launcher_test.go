// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package podspec

import (
	"encoding/json"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

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
	if pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken {
		t.Fatal("the launcher Pod mounts a ServiceAccount token")
	}
	checkLauncherContainer(t, &pod.Spec.Containers[0])
}

// checkLauncherContainer checks that c has no privilege, no host port,
// one of each device, and a readiness probe on the guest agent.
func checkLauncherContainer(t *testing.T, c *corev1.Container) {
	t.Helper()
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
	if rp := c.ReadinessProbe; rp == nil || rp.Exec == nil || rp.Exec.Command[len(rp.Exec.Command)-1] != "ready" {
		t.Fatal("the launcher container has no readiness probe on the guest agent")
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

// TestBuildLauncher_MemoryHoldsTheMachineAndTheVMM pins the memory of the
// Pod above the memory of the machine. A Pod limit equal to the machine
// memory was killed by the kernel during each snapshot of a small machine.
func TestBuildLauncher_MemoryHoldsTheMachineAndTheVMM(t *testing.T) {
	t.Parallel()
	pod := launcherOrFatal(t)
	res := pod.Spec.Containers[0].Resources
	want := resource.MustParse("2Gi")
	want.Add(LauncherMemoryOverhead)
	for name, list := range map[string]corev1.ResourceList{"limit": res.Limits, "request": res.Requests} {
		got := list[corev1.ResourceMemory]
		if got.Cmp(want) != 0 {
			t.Errorf("memory %s = %s, want %s (the machine and the overhead)", name, got.String(), want.String())
		}
	}
	if LauncherMemoryOverhead.Sign() <= 0 {
		t.Fatal("the launcher Pod has no memory for the VMM")
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

// TestLauncherFileNamesMatchTheLauncher keeps the file names that the node
// agent uses equal to the names of the launcher, and the two evidence types
// equal on the wire.
func TestLauncherFileNamesMatchTheLauncher(t *testing.T) {
	t.Parallel()
	if LauncherAPISocket != launcher.APISocket {
		t.Fatalf("socket names differ: %s and %s", LauncherAPISocket, launcher.APISocket)
	}
	if LauncherStagedNoReseed != launcher.StagedNoReseed {
		t.Fatalf("staged markers differ: %s and %s", LauncherStagedNoReseed, launcher.StagedNoReseed)
	}
	pod := launcherOrFatal(t)
	var s launcherSpec
	if err := json.Unmarshal([]byte(pod.Spec.Containers[0].Env[0].Value), &s); err != nil {
		t.Fatal(err)
	}
	if s.WorkDir != LauncherVMDir {
		t.Fatalf("work dir = %s, want %s", s.WorkDir, LauncherVMDir)
	}
	in := launcher.RestoreEvidence{EntropyReseeded: true, Uniquified: true, ClockSet: true, Error: "x"}
	raw, _ := json.Marshal(in)
	var out RestoreEvidence
	if err := json.Unmarshal(raw, &out); err != nil || out != (RestoreEvidence{true, true, true, "x"}) {
		t.Fatalf("evidence on the wire: %s -> %+v, %v", raw, out, err)
	}
}

// TestBuildLauncher_RestoreLoadsTheStagedSnapshot proves that a restore Pod
// asks the launcher to wait for the files of the node agent and to load
// them, and that the launcher accepts that spec.
func TestBuildLauncher_RestoreLoadsTheStagedSnapshot(t *testing.T) {
	opts := launcherOpts()
	opts.Restore = true
	opts.NodeName = "node-a"
	pod, err := BuildLauncher(launcherSandbox(), opts)
	if err != nil {
		t.Fatal(err)
	}
	mf := pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0].MatchFields
	if len(mf) != 1 || mf[0].Key != "metadata.name" || mf[0].Values[0] != "node-a" {
		t.Fatalf("the restore Pod is not pinned to the node of the snapshot: %+v", mf)
	}
	t.Setenv(launcher.SpecEnv, pod.Spec.Containers[0].Env[0].Value)
	s, err := launcher.ReadSpec("")
	if err != nil {
		t.Fatalf("the launcher refuses the restore spec: %v", err)
	}
	snap := s.Source.Snapshot
	if s.Source.Boot != nil || snap == nil || s.Workload != nil {
		t.Fatalf("source = %+v, workload = %+v; want a snapshot and no workload", s.Source, s.Workload)
	}
	if snap.State != "/work/vm/restore/state.bin" || snap.Memory != "/work/vm/restore/memory.bin" ||
		snap.Staged != "/work/vm/restore/staged" || snap.Evidence != "/work/vm/restore/evidence.json" ||
		snap.TakenAt != "/work/vm/restore/taken-at" {
		t.Fatalf("snapshot = %+v", snap)
	}
}

// TestBuildLauncher_CPUTemplateAndInstanceType pins the two ways a restore
// gets a CPU that its snapshot can run on: the template of the class, or a
// node of the instance type of the source.
func TestBuildLauncher_CPUTemplateAndInstanceType(t *testing.T) {
	opts := launcherOpts()
	opts.CPUTemplate = "fleet-v1"
	opts.InstanceType = "m8i.2xlarge"
	pod, err := BuildLauncher(launcherSandbox(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(launcher.SpecEnv, pod.Spec.Containers[0].Env[0].Value)
	s, err := launcher.ReadSpec("")
	if err != nil || s.CPUTemplate != "/opt/setec/cpu-templates/fleet-v1.json" {
		t.Fatalf("cpu template = %q, %v", s.CPUTemplate, err)
	}
	found := false
	for _, e := range pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0].MatchExpressions {
		if e.Key == corev1.LabelInstanceTypeStable && e.Values[0] == "m8i.2xlarge" {
			found = true
		}
	}
	if !found {
		t.Fatal("the Pod is not kept on the instance type of the source")
	}
}

// TestBuildLauncherBase_BootsWithNoWorkloadAndNoOwner pins the base Pod of
// the warm pool: a boot, no workload, no owner Sandbox, the base label.
func TestBuildLauncherBase_BootsWithNoWorkloadAndNoOwner(t *testing.T) {
	sb := launcherSandbox()
	pod, err := BuildLauncherBase("base-tools-0", "setec-system", sb.Spec.Image, sb.Spec.Resources, launcherOpts())
	if err != nil {
		t.Fatal(err)
	}
	if pod.Name != "base-tools-0" || len(pod.OwnerReferences) != 0 || pod.Labels[BaseLabel] != "true" {
		t.Fatalf("base Pod = %s %v %v", pod.Name, pod.OwnerReferences, pod.Labels)
	}
	t.Setenv(launcher.SpecEnv, pod.Spec.Containers[0].Env[0].Value)
	s, err := launcher.ReadSpec("")
	if err != nil || !s.Base || s.Workload != nil || s.Source.Boot == nil {
		t.Fatalf("base spec = %+v, %v", s, err)
	}
}

// TestBuildLauncher_FromBaseLoadsTheBaseAndStartsTheWorkload pins the Pod
// of a warm start: the staged snapshot source and the Sandbox workload.
func TestBuildLauncher_FromBaseLoadsTheBaseAndStartsTheWorkload(t *testing.T) {
	opts := launcherOpts()
	opts.FromBase = true
	pod, err := BuildLauncher(launcherSandbox(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(launcher.SpecEnv, pod.Spec.Containers[0].Env[0].Value)
	s, err := launcher.ReadSpec("")
	if err != nil || s.Source.Snapshot == nil || s.Workload == nil || s.Workload.Argv[0] != "nmap" || s.Base {
		t.Fatalf("warm start spec = %+v, %v", s, err)
	}
}

// TestBuildLauncher_SessionGetsItsWorkspaceDevice pins the workspace of a
// launcher session: its PVC as a raw block device that the machine mounts.
func TestBuildLauncher_SessionGetsItsWorkspaceDevice(t *testing.T) {
	sb := launcherSandbox()
	sb.Spec.Lifecycle = &setecv1alpha1.Lifecycle{Mode: setecv1alpha1.LifecycleModeSession}
	if !sb.Spec.IsSession() {
		t.Skip("the fixture is not a session")
	}
	pod, err := BuildLauncher(sb, launcherOpts())
	if err != nil {
		t.Fatal(err)
	}
	devs := pod.Spec.Containers[0].VolumeDevices
	if len(devs) != 1 || devs[0].DevicePath != LauncherWorkspaceDevice {
		t.Fatalf("volume devices = %+v", devs)
	}
	found := false
	for _, v := range pod.Spec.Volumes {
		if v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == WorkspacePVCName(sb.Name) {
			found = true
		}
	}
	if !found {
		t.Fatal("no workspace PVC volume")
	}
	t.Setenv(launcher.SpecEnv, pod.Spec.Containers[0].Env[0].Value)
	s, err := launcher.ReadSpec("")
	if err != nil || s.WorkspaceDevice != LauncherWorkspaceDevice {
		t.Fatalf("spec workspace = %q, %v", s.WorkspaceDevice, err)
	}
}

// TestBuildLauncher_IdentityKeyStaysWithTheLauncher pins setec#235 in the
// Pod: the identity Secret of the Sandbox is mounted read-only into the
// launcher container, the launcher spec names it, and a warm pool base,
// which belongs to no Sandbox, gets no identity.
func TestBuildLauncher_IdentityKeyStaysWithTheLauncher(t *testing.T) {
	opts := launcherOpts()
	opts.Identity = &LauncherIdentity{SandboxID: "ns/sb/uid", Tenant: "acme", Generation: 3}
	sb := launcherSandbox()
	pod, err := BuildLauncher(sb, opts)
	if err != nil {
		t.Fatal(err)
	}
	var secretVol string
	for _, v := range pod.Spec.Volumes {
		if v.Secret != nil {
			secretVol = v.Secret.SecretName
			if v.Secret.DefaultMode == nil || *v.Secret.DefaultMode != 0o400 {
				t.Fatalf("the identity Secret mode = %v", v.Secret.DefaultMode)
			}
		}
	}
	if secretVol != IdentitySecretName(sb.Name) {
		t.Fatalf("the identity Secret volume = %q", secretVol)
	}
	mounted := false
	for _, m := range pod.Spec.Containers[0].VolumeMounts {
		if m.Name == launcherIdentityVolume {
			mounted = m.ReadOnly && m.MountPath == launcherIdentityMountPath
		}
	}
	if !mounted {
		t.Fatal("the identity Secret is not mounted read-only in the launcher container")
	}
	t.Setenv(launcher.SpecEnv, pod.Spec.Containers[0].Env[0].Value)
	s, err := launcher.ReadSpec("")
	if err != nil {
		t.Fatalf("the launcher refuses the spec: %v", err)
	}
	if s.Identity == nil || s.Identity.SandboxID != "ns/sb/uid" || s.Identity.Generation != 3 ||
		s.Identity.KeyFile != LauncherIdentityKey || s.Identity.GenerationFile != LauncherIdentityGeneration {
		t.Fatalf("the launcher identity = %+v", s.Identity)
	}

	base, err := BuildLauncherBase("base-1", "pool", sb.Spec.Image, sb.Spec.Resources, opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range base.Spec.Volumes {
		if v.Secret != nil {
			t.Fatal("a base mounts an identity Secret")
		}
	}
}

// TestBuildLauncher_ClassRequestsLowerTheReservation pins the scheduler
// reservation of a class on the launcher Pod: a request below its limit
// replaces the request, a request above its limit keeps the limit, and the
// device resources keep a request equal to the limit.
func TestBuildLauncher_ClassRequestsLowerTheReservation(t *testing.T) {
	t.Parallel()
	cpu := resource.MustParse("250m")
	tooMuch := resource.MustParse("1Ti")
	opts := launcherOpts()
	opts.Requests = &setecv1alpha1.ResourceRequests{CPU: &cpu, Memory: &tooMuch}
	pod, err := BuildLauncher(launcherSandbox(), opts)
	if err != nil {
		t.Fatalf("BuildLauncher: %v", err)
	}
	res := pod.Spec.Containers[0].Resources
	if got := res.Requests[corev1.ResourceCPU]; got.Cmp(cpu) != 0 {
		t.Fatalf("cpu request = %s, want %s", got.String(), cpu.String())
	}
	if got, limit := res.Requests[corev1.ResourceMemory], res.Limits[corev1.ResourceMemory]; got.Cmp(limit) != 0 {
		t.Fatalf("memory request = %s, want the limit %s", got.String(), limit.String())
	}
	for _, r := range []corev1.ResourceName{KVMResource, TunResource} {
		if got, limit := res.Requests[r], res.Limits[r]; got.Cmp(limit) != 0 {
			t.Fatalf("%s request = %s, want the limit %s", r, got.String(), limit.String())
		}
	}

	// No class reservation: requests equal limits, except the ephemeral
	// storage, which requests the headroom only.
	plain := launcherOrFatal(t).Spec.Containers[0].Resources
	for name, limit := range plain.Limits {
		if name == corev1.ResourceEphemeralStorage {
			continue
		}
		if got := plain.Requests[name]; got.Cmp(limit) != 0 {
			t.Fatalf("%s request = %s, want the limit %s", name, got.String(), limit.String())
		}
	}
}

// TestBuildLauncher_EphemeralStorageLimit pins the ephemeral-storage limit
// of the launcher Pod (setec#172): the work volume, the scratch size plus
// 2Gi, plus the headroom for logs. The request is the headroom.
func TestBuildLauncher_EphemeralStorageLimit(t *testing.T) {
	t.Parallel()
	opts := launcherOpts()
	opts.Scratch = resource.MustParse("4Gi")
	pod, err := BuildLauncher(launcherSandbox(), opts)
	if err != nil {
		t.Fatalf("BuildLauncher: %v", err)
	}
	res := pod.Spec.Containers[0].Resources
	if got, want := res.Limits[corev1.ResourceEphemeralStorage], resource.MustParse("7Gi"); got.Cmp(want) != 0 {
		t.Fatalf("ephemeral-storage limit = %s, want %s", got.String(), want.String())
	}
	if got, want := res.Requests[corev1.ResourceEphemeralStorage], resource.MustParse("1Gi"); got.Cmp(want) != 0 {
		t.Fatalf("ephemeral-storage request = %s, want %s", got.String(), want.String())
	}
}
