// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package podspec

import (
	"errors"
	"testing"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
)

const testKeepaliveImage = "ghcr.io/zeroroot-ai/setec-keepalive:v1.2.3"

func withNoCommand() func(*setecv1alpha1.Sandbox) {
	return func(sb *setecv1alpha1.Sandbox) { sb.Spec.Command = nil }
}

// TestBuild_SessionWithoutCommandBootsKeepalive is the fixture for
// setec#7: a session that declares no command boots the keepalive the
// operator installs, and the pieces that make that work agree with each
// other.
func TestBuild_SessionWithoutCommandBootsKeepalive(t *testing.T) {
	t.Parallel()
	sb := newSandbox(withLifecycleMode(setecv1alpha1.LifecycleModeSession), withNoCommand())
	pod, err := BuildWithOptions(sb, defaultRuntimeClass, BuildOptions{KeepaliveImage: testKeepaliveImage})
	if err != nil {
		t.Fatalf("BuildWithOptions: %v", err)
	}

	c := pod.Spec.Containers[0]
	if len(c.Command) != 1 || c.Command[0] != KeepalivePath {
		t.Fatalf("workload command = %v, want [%s]", c.Command, KeepalivePath)
	}
	var mounted bool
	for _, m := range c.VolumeMounts {
		if m.Name == keepaliveVolumeName {
			mounted = true
			if m.MountPath != keepaliveMountPath || !m.ReadOnly {
				t.Fatalf("keepalive mount = %+v, want read-only at %s", m, keepaliveMountPath)
			}
		}
	}
	if !mounted {
		t.Fatalf("workload does not mount %s: %+v", keepaliveVolumeName, c.VolumeMounts)
	}

	if len(pod.Spec.InitContainers) != 1 {
		t.Fatalf("init containers = %d, want 1", len(pod.Spec.InitContainers))
	}
	init := pod.Spec.InitContainers[0]
	if init.Name != KeepaliveInitContainerName || init.Image != testKeepaliveImage {
		t.Fatalf("init container = %s/%s, want %s/%s", init.Name, init.Image, KeepaliveInitContainerName, testKeepaliveImage)
	}
	if len(init.Args) != 2 || init.Args[0] != "--install" || init.Args[1] != keepaliveMountPath {
		t.Fatalf("init args = %v, want [--install %s]", init.Args, keepaliveMountPath)
	}
	if init.SecurityContext == nil || init.SecurityContext.Privileged == nil || *init.SecurityContext.Privileged ||
		init.SecurityContext.ReadOnlyRootFilesystem == nil || !*init.SecurityContext.ReadOnlyRootFilesystem {
		t.Fatalf("init container is not hardened: %+v", init.SecurityContext)
	}
	if init.Resources.Requests.Cpu().Cmp(*init.Resources.Limits.Cpu()) != 0 ||
		init.Resources.Requests.Memory().Cmp(*init.Resources.Limits.Memory()) != 0 {
		t.Fatalf("init container requests must equal limits to keep Guaranteed QoS: %+v", init.Resources)
	}

	var volume bool
	for _, v := range pod.Spec.Volumes {
		if v.Name == keepaliveVolumeName {
			volume = true
			if v.EmptyDir == nil || v.EmptyDir.SizeLimit == nil {
				t.Fatalf("keepalive volume must be a size-limited emptyDir: %+v", v)
			}
		}
	}
	if !volume {
		t.Fatalf("pod has no %s volume: %+v", keepaliveVolumeName, pod.Spec.Volumes)
	}
}

// TestBuild_SessionWithCommandIsUnchanged locks in that a session that
// states its own command gets exactly that command and no injection.
func TestBuild_SessionWithCommandIsUnchanged(t *testing.T) {
	t.Parallel()
	sb := newSandbox(withLifecycleMode(setecv1alpha1.LifecycleModeSession))
	pod, err := BuildWithOptions(sb, defaultRuntimeClass, BuildOptions{KeepaliveImage: testKeepaliveImage})
	if err != nil {
		t.Fatalf("BuildWithOptions: %v", err)
	}
	if len(pod.Spec.InitContainers) != 0 {
		t.Fatalf("init containers = %+v, want none", pod.Spec.InitContainers)
	}
	if got := pod.Spec.Containers[0].Command; len(got) != len(sb.Spec.Command) || got[0] != sb.Spec.Command[0] {
		t.Fatalf("command = %v, want %v", got, sb.Spec.Command)
	}
	for _, v := range pod.Spec.Volumes {
		if v.Name == keepaliveVolumeName {
			t.Fatalf("keepalive volume injected for a session with a command")
		}
	}
}

// TestBuild_SessionWithoutCommandNeedsKeepaliveImage: the operator
// refuses the Pod rather than booting a command it does not have.
func TestBuild_SessionWithoutCommandNeedsKeepaliveImage(t *testing.T) {
	t.Parallel()
	sb := newSandbox(withLifecycleMode(setecv1alpha1.LifecycleModeSession), withNoCommand())
	_, err := BuildWithOptions(sb, defaultRuntimeClass, BuildOptions{})
	if !errors.Is(err, ErrNoKeepaliveImage) {
		t.Fatalf("err = %v, want ErrNoKeepaliveImage", err)
	}
}

// TestBuild_EphemeralWithoutCommandRejected: an ephemeral Sandbox's one
// command is its whole life, so it is still required (ADR-0006).
func TestBuild_EphemeralWithoutCommandRejected(t *testing.T) {
	t.Parallel()
	for name, sb := range map[string]*setecv1alpha1.Sandbox{
		"implicit ephemeral": newSandbox(withNoCommand()),
		"explicit ephemeral": newSandbox(withLifecycleMode(setecv1alpha1.LifecycleModeEphemeral), withNoCommand()),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := BuildWithOptions(sb, defaultRuntimeClass, BuildOptions{KeepaliveImage: testKeepaliveImage})
			if !errors.Is(err, ErrMissingCommand) {
				t.Fatalf("err = %v, want ErrMissingCommand", err)
			}
		})
	}
}
