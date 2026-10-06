// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package frontend

import (
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
)

// TestExecTarget pins the two exec paths: a launcher Sandbox runs the
// command in its machine through the relay of the launcher container, any
// other Sandbox in its workload container.
func TestExecTarget(t *testing.T) {
	t.Parallel()
	cmd := []string{"nmap", "-h"}
	launcher := &setecv1alpha1.Sandbox{}
	launcher.Status.Runtime = &setecv1alpha1.SandboxRuntimeStatus{Chosen: "launcher"}
	c, got := execTarget(launcher, cmd)
	want := append(append([]string{}, LauncherExecCommand...), cmd...)
	if c != "launcher" || !reflect.DeepEqual(got, want) {
		t.Fatalf("launcher: %s %v", c, got)
	}
	kata := &setecv1alpha1.Sandbox{}
	kata.Status.Runtime = &setecv1alpha1.SandboxRuntimeStatus{Chosen: "kata-fc"}
	if c, got := execTarget(kata, cmd); c != workloadContainerName || !reflect.DeepEqual(got, cmd) {
		t.Fatalf("kata-fc: %s %v", c, got)
	}
}

func TestPodContainer(t *testing.T) {
	t.Parallel()
	l := &corev1.Pod{}
	l.Spec.Containers = []corev1.Container{{Name: "launcher"}}
	w := &corev1.Pod{}
	w.Spec.Containers = []corev1.Container{{Name: "workload"}}
	if podContainer(l) != "launcher" || podContainer(w) != "workload" || podContainer(nil) != "workload" {
		t.Fatal("podContainer picks the wrong container")
	}
}
