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
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/zeroroot-ai/setec/internal/frontend"
	"github.com/zeroroot-ai/setec/internal/tenancy"
)

// TestFrontendScope_OnlyPairNamespacesAndTheirGrants applies the
// frontend-scope policy of the chart to a real API server (setec#207) and
// acts as the frontend ServiceAccount. The NamespaceProvisioner makes a
// pair namespace and its grants; every other namespace or binding of the
// frontend is refused.
func TestFrontendScope_OnlyPairNamespacesAndTheirGrants(t *testing.T) {
	g := NewWithT(t)
	helm, err := exec.LookPath("helm")
	g.Expect(err).NotTo(HaveOccurred(), "this test renders the chart and needs helm on PATH")
	var out bytes.Buffer
	cmd := exec.Command(helm, "template", "setec", filepath.Join("..", "..", "charts", "setec"), //nolint:gosec // fixed arguments
		"--set", "webhook.certManager.enabled=true",
		"--set", "frontend.enabled=true",
		"--set", "frontend.tlsCertSecretName=x", "--set", "frontend.tlsClientCASecretName=y",
		"--set", "frontend.clients[0].name=saas",
		"--set", "frontend.clients[0].spiffeID=spiffe://example.org/ns/gibson/sa/gibson-daemon",
		"--show-only", "templates/frontend-scope-policy.yaml")
	cmd.Stdout = &out
	g.Expect(cmd.Run()).To(Succeed())
	for doc := range strings.SplitSeq(out.String(), "\n---") {
		obj := &unstructured.Unstructured{}
		g.Expect(yaml.Unmarshal([]byte(doc), &obj.Object)).To(Succeed())
		if obj.GetKind() == "" {
			continue
		}
		g.Expect(testClient.Create(testCtx, obj)).To(Succeed())
		t.Cleanup(func() { _ = testClient.Delete(testCtx, obj) })
	}

	// The frontend ServiceAccount, with every verb, so only the policy refuses.
	const feUser = "system:serviceaccount:setec-system:setec-frontend"
	admin := &rbacv1.ClusterRoleBinding{
		Name:     "scope-test-admin",
		RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "cluster-admin"},
		Subjects: []rbacv1.Subject{{Kind: rbacv1.UserKind, Name: feUser, APIGroup: rbacv1.GroupName}},
	}
	g.Expect(testClient.Create(testCtx, admin)).To(Succeed())
	t.Cleanup(func() { _ = testClient.Delete(testCtx, admin) })
	cfg := rest.CopyConfig(testEnv.Config)
	cfg.Impersonate = rest.ImpersonationConfig{UserName: feUser}
	fe, err := client.New(cfg, client.Options{Scheme: testClient.Scheme()})
	g.Expect(err).NotTo(HaveOccurred())

	// A namespace with no sandbox labels is refused. Retry until the new
	// policy is active.
	g.Eventually(func() string {
		ns := &corev1.Namespace{Name: "not-a-pair-" + strings.ToLower(time.Now().Format("150405.000"))}
		ns.Name = strings.ReplaceAll(ns.Name, ".", "")
		if err := fe.Create(testCtx, ns); err != nil {
			return err.Error()
		}
		_ = testClient.Delete(testCtx, ns)
		return "admitted"
	}, 30*time.Second, 500*time.Millisecond).Should(ContainSubstring("may create only a pair namespace"))

	p, err := tenancy.NewPair("saas", "acme"+strings.ReplaceAll(time.Now().Format("150405.000"), ".", ""))
	g.Expect(err).NotTo(HaveOccurred())
	prov := &frontend.NamespaceProvisioner{Client: fe, Grants: []frontend.RoleGrant{
		{ClusterRole: "setec-sandbox-namespace", ServiceAccount: types.NamespacedName{Namespace: "setec-system", Name: "setec"}},
		{ClusterRole: "setec-frontend-exec", ServiceAccount: types.NamespacedName{Namespace: "setec-system", Name: "setec-frontend"}},
	}}
	name, err := prov.NamespaceFor(testCtx, p)
	g.Expect(err).NotTo(HaveOccurred(), "the frontend must be able to make a pair namespace and its two grants")
	t.Cleanup(func() { _ = testClient.Delete(testCtx, &corev1.Namespace{Name: name}) })

	// A binding of a different role in the pair namespace is refused.
	other := &rbacv1.RoleBinding{
		Name: "x", Namespace: name,
		RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "cluster-admin"},
		Subjects: []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: "setec", Namespace: "setec-system"}},
	}
	g.Expect(fe.Create(testCtx, other)).To(MatchError(ContainSubstring("may bind only the two roles")))

	// The right role in a namespace that is not a sandbox namespace is refused.
	plain := newNamespace(t, "plain")
	wrongNS := &rbacv1.RoleBinding{
		Name: "y", Namespace: plain,
		RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "setec-sandbox-namespace"},
		Subjects: []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: "setec", Namespace: "setec-system"}},
	}
	g.Expect(fe.Create(testCtx, wrongNS)).To(MatchError(ContainSubstring("may bind only the two roles")))
}
