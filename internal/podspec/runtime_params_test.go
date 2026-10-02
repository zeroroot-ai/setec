// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package podspec

import (
	"strings"
	"testing"

	runtimepkg "github.com/zeroroot-ai/setec/internal/runtime"
)

// SandboxClass.spec.runtime.params was documented, translated and validated, and
// never delivered: applyRuntimeSelection called MutatePod(pod, nil) with a
// comment telling callers to invoke MutatePod themselves, and no caller did
// (#121). A class setting runtime.params.vcpus got the default vCPU count and
// nothing reported it.
//
// These assert on the pod the builder returns, which is the artifact that
// reaches the API server, rather than on MutatePod in isolation — in isolation it
// already worked, which is exactly why the gap survived.

func TestBuild_RuntimeParamsReachThePodAnnotations(t *testing.T) {
	sel := &runtimepkg.Selection{
		Backend:    runtimepkg.BackendKataQEMU,
		Dispatcher: runtimepkg.NewKataQEMUDispatcher(runtimepkg.BackendConfig{RuntimeClassName: "kata-qemu"}),
		Params:     map[string]string{"vcpus": "4", "memory": "2048"},
	}

	pod, err := BuildWithOptions(newSandbox(), "kata-qemu", BuildOptions{RuntimeSelection: sel})
	if err != nil {
		t.Fatalf("BuildWithOptions: %v", err)
	}

	want := map[string]string{
		"io.katacontainers.config.hypervisor.default_vcpus":  "4",
		"io.katacontainers.config.hypervisor.default_memory": "2048",
	}
	for k, v := range want {
		if got := pod.Annotations[k]; got != v {
			t.Errorf("pod annotation %s = %q, want %q", k, got, v)
		}
	}
}

// TestBuild_NoParamsLeavesNoHypervisorAnnotations. The other direction: a class
// with no params must not acquire an annotation, or every sandbox would carry a
// hypervisor override nobody asked for.
func TestBuild_NoParamsLeavesNoHypervisorAnnotations(t *testing.T) {
	sel := &runtimepkg.Selection{
		Backend:    runtimepkg.BackendKataQEMU,
		Dispatcher: runtimepkg.NewKataQEMUDispatcher(runtimepkg.BackendConfig{RuntimeClassName: "kata-qemu"}),
	}

	pod, err := BuildWithOptions(newSandbox(), "kata-qemu", BuildOptions{RuntimeSelection: sel})
	if err != nil {
		t.Fatalf("BuildWithOptions: %v", err)
	}
	for k := range pod.Annotations {
		if strings.HasPrefix(k, "io.katacontainers.config.hypervisor.") {
			t.Errorf("pod carries %s with no params declared", k)
		}
	}
}

// TestBuild_UnknownParamFailsTheBuild. Fail closed. The webhook refuses such a
// class at admission, so reaching here means admission was bypassed (--dry-run,
// a pre-existing object, webhooks disabled) — and a silently ignored override is
// what this whole change exists to end.
func TestBuild_UnknownParamFailsTheBuild(t *testing.T) {
	sel := &runtimepkg.Selection{
		Backend:    runtimepkg.BackendKataQEMU,
		Dispatcher: runtimepkg.NewKataQEMUDispatcher(runtimepkg.BackendConfig{RuntimeClassName: "kata-qemu"}),
		Params:     map[string]string{"hugepages": "on"},
	}

	_, err := BuildWithOptions(newSandbox(), "kata-qemu", BuildOptions{RuntimeSelection: sel})
	if err == nil {
		t.Fatal("a pod was built with a hypervisor param the backend does not know")
	}
	if !strings.Contains(err.Error(), "hugepages") {
		t.Errorf("error %q does not name the offending key", err.Error())
	}
}
