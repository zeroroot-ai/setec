// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package controller

import (
	"bytes"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
	"github.com/zeroroot-ai/setec/internal/podspec"
)

// TestHostGuard_AdmitsTheLauncherAndRefusesAPrivilegedPod applies the host
// guard of the chart to a real API server (setec#187). The launcher Pod of
// podspec.BuildLauncher is admitted in a Sandbox namespace, and a
// privileged Pod in the same namespace is still refused.
func TestHostGuard_AdmitsTheLauncherAndRefusesAPrivilegedPod(t *testing.T) {
	g := NewWithT(t)
	ns := newNamespace(t, "hostguard")

	helm, err := exec.LookPath("helm")
	g.Expect(err).NotTo(HaveOccurred(), "this test renders the chart and needs helm on PATH")
	var out bytes.Buffer
	cmd := exec.Command(helm, "template", "setec", filepath.Join("..", "..", "charts", "setec"), //nolint:gosec // fixed arguments
		"--set", "webhook.certManager.enabled=true",
		"--set", "sandboxNamespaces={"+ns+"}",
		"--show-only", "templates/sandbox-namespace-host-guard.yaml")
	cmd.Stdout = &out
	g.Expect(cmd.Run()).To(Succeed())

	docs := 0
	for doc := range strings.SplitSeq(out.String(), "\n---") {
		if strings.TrimSpace(doc) == "" {
			continue
		}
		obj := &unstructured.Unstructured{}
		g.Expect(yaml.Unmarshal([]byte(doc), &obj.Object)).To(Succeed())
		if obj.GetKind() == "" {
			continue
		}
		obj.SetName(obj.GetName() + "-" + ns)
		if obj.GetKind() == "ValidatingAdmissionPolicyBinding" {
			g.Expect(unstructured.SetNestedField(obj.Object, obj.GetName(), "spec", "policyName")).To(Succeed())
		}
		g.Expect(testClient.Create(testCtx, obj)).To(Succeed())
		t.Cleanup(func() { _ = testClient.Delete(testCtx, obj) })
		docs++
	}
	g.Expect(docs).To(Equal(2), "the host guard renders a policy and a binding")
	// The binding names the policy by its rendered name; both got the suffix.

	sb := &setecv1alpha1.Sandbox{Name: "launcher", Namespace: ns}
	sb.Spec.Image = "registry.example/tool@sha256:" + strings.Repeat("a", 64)
	sb.Spec.Resources = setecv1alpha1.Resources{VCPU: 1, Memory: resource.MustParse("512Mi")}
	launcher, err := podspec.BuildLauncher(sb, podspec.LauncherOptions{
		Image: "launcher:test", DiskRepo: "registry.example/disks", DiskKeys: []string{"key"},
	})
	g.Expect(err).NotTo(HaveOccurred())
	launcher.OwnerReferences = nil

	privileged := &corev1.Pod{Name: "privileged", Namespace: ns}
	privileged.Spec.Containers = []corev1.Container{{
		Name: "c", Image: "busybox",
		SecurityContext: &corev1.SecurityContext{Privileged: new(true)},
	}}

	// A new policy takes a moment to become active. Retry the privileged
	// Pod until the guard refuses it, so the launcher check below runs
	// against an active guard.
	g.Eventually(func() string {
		p := privileged.DeepCopy()
		err := testClient.Create(testCtx, p)
		if err == nil {
			_ = testClient.Delete(testCtx, p)
			return "admitted"
		}
		return err.Error()
	}, 30*time.Second, 500*time.Millisecond).Should(ContainSubstring("privileged containers are not permitted"))

	g.Expect(testClient.Create(testCtx, launcher)).To(Succeed(), "the guard must admit the launcher Pod")
	_ = testClient.Delete(testCtx, launcher)
}
