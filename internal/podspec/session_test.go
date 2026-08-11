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

package podspec

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	corev1 "k8s.io/api/core/v1"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
)

// withLifecycleMode sets spec.lifecycle.mode on the Sandbox.
func withLifecycleMode(mode setecv1alpha1.LifecycleMode) func(*setecv1alpha1.Sandbox) {
	return func(sb *setecv1alpha1.Sandbox) {
		sb.Spec.Lifecycle = &setecv1alpha1.Lifecycle{Mode: mode}
	}
}

// TestBuild_SessionMountsWorkspacePVC asserts a session Sandbox's Pod
// mounts the deterministic workspace claim at /workspace and chowns it
// to the sandbox group via fsGroup.
func TestBuild_SessionMountsWorkspacePVC(t *testing.T) {
	t.Parallel()
	pod := buildOrFatal(t, newSandbox(withLifecycleMode(setecv1alpha1.LifecycleModeSession)), defaultRuntimeClass)

	var vol *corev1.Volume
	for i := range pod.Spec.Volumes {
		if pod.Spec.Volumes[i].Name == WorkspaceVolumeName {
			vol = &pod.Spec.Volumes[i]
		}
	}
	if vol == nil {
		t.Fatalf("session Pod has no %q volume; volumes = %+v", WorkspaceVolumeName, pod.Spec.Volumes)
	}
	if vol.PersistentVolumeClaim == nil {
		t.Fatalf("workspace volume is not PVC-backed: %+v", vol.VolumeSource)
	}
	if got, want := vol.PersistentVolumeClaim.ClaimName, WorkspacePVCName("demo"); got != want {
		t.Errorf("workspace claim = %q, want %q", got, want)
	}

	var mount *corev1.VolumeMount
	for i, m := range pod.Spec.Containers[0].VolumeMounts {
		if m.Name == WorkspaceVolumeName {
			mount = &pod.Spec.Containers[0].VolumeMounts[i]
		}
	}
	if mount == nil {
		t.Fatalf("workload container has no %q mount", WorkspaceVolumeName)
	}
	if mount.MountPath != WorkspaceMountPath {
		t.Errorf("workspace mountPath = %q, want %q", mount.MountPath, WorkspaceMountPath)
	}
	if mount.ReadOnly {
		t.Errorf("workspace mount is read-only; the session worktree must be writable")
	}

	if pod.Spec.SecurityContext.FSGroup == nil || *pod.Spec.SecurityContext.FSGroup != sandboxGID {
		t.Errorf("fsGroup = %v, want %d so the non-root workload can write /workspace",
			pod.Spec.SecurityContext.FSGroup, sandboxGID)
	}
}

// TestBuild_EphemeralHasNoWorkspace asserts that the default lifecycle
// (no lifecycle block at all) and an explicit "ephemeral" produce Pods
// with no workspace volume, no workspace mount, and no fsGroup — and
// that both spellings produce byte-for-byte identical Pods, which is
// the "ephemeral stays exactly today's behavior" contract of ADR-0006.
func TestBuild_EphemeralHasNoWorkspace(t *testing.T) {
	t.Parallel()

	implicit := buildOrFatal(t, newSandbox(), defaultRuntimeClass)
	explicit := buildOrFatal(t, newSandbox(withLifecycleMode(setecv1alpha1.LifecycleModeEphemeral)), defaultRuntimeClass)

	for name, pod := range map[string]*corev1.Pod{"implicit": implicit, "explicit": explicit} {
		for _, v := range pod.Spec.Volumes {
			if v.Name == WorkspaceVolumeName {
				t.Errorf("%s ephemeral Pod carries a workspace volume", name)
			}
		}
		for _, m := range pod.Spec.Containers[0].VolumeMounts {
			if m.Name == WorkspaceVolumeName {
				t.Errorf("%s ephemeral Pod carries a workspace mount", name)
			}
		}
		if pod.Spec.SecurityContext.FSGroup != nil {
			t.Errorf("%s ephemeral Pod sets fsGroup = %d; must stay unset", name, *pod.Spec.SecurityContext.FSGroup)
		}
	}

	if diff := cmp.Diff(implicit, explicit); diff != "" {
		t.Errorf("implicit vs explicit ephemeral Pod differ (-implicit +explicit):\n%s", diff)
	}
}
