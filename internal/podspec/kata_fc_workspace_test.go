// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package podspec

import (
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
	runtimepkg "github.com/zeroroot-ai/setec/internal/runtime"
)

// kataFCSelection returns a runtime Selection for the kata-fc backend,
// as the controller would after Registry.Select chooses it.
func kataFCSelection() *runtimepkg.Selection {
	return &runtimepkg.Selection{
		Backend:    runtimepkg.BackendKataFC,
		Dispatcher: runtimepkg.NewKataFCDispatcher(runtimepkg.BackendConfig{RuntimeClassName: "kata-fc"}),
	}
}

// gvisorSelection returns a runtime Selection for the gVisor backend.
func gvisorSelection() *runtimepkg.Selection {
	return &runtimepkg.Selection{
		Backend:    runtimepkg.BackendGVisor,
		Dispatcher: runtimepkg.NewGVisorDispatcher(runtimepkg.BackendConfig{RuntimeClassName: "gvisor"}),
	}
}

// TestBuild_KataFCSessionUsesBlockWorkspace asserts that a session
// Sandbox scheduled on kata-fc gets the block-device workspace shape
// (setec#91): the container consumes the workspace PVC via
// VolumeDevices rather than VolumeMounts, gets an ordinary emptyDir at
// /workspace, and its command is the keepalive binary wrapping the
// Sandbox's own command with the format/mount flags.
//
// No init container does the formatting: the operator's own
// ValidatingAdmissionPolicy (sandbox-namespace-host-guard, setec#159)
// refuses ANY privileged container in a Sandbox namespace, and
// Kubernetes itself refuses Bidirectional mount propagation — the
// mechanism an init-container design would need to share its mount
// with the workload container — on anything less than privileged. So
// the same container that will run the workload also does the format
// and mount, in its own mount namespace, before exec-ing into it.
func TestBuild_KataFCSessionUsesBlockWorkspace(t *testing.T) {
	t.Parallel()
	sb := newSandbox(withLifecycleMode(setecv1alpha1.LifecycleModeSession))
	pod, err := BuildWithOptions(sb, "kata-fc", BuildOptions{
		RuntimeSelection: kataFCSelection(),
		KeepaliveImage:   testKeepaliveImage,
	})
	if err != nil {
		t.Fatalf("BuildWithOptions: %v", err)
	}

	c := pod.Spec.Containers[0]

	// No container in the pod may be privileged (setec#159): the
	// operator's own admission policy rejects the Pod outright.
	for _, cc := range append(append([]corev1.Container{}, pod.Spec.InitContainers...), pod.Spec.Containers...) {
		if cc.SecurityContext != nil && cc.SecurityContext.Privileged != nil && *cc.SecurityContext.Privileged {
			t.Fatalf("container %q is privileged; the Sandbox namespace admission policy forbids this", cc.Name)
		}
	}

	// The workload container consumes the PVC via VolumeDevices.
	if len(c.VolumeDevices) != 1 || c.VolumeDevices[0].Name != WorkspaceVolumeName {
		t.Fatalf("workload container VolumeDevices = %+v, want one entry for %q", c.VolumeDevices, WorkspaceVolumeName)
	}
	for _, m := range c.VolumeMounts {
		if m.Name == WorkspaceVolumeName {
			t.Fatalf("kata-fc workload container mounts %q directly via VolumeMounts; "+
				"a Block-mode PVC must be consumed via VolumeDevices: %+v", WorkspaceVolumeName, m)
		}
	}

	// It mounts a plain emptyDir at /workspace — the keepalive wrapper
	// mounts the formatted device onto it from inside this same
	// container, so no MountPropagation is needed at all.
	var mount *corev1.VolumeMount
	for i, m := range c.VolumeMounts {
		if m.Name == workspaceMountVolumeName {
			mount = &c.VolumeMounts[i]
		}
	}
	if mount == nil {
		t.Fatalf("workload container has no %q mount: %+v", workspaceMountVolumeName, c.VolumeMounts)
	}
	if mount.MountPath != WorkspaceMountPath {
		t.Errorf("workspace mountPath = %q, want %q", mount.MountPath, WorkspaceMountPath)
	}
	if mount.MountPropagation != nil {
		t.Errorf("workspace mount propagation = %v, want none (same-container mount needs no propagation)",
			*mount.MountPropagation)
	}

	// The pod-level workspace Volume is still the PVC (ADR-0147 is
	// unchanged: the durable claim is what a CSI driver reattaches).
	var pvcVol *corev1.Volume
	for i := range pod.Spec.Volumes {
		if pod.Spec.Volumes[i].Name == WorkspaceVolumeName {
			pvcVol = &pod.Spec.Volumes[i]
		}
	}
	if pvcVol == nil || pvcVol.PersistentVolumeClaim == nil {
		t.Fatalf("pod has no PVC-backed %q volume: %+v", WorkspaceVolumeName, pod.Spec.Volumes)
	}

	var foundEmptyDir bool
	for _, v := range pod.Spec.Volumes {
		if v.Name == workspaceMountVolumeName {
			foundEmptyDir = true
			if v.EmptyDir == nil {
				t.Errorf("%q volume is not an emptyDir: %+v", workspaceMountVolumeName, v)
			}
		}
	}
	if !foundEmptyDir {
		t.Fatalf("pod has no %q volume: %+v", workspaceMountVolumeName, pod.Spec.Volumes)
	}

	// The command is the keepalive binary, wrapping the Sandbox's own
	// command after the format/mount flags.
	wantPrefix := []string{
		KeepalivePath,
		"--format-workspace-device", kataFCWorkspaceDevicePath,
		"--format-workspace-target", WorkspaceMountPath,
	}
	if len(c.Command) < len(wantPrefix) {
		t.Fatalf("command = %v, too short to carry the format flags", c.Command)
	}
	for i, want := range wantPrefix {
		if c.Command[i] != want {
			t.Fatalf("command[%d] = %q, want %q (full command: %v)", i, c.Command[i], want, c.Command)
		}
	}
	// After the uid/gid flags (2 flag+value pairs, 4 more tokens) comes
	// "--" then the Sandbox's own command, unchanged.
	sep := len(wantPrefix) + 4
	if sep >= len(c.Command) || c.Command[sep] != "--" {
		t.Fatalf("command = %v, want a \"--\" separator at index %d before the Sandbox's own command", c.Command, sep)
	}
	gotCmd := c.Command[sep+1:]
	if len(gotCmd) != len(sb.Spec.Command) {
		t.Fatalf("wrapped command = %v, want %v", gotCmd, sb.Spec.Command)
	}
	for i := range gotCmd {
		if gotCmd[i] != sb.Spec.Command[i] {
			t.Fatalf("wrapped command = %v, want %v", gotCmd, sb.Spec.Command)
		}
	}

	// The wrapper formats and mounts as root, then drops to the sandbox
	// user. A non-root container never gets an added capability in its
	// effective set (setec#91), so the container itself runs as root
	// with exactly the capabilities that step needs.
	for _, want := range blockWorkspaceCapabilities {
		var has bool
		for _, cap := range c.SecurityContext.Capabilities.Add {
			if cap == want {
				has = true
			}
		}
		if !has {
			t.Errorf("workload container capabilities.add = %v, want %s", c.SecurityContext.Capabilities.Add, want)
		}
	}
	if c.SecurityContext.RunAsUser == nil || *c.SecurityContext.RunAsUser != 0 {
		t.Errorf("workload container runAsUser = %v, want 0 for the format-and-mount step", c.SecurityContext.RunAsUser)
	}
	if c.SecurityContext.RunAsNonRoot == nil || *c.SecurityContext.RunAsNonRoot {
		t.Errorf("workload container runAsNonRoot = %v, want false (the pod-level true would refuse UID 0)", c.SecurityContext.RunAsNonRoot)
	}
	// Kubernetes refuses CAP_SYS_ADMIN next to allowPrivilegeEscalation:
	// false. The wrapper sets no_new_privs itself after the drop.
	if c.SecurityContext.AllowPrivilegeEscalation == nil || !*c.SecurityContext.AllowPrivilegeEscalation {
		t.Errorf("workload container allowPrivilegeEscalation = %v, want true", c.SecurityContext.AllowPrivilegeEscalation)
	}
	if c.SecurityContext.Privileged == nil || *c.SecurityContext.Privileged {
		t.Errorf("workload container privileged = %v, want false (sandbox-namespace-host-guard refuses it)", c.SecurityContext.Privileged)
	}
	// The Sandbox reports Running only once the workspace is mounted.
	if c.ReadinessProbe == nil || c.ReadinessProbe.Exec == nil {
		t.Fatalf("workload container has no exec readiness probe: %+v", c.ReadinessProbe)
	}
	if got, want := strings.Join(c.ReadinessProbe.Exec.Command, " "), KeepalivePath+" --workspace-ready "+WorkspaceMountPath; got != want {
		t.Errorf("readiness probe = %q, want %q", got, want)
	}

	// The drop and the chown both need the sandbox identity.
	uidArg, gidArg := argAfter(c.Command, "--format-workspace-uid"), argAfter(c.Command, "--format-workspace-gid")
	if uidArg != "65532" || gidArg != "65532" {
		t.Errorf("wrapper uid/gid = %q/%q, want 65532/65532: %v", uidArg, gidArg, c.Command)
	}

	// The keepalive-install init container must exist (it carries the
	// binary the workload command above resolves to), and must not be
	// privileged either.
	var initC *corev1.Container
	for i := range pod.Spec.InitContainers {
		if pod.Spec.InitContainers[i].Name == KeepaliveInitContainerName {
			initC = &pod.Spec.InitContainers[i]
		}
	}
	if initC == nil {
		t.Fatalf("pod has no %q init container: %+v", KeepaliveInitContainerName, pod.Spec.InitContainers)
	}
	if initC.Image != testKeepaliveImage {
		t.Errorf("keepalive init container image = %q, want %q", initC.Image, testKeepaliveImage)
	}
}

// TestBuild_KataFCSessionNoCommandFallsIntoKeepalive asserts that a
// kata-fc session with no spec.command still boots the plain keepalive
// (setec#7) after formatting its workspace — no "--" separator, no
// trailing command, because there is nothing to hand off to.
func TestBuild_KataFCSessionNoCommandFallsIntoKeepalive(t *testing.T) {
	t.Parallel()
	sb := newSandbox(withLifecycleMode(setecv1alpha1.LifecycleModeSession), withNoCommand())
	pod, err := BuildWithOptions(sb, "kata-fc", BuildOptions{
		RuntimeSelection: kataFCSelection(),
		KeepaliveImage:   testKeepaliveImage,
	})
	if err != nil {
		t.Fatalf("BuildWithOptions: %v", err)
	}
	c := pod.Spec.Containers[0]
	for _, tok := range c.Command {
		if tok == "--" {
			t.Fatalf("command = %v carries a \"--\" separator with no Sandbox command to run after it", c.Command)
		}
	}
	if c.Command[len(c.Command)-1] == "--" {
		t.Fatalf("command = %v ends with a bare separator", c.Command)
	}
}

// TestBuild_KataFCSessionNeedsWorkspaceFormatImage asserts the operator
// refuses to build a kata-fc session Pod when no keepalive/format image
// is configured, even when the Sandbox declares its own command (so
// usesKeepalive is false but usesBlockWorkspace is still true).
func TestBuild_KataFCSessionNeedsWorkspaceFormatImage(t *testing.T) {
	t.Parallel()
	sb := newSandbox(withLifecycleMode(setecv1alpha1.LifecycleModeSession))
	_, err := BuildWithOptions(sb, "kata-fc", BuildOptions{RuntimeSelection: kataFCSelection()})
	if !errors.Is(err, ErrNoWorkspaceFormatImage) {
		t.Fatalf("err = %v, want ErrNoWorkspaceFormatImage", err)
	}
}

// TestBuild_GVisorSessionKeepsFilesystemWorkspace asserts that a
// non-kata-fc backend is completely unaffected by setec#91: the session
// workspace is still a plain PVC mount via VolumeMounts, the Sandbox's
// own command runs directly (no keepalive wrapper, no extra
// capability), and no workspace-mount emptyDir exists — the Pod every
// backend built before kata-fc grew the Block-mode path.
func TestBuild_GVisorSessionKeepsFilesystemWorkspace(t *testing.T) {
	t.Parallel()
	sb := newSandbox(withLifecycleMode(setecv1alpha1.LifecycleModeSession))
	pod, err := BuildWithOptions(sb, "gvisor", BuildOptions{
		RuntimeSelection: gvisorSelection(),
		KeepaliveImage:   testKeepaliveImage,
	})
	if err != nil {
		t.Fatalf("BuildWithOptions: %v", err)
	}

	c := pod.Spec.Containers[0]
	if len(c.VolumeDevices) != 0 {
		t.Errorf("gvisor workload container has VolumeDevices: %+v; only kata-fc uses raw block devices", c.VolumeDevices)
	}
	var mount *corev1.VolumeMount
	for i := range c.VolumeMounts {
		if c.VolumeMounts[i].Name == WorkspaceVolumeName {
			mount = &c.VolumeMounts[i]
		}
	}
	if mount == nil {
		t.Fatalf("gvisor workload container has no direct %q mount: %+v", WorkspaceVolumeName, c.VolumeMounts)
	}
	if mount.MountPath != WorkspaceMountPath {
		t.Errorf("workspace mountPath = %q, want %q", mount.MountPath, WorkspaceMountPath)
	}

	if len(c.Command) != len(sb.Spec.Command) || c.Command[0] != sb.Spec.Command[0] {
		t.Errorf("command = %v, want the Sandbox's own command %v unwrapped", c.Command, sb.Spec.Command)
	}
	for _, cap := range c.SecurityContext.Capabilities.Add {
		for _, blockOnly := range blockWorkspaceCapabilities {
			if cap == blockOnly {
				t.Errorf("gvisor workload container carries %s; only kata-fc needs it", cap)
			}
		}
	}
	if c.SecurityContext.RunAsUser != nil {
		t.Errorf("gvisor workload container runAsUser = %d; only kata-fc's wrapper runs as root", *c.SecurityContext.RunAsUser)
	}
	if c.ReadinessProbe != nil {
		t.Errorf("gvisor workload container has a readiness probe %+v; only kata-fc mounts its workspace after start", c.ReadinessProbe)
	}
	if c.SecurityContext.AllowPrivilegeEscalation == nil || *c.SecurityContext.AllowPrivilegeEscalation {
		t.Errorf("gvisor workload container allowPrivilegeEscalation = %v, want false", c.SecurityContext.AllowPrivilegeEscalation)
	}

	for _, v := range pod.Spec.Volumes {
		if v.Name == workspaceMountVolumeName {
			t.Errorf("gvisor session pod carries the kata-fc-only %q volume: %+v", workspaceMountVolumeName, v)
		}
	}
	if len(pod.Spec.InitContainers) != 0 {
		t.Errorf("gvisor session pod carries init containers: %+v; it needs no keepalive/format binary", pod.Spec.InitContainers)
	}
}

// TestBuild_SessionWithNoRuntimeSelectionKeepsFilesystemWorkspace
// asserts that BuildWithOptions() with no RuntimeSelection at all (e.g. a caller that
// has not resolved a backend yet) preserves the pre-setec#91 Pod shape,
// exactly like TestBuild_SessionMountsWorkspacePVC.
func TestBuild_SessionWithNoRuntimeSelectionKeepsFilesystemWorkspace(t *testing.T) {
	t.Parallel()
	sb := newSandbox(withLifecycleMode(setecv1alpha1.LifecycleModeSession))
	pod, err := BuildWithOptions(sb, defaultRuntimeClass, BuildOptions{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(pod.Spec.InitContainers) != 0 {
		t.Errorf("pod with no RuntimeSelection carries init containers: %+v", pod.Spec.InitContainers)
	}
	if len(pod.Spec.Containers[0].VolumeDevices) != 0 {
		t.Errorf("pod with no RuntimeSelection carries VolumeDevices: %+v", pod.Spec.Containers[0].VolumeDevices)
	}
}

// argAfter returns the element after flag in args, or "".
func argAfter(args []string, flag string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag {
			return args[i+1]
		}
	}
	return ""
}
