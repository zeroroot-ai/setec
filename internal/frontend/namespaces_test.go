// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package frontend

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/zeroroot-ai/setec/internal/tenancy"
)

func provisioner(t *testing.T, objs ...client.Object) *NamespaceProvisioner {
	t.Helper()
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	return &NamespaceProvisioner{
		Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build(),
		Grants: []RoleGrant{
			{ClusterRole: "setec-sandbox-namespace", ServiceAccount: types.NamespacedName{Namespace: "setec-system", Name: "setec"}},
			{ClusterRole: "setec-frontend-exec", ServiceAccount: types.NamespacedName{Namespace: "setec-system", Name: "setec-frontend"}},
		},
	}
}

func mustPair(t *testing.T, clientName, tenant string) tenancy.Pair {
	t.Helper()
	p, err := tenancy.NewPair(clientName, tenant)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// TestNamespaceProvisioner_MakesOneNamespacePerPair is the done-when test of
// setec#207: the first call of a pair makes its namespace and grants, a
// second call reuses them, and two pairs never share a namespace.
func TestNamespaceProvisioner_MakesOneNamespacePerPair(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	n := provisioner(t)
	a := mustPair(t, "cluster-a", "acme")
	b := mustPair(t, "cluster-b", "acme")

	nsA, err := n.NamespaceFor(ctx, a)
	if err != nil {
		t.Fatalf("pair A: %v", err)
	}
	again, err := n.NamespaceFor(ctx, a)
	if err != nil || again != nsA {
		t.Fatalf("pair A again = %q, %v; want %q", again, err, nsA)
	}
	nsB, err := n.NamespaceFor(ctx, b)
	if err != nil || nsB == nsA {
		t.Fatalf("pair B = %q, %v; want a namespace of its own", nsB, err)
	}
	if !strings.HasPrefix(nsA, PairNamespacePrefix) || len(nsA) > 63 {
		t.Fatalf("namespace name %q", nsA)
	}

	ns := &corev1.Namespace{}
	if err := n.Client.Get(ctx, types.NamespacedName{Name: nsA}, ns); err != nil {
		t.Fatal(err)
	}
	if ns.Labels[tenancy.ClientLabelKey] != "cluster-a" || ns.Labels[tenancy.TenantLabelKey] != "acme" ||
		ns.Labels[SandboxNamespaceLabel] != "true" {
		t.Fatalf("labels = %v", ns.Labels)
	}
	rbs := &rbacv1.RoleBindingList{}
	if err := n.Client.List(ctx, rbs, client.InNamespace(nsA)); err != nil {
		t.Fatal(err)
	}
	if len(rbs.Items) != 2 {
		t.Fatalf("role bindings in %s = %d, want 2", nsA, len(rbs.Items))
	}
	nss := &corev1.NamespaceList{}
	if err := n.Client.List(ctx, nss); err != nil || len(nss.Items) != 2 {
		t.Fatalf("namespaces = %d, %v; want 2", len(nss.Items), err)
	}
}

// TestNamespaceProvisioner_RefusesAForeignNamespace proves that a namespace
// with the name of a pair and other labels is refused, never adopted.
func TestNamespaceProvisioner_RefusesAForeignNamespace(t *testing.T) {
	t.Parallel()
	p := mustPair(t, "cluster-a", "acme")
	squatter := &corev1.Namespace{Name: PairNamespaceName(p), Labels: map[string]string{tenancy.TenantLabelKey: "acme"}}
	n := provisioner(t, squatter)
	if _, err := n.NamespaceFor(context.Background(), p); err == nil {
		t.Fatal("a namespace with the wrong labels was adopted")
	}
}

// TestNamespaceProvisioner_ReusesAnExistingPairNamespace covers a second
// frontend replica that finds the namespace already made by the first.
func TestNamespaceProvisioner_ReusesAnExistingPairNamespace(t *testing.T) {
	t.Parallel()
	p := mustPair(t, "cluster-a", "acme")
	labels := p.Labels()
	labels[SandboxNamespaceLabel] = "true"
	existing := &corev1.Namespace{Name: PairNamespaceName(p), Labels: labels}
	n := provisioner(t, existing)
	got, err := n.NamespaceFor(context.Background(), p)
	if err != nil || got != existing.Name {
		t.Fatalf("NamespaceFor = %q, %v; want %q", got, err, existing.Name)
	}
}
