// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package controller

import (
	"context"
	"testing"

	"github.com/go-logr/logr"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/controller-runtime/pkg/client"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
	runtimepkg "github.com/zeroroot-ai/setec/internal/runtime"
)

// TestHandleMissingPod_ClassRequestsReachPod proves the controller
// carries the SandboxClass's scheduler reservation onto the Pod it
// creates (setec#372): requests come from the class, limits from the
// Sandbox, so a long-lived member reserves less than its burst ceiling.
func TestHandleMissingPod_ClassRequestsReachPod(t *testing.T) {
	g := NewWithT(t)

	cfg := &runtimepkg.RuntimeConfig{
		Runtimes: map[string]runtimepkg.BackendConfig{
			runtimepkg.BackendRunc: emptyOverheadConfig("runc"),
		},
		Defaults: runtimepkg.DefaultsConfig{
			Runtime: runtimepkg.RuntimeDefaults{Backend: runtimepkg.BackendRunc},
		},
	}
	reg := runtimepkg.NewRegistry()
	reg.Register(runtimepkg.NewRuncDispatcher(cfg.Runtimes[runtimepkg.BackendRunc]))

	cpu := resource.MustParse("250m")
	mem := resource.MustParse("768Mi")
	cls := newSandboxClassForRS("member-class", runtimepkg.BackendRunc, nil)
	cls.Spec.Requests = &setecv1alpha1.ResourceRequests{CPU: &cpu, Memory: &mem}

	sb := newSandboxForRS(cls.Name)
	sb.Spec.Resources = setecv1alpha1.Resources{VCPU: 2, Memory: resource.MustParse("4Gi")}

	r, c := newRSReconciler(t, reg, cfg, cls, sb)

	_, err := r.handleMissingPod(context.Background(), logr.Discard(), sb, cls, "")
	g.Expect(err).ToNot(HaveOccurred())

	var pods corev1.PodList
	g.Expect(c.List(context.Background(), &pods, client.InNamespace(sb.Namespace))).To(Succeed())
	g.Expect(pods.Items).To(HaveLen(1))
	res := pods.Items[0].Spec.Containers[0].Resources

	g.Expect(res.Requests.Cpu().Cmp(cpu)).To(BeZero(), "request cpu = %s, want 250m", res.Requests.Cpu())
	g.Expect(res.Requests.Memory().Cmp(mem)).To(BeZero(), "request memory = %s, want 768Mi", res.Requests.Memory())
	g.Expect(res.Limits.Cpu().Cmp(resource.MustParse("2"))).To(BeZero(), "limit cpu = %s, want 2", res.Limits.Cpu())
	g.Expect(res.Limits.Memory().Cmp(resource.MustParse("4Gi"))).To(BeZero(), "limit memory = %s, want 4Gi", res.Limits.Memory())
}
