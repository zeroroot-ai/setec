// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package podspec

import (
	"errors"
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
// VolumeDevices rather than VolumeMounts, a workspace-format init
// container formats and mounts it onto a Bidirectional emptyDir, and
// the workload container mounts that same emptyDir at /workspace with
// HostToContainer propagation.
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

	// The container must NOT mount the workspace PVC directly.
	for _, m := range c.VolumeMounts {
		if m.Name == WorkspaceVolumeName {
			t.Fatalf("kata-fc workload container mounts %q directly via VolumeMounts; "+
				"a Block-mode PVC must be consumed via VolumeDevices: %+v", WorkspaceVolumeName, m)
		}
	}
	// It must instead mount the shared emptyDir at /workspace with
	// HostToContainer propagation, to see the init container's mount.
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
	if mount.MountPropagation == nil || *mount.MountPropagation != corev1.MountPropagationHostToContainer {
		t.Errorf("workspace mount propagation = %v, want HostToContainer", mount.MountPropagation)
	}

	// The pod-level workspace Volume is still the PVC (ADR-0007 is
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

	// The shared emptyDir volume must exist.
	var found bool
	for _, v := range pod.Spec.Volumes {
		if v.Name == workspaceMountVolumeName {
			found = true
			if v.EmptyDir == nil {
				t.Errorf("%q volume is not an emptyDir: %+v", workspaceMountVolumeName, v)
			}
		}
	}
	if !found {
		t.Fatalf("pod has no %q volume: %+v", workspaceMountVolumeName, pod.Spec.Volumes)
	}

	// The workspace-format init container must exist, consume the PVC
	// via VolumeDevices (never VolumeMounts — that would be a k8s
	// validation error against a Block-mode PVC), and mount the
	// emptyDir with Bidirectional propagation so its mount is visible
	// to the workload container started after it.
	var initC *corev1.Container
	for i := range pod.Spec.InitContainers {
		if pod.Spec.InitContainers[i].Name == KataFCWorkspaceFormatInitContainerName {
			initC = &pod.Spec.InitContainers[i]
		}
	}
	if initC == nil {
		t.Fatalf("pod has no %q init container: %+v", KataFCWorkspaceFormatInitContainerName, pod.Spec.InitContainers)
	}
	if initC.Image != testKeepaliveImage {
		t.Errorf("workspace-format init container image = %q, want %q (reuses the keepalive image)",
			initC.Image, testKeepaliveImage)
	}
	if len(initC.VolumeDevices) != 1 || initC.VolumeDevices[0].Name != WorkspaceVolumeName {
		t.Fatalf("workspace-format init container VolumeDevices = %+v, want one entry for %q",
			initC.VolumeDevices, WorkspaceVolumeName)
	}
	for _, m := range initC.VolumeMounts {
		if m.Name == WorkspaceVolumeName {
			t.Fatalf("workspace-format init container mounts the PVC via VolumeMounts, not VolumeDevices: %+v", m)
		}
	}
	var initMount *corev1.VolumeMount
	for i := range initC.VolumeMounts {
		if initC.VolumeMounts[i].Name == workspaceMountVolumeName {
			initMount = &initC.VolumeMounts[i]
		}
	}
	if initMount == nil {
		t.Fatalf("workspace-format init container has no %q mount: %+v", workspaceMountVolumeName, initC.VolumeMounts)
	}
	if initMount.MountPropagation == nil || *initMount.MountPropagation != corev1.MountPropagationBidirectional {
		t.Errorf("workspace-format init container mount propagation = %v, want Bidirectional", initMount.MountPropagation)
	}
	if initC.SecurityContext == nil || initC.SecurityContext.Privileged == nil || !*initC.SecurityContext.Privileged {
		t.Fatalf("workspace-format init container is not privileged: %+v; "+
			"the Kubernetes API refuses Bidirectional mount propagation on anything less",
			initC.SecurityContext)
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
// workspace is still a plain PVC mount, with no workspace-format init
// container and no extra emptyDir, matching the Pod every backend built
// before kata-fc grew the Block-mode path.
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

	for _, v := range pod.Spec.Volumes {
		if v.Name == workspaceMountVolumeName {
			t.Errorf("gvisor session pod carries the kata-fc-only %q volume: %+v", workspaceMountVolumeName, v)
		}
	}
	for _, ic := range pod.Spec.InitContainers {
		if ic.Name == KataFCWorkspaceFormatInitContainerName {
			t.Errorf("gvisor session pod carries the kata-fc-only workspace-format init container: %+v", ic)
		}
	}
}

// TestBuild_SessionWithNoRuntimeSelectionKeepsFilesystemWorkspace
// asserts that Build() (no RuntimeSelection at all — e.g. a caller that
// has not resolved a backend yet) preserves the pre-setec#91 Pod shape,
// exactly like TestBuild_SessionMountsWorkspacePVC.
func TestBuild_SessionWithNoRuntimeSelectionKeepsFilesystemWorkspace(t *testing.T) {
	t.Parallel()
	sb := newSandbox(withLifecycleMode(setecv1alpha1.LifecycleModeSession))
	pod, err := Build(sb, defaultRuntimeClass)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	for _, ic := range pod.Spec.InitContainers {
		if ic.Name == KataFCWorkspaceFormatInitContainerName {
			t.Errorf("pod with no RuntimeSelection carries a workspace-format init container: %+v", ic)
		}
	}
}
