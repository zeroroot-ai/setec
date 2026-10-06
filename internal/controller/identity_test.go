// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package controller

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
	"github.com/zeroroot-ai/setec/internal/podspec"
	"github.com/zeroroot-ai/setec/internal/sandboxid"
	"github.com/zeroroot-ai/setec/internal/tenancy"
)

// TestEnsureIdentity_EachSandboxGetsItsOwnKey pins setec#235 at the
// operator: a Sandbox gets a key in its own Secret, owned by it, and the
// public key and generation 1 in its status. A second Sandbox gets another
// key. A Secret of an earlier Sandbox of the same name is replaced.
func TestEnsureIdentity_EachSandboxGetsItsOwnKey(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(s))
	utilruntime.Must(setecv1alpha1.AddToScheme(s))
	ns := &corev1.Namespace{}
	ns.Name = "tenant"
	ns.Labels = map[string]string{tenancy.ClientLabelKey: "c1", tenancy.TenantLabelKey: "acme"}
	a := &setecv1alpha1.Sandbox{Name: "a", Namespace: "tenant", UID: "uid-a"}
	b := &setecv1alpha1.Sandbox{Name: "b", Namespace: "tenant", UID: "uid-b"}
	stale := &corev1.Secret{Data: map[string][]byte{podspec.IdentitySecretKey: []byte("old")}}
	stale.Name, stale.Namespace = podspec.IdentitySecretName("b"), "tenant"
	stale.OwnerReferences = []metav1.OwnerReference{{APIVersion: "setec.zeroroot.ai/v1alpha1", Kind: "Sandbox",
		Name: "b", UID: "uid-old", Controller: new(true)}}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(ns, a, b, stale).
		WithStatusSubresource(&setecv1alpha1.Sandbox{}).Build()
	r := &SandboxReconciler{Client: c, Scheme: s}

	idA, err := r.ensureIdentity(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	if idA.SandboxID != "tenant/a/uid-a" || idA.Client != "c1" || idA.Tenant != "acme" || idA.Generation != 1 {
		t.Fatalf("identity = %+v", idA)
	}
	sec := &corev1.Secret{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: "tenant", Name: podspec.IdentitySecretName("a")}, sec); err != nil {
		t.Fatal(err)
	}
	if !metav1.IsControlledBy(sec, a) {
		t.Fatal("the identity Secret is not owned by its Sandbox")
	}
	key, err := sandboxid.KeyFromSeed(string(sec.Data[podspec.IdentitySecretKey]))
	if err != nil {
		t.Fatal(err)
	}
	if a.Status.Identity.PublicKey != base64.StdEncoding.EncodeToString(key.Public().(ed25519.PublicKey)) {
		t.Fatal("the status does not hold the public key of the Secret")
	}

	if _, err := r.ensureIdentity(ctx, b); err != nil {
		t.Fatalf("replace the Secret of an earlier Sandbox: %v", err)
	}
	if b.Status.Identity.PublicKey == a.Status.Identity.PublicKey {
		t.Fatal("two Sandboxes share a key")
	}
	if err := c.Get(ctx, client.ObjectKey{Namespace: "tenant", Name: podspec.IdentitySecretName("b")}, sec); err != nil ||
		!metav1.IsControlledBy(sec, b) {
		t.Fatalf("the Secret of b = %v, %v", sec.OwnerReferences, err)
	}

	// A second call keeps the key and the generation.
	a.Status.Identity.Generation = 4
	before := a.Status.Identity.PublicKey
	if again, err := r.ensureIdentity(ctx, a); err != nil || again.Generation != 4 || a.Status.Identity.PublicKey != before {
		t.Fatalf("a second ensureIdentity = %+v, %v", again, err)
	}
}
