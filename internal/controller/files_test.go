// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package controller

import (
	"bytes"
	"context"
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
	"github.com/zeroroot-ai/setec/internal/sandboxfiles"
)

// TestEnsureFiles_KeepsTheContentInASecretTheSandboxOwns proves that the
// content of spec.files goes into the Secret <name>-files, owned by the
// Sandbox, one key for each file, that a second call changes nothing, that a
// Sandbox with no files gets no Secret, and that a Secret of an earlier
// Sandbox of the same name is replaced.
func TestEnsureFiles_KeepsTheContentInASecretTheSandboxOwns(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(s))
	utilruntime.Must(setecv1alpha1.AddToScheme(s))
	a := &setecv1alpha1.Sandbox{Name: "a", Namespace: "tenant", UID: "uid-a", Spec: setecv1alpha1.SandboxSpec{
		Files: []setecv1alpha1.SandboxFile{
			{Path: "s/run.sh", Content: []byte("#!/bin/sh\n")}, {Path: "s/data", Content: []byte("payload")},
		},
	}}
	none := &setecv1alpha1.Sandbox{Name: "none", Namespace: "tenant", UID: "uid-none"}
	b := &setecv1alpha1.Sandbox{Name: "b", Namespace: "tenant", UID: "uid-b", Spec: setecv1alpha1.SandboxSpec{
		Files: []setecv1alpha1.SandboxFile{{Path: "x", Content: []byte("new")}},
	}}
	stale := &corev1.Secret{Data: map[string][]byte{"f0": []byte("old"), "f9": []byte("old")}}
	stale.Name, stale.Namespace = podspec.FilesSecretName("b"), "tenant"
	stale.OwnerReferences = []metav1.OwnerReference{{APIVersion: "setec.zeroroot.ai/v1alpha1", Kind: "Sandbox",
		Name: "b", UID: "uid-old", Controller: new(true)}}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(a, none, b, stale).Build()
	r := &SandboxReconciler{Client: c, Scheme: s}

	for range 2 {
		if err := r.ensureFiles(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	sec := &corev1.Secret{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: "tenant", Name: podspec.FilesSecretName("a")}, sec); err != nil {
		t.Fatal(err)
	}
	if !metav1.IsControlledBy(sec, a) {
		t.Fatalf("the files Secret is not owned by the Sandbox: %+v", sec.OwnerReferences)
	}
	if len(sec.Data) != 3 || !bytes.Contains(sec.Data[sandboxfiles.ManifestKey], []byte("/setec/files/s/run.sh")) || !bytes.Equal(sec.Data[sandboxfiles.SecretKey(0)], []byte("#!/bin/sh\n")) ||
		!bytes.Equal(sec.Data[sandboxfiles.SecretKey(1)], []byte("payload")) {
		t.Fatalf("secret data = %v", sec.Data)
	}

	if err := r.ensureFiles(ctx, none); err != nil {
		t.Fatal(err)
	}
	err := c.Get(ctx, client.ObjectKey{Namespace: "tenant", Name: podspec.FilesSecretName("none")}, &corev1.Secret{})
	if client.IgnoreNotFound(err) != nil || err == nil {
		t.Fatalf("a Sandbox with no files got a Secret, err = %v", err)
	}

	if err := r.ensureFiles(ctx, b); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, client.ObjectKey{Namespace: "tenant", Name: podspec.FilesSecretName("b")}, sec); err != nil {
		t.Fatal(err)
	}
	if !metav1.IsControlledBy(sec, b) || len(sec.Data) != 2 || string(sec.Data["f0"]) != "new" {
		t.Fatalf("the stale Secret was not replaced: %+v", sec)
	}
}
