// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package controller

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"slices"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
	"github.com/zeroroot-ai/setec/internal/diskbuilder"
)

// The disk signing seed of the tests: 32 bytes of 7, its public key and the
// key id that names its disks.
var (
	testDiskSeed      = seedOf(7)
	testDiskPublicKey = publicKeyOf(testDiskSeed)
	testDiskKeyID     = keyIDOf(testDiskSeed)
)

func seedOf(b byte) string {
	return base64.StdEncoding.EncodeToString(slices.Repeat([]byte{b}, ed25519.SeedSize))
}

func publicKeyOf(seed string) string {
	k, err := diskbuilder.ParsePrivateKey([]byte(seed))
	if err != nil {
		panic(err)
	}
	return diskbuilder.EncodePublicKey(k.Public().(ed25519.PublicKey))
}

func keyIDOf(seed string) string {
	k, err := diskbuilder.ParsePrivateKey([]byte(seed))
	if err != nil {
		panic(err)
	}
	return diskbuilder.KeyID(k.Public().(ed25519.PublicKey))
}

// diskSeedSecret is the disk signing Secret with the seed of the tests.
func diskSeedSecret(namespace, name string) *corev1.Secret {
	return &corev1.Secret{Name: name, Namespace: namespace, Data: map[string][]byte{"seed": []byte(testDiskSeed)}}
}

func diskReconciler(t *testing.T) *SandboxReconciler {
	t.Helper()
	s := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(s))
	utilruntime.Must(setecv1alpha1.AddToScheme(s))
	return &SandboxReconciler{
		Client: fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(&setecv1alpha1.Sandbox{}).
			WithObjects(diskSeedSecret("setec-system", "disk-seed")).Build(),
		DiskRepo: "ghcr.io/org/setec-disks",
		DiskKeys: []string{testDiskPublicKey},
		DiskBuilder: DiskBuilderConfig{
			Image: "ghcr.io/org/setec-disk-builder:v1", Namespace: "setec-system", SigningSecret: "disk-seed",
		},
	}
}

// TestEnsureLauncherDisk_OneJobPerDigest covers the disk step of a launcher
// Sandbox: the first call makes the Job, a running Job waits, a complete Job
// lets the Pod start, and a second Sandbox of the same digest shares the Job.
func TestEnsureLauncherDisk_OneJobPerDigest(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := diskReconciler(t)
	image := "ghcr.io/org/tool@sha256:" + strings.Repeat("ab", 32)
	sb := &setecv1alpha1.Sandbox{Name: "a", Namespace: "tenant"}
	sb.Spec.Image = image

	keyID, done, err := r.ensureLauncherDisk(ctx, sb)
	if err != nil || done || keyID != testDiskKeyID {
		t.Fatalf("first call = %q, %t, %v; want a new Job and a wait", keyID, done, err)
	}
	name, _ := diskJobName(image, testDiskKeyID)
	job := &batchv1.Job{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: "setec-system", Name: name}, job); err != nil {
		t.Fatalf("the Job: %v", err)
	}
	c := job.Spec.Template.Spec.Containers[0]
	if i := slices.Index(c.Args, "--key-id"); i < 0 || c.Args[i+1] != testDiskKeyID {
		t.Fatalf("the Job args %v do not pass the key id %s", c.Args, testDiskKeyID)
	}
	if c.Image != "ghcr.io/org/setec-disk-builder:v1" || c.Args[len(c.Args)-1] != image ||
		*job.Spec.Template.Spec.AutomountServiceAccountToken || !*job.Spec.Template.Spec.SecurityContext.RunAsNonRoot {
		t.Fatalf("the Job pod = %+v", job.Spec.Template.Spec)
	}

	if got := job.Spec.Template.Labels["app.kubernetes.io/component"]; got != "disk-builder" {
		t.Fatalf("the Job pod component label = %q; the e2e diagnostics select the pod by it", got)
	}

	if _, done, err := r.ensureLauncherDisk(ctx, sb); err != nil || done {
		t.Fatalf("a running Job = %t, %v; want a wait", done, err)
	}
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	if err := r.Status().Update(ctx, job); err != nil {
		t.Fatal(err)
	}
	other := sb.DeepCopy()
	other.Name = "b"
	if _, done, err := r.ensureLauncherDisk(ctx, other); err != nil || !done {
		t.Fatalf("a complete Job for a second Sandbox = %t, %v; want done", done, err)
	}
	jobs := &batchv1.JobList{}
	_ = r.List(ctx, jobs)
	if len(jobs.Items) != 1 {
		t.Fatalf("jobs = %d, want one for the digest", len(jobs.Items))
	}
}

func TestEnsureLauncherDisk_FailedJobAndBadImage(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := diskReconciler(t)
	sb := &setecv1alpha1.Sandbox{Name: "a", Namespace: "tenant"}
	sb.Spec.Image = "ghcr.io/org/tool:latest"
	if _, _, err := r.ensureLauncherDisk(ctx, sb); err == nil {
		t.Fatal("an image with no digest was accepted")
	}
	sb.Spec.Image = "ghcr.io/org/tool@sha256:" + strings.Repeat("cd", 32)
	name, _ := diskJobName(sb.Spec.Image, testDiskKeyID)
	failed := &batchv1.Job{Name: name, Namespace: "setec-system"}
	failed.Spec.Template.Spec.Containers = []corev1.Container{{Name: "x", Image: "x"}}
	failed.Spec.Template.Spec.RestartPolicy = corev1.RestartPolicyNever
	if err := r.Create(ctx, failed); err != nil {
		t.Fatal(err)
	}
	failed.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Message: "BackoffLimitExceeded"}}
	if err := r.Status().Update(ctx, failed); err != nil {
		t.Fatal(err)
	}
	if err := r.Create(ctx, sb); err != nil {
		t.Fatal(err)
	}
	_, proceed, _, err := r.waitForLauncherDisk(ctx, sb)
	if proceed || err != nil {
		t.Fatalf("waitForLauncherDisk = %t, %v", proceed, err)
	}
	got := &setecv1alpha1.Sandbox{}
	_ = r.Get(ctx, types.NamespacedName{Namespace: "tenant", Name: "a"}, got)
	if got.Status.Phase != setecv1alpha1.SandboxPhaseFailed || got.Status.Reason != ReasonDiskBuildFailed {
		t.Fatalf("status = %s/%s, want Failed/%s", got.Status.Phase, got.Status.Reason, ReasonDiskBuildFailed)
	}
}

// TestEnsureLauncherDisk_ARotatedSeedBuildsANewDisk is setec#272: after the
// seed in the signing Secret rotates, the disk step asks for the disk of the
// new key, with a new Job, although the Job of the previous key is complete.
// No restart of the operator is needed.
func TestEnsureLauncherDisk_ARotatedSeedBuildsANewDisk(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := diskReconciler(t)
	newSeed := seedOf(9)
	r.DiskKeys = []string{publicKeyOf(newSeed), testDiskPublicKey}
	sb := &setecv1alpha1.Sandbox{Name: "a", Namespace: "tenant"}
	sb.Spec.Image = "ghcr.io/org/tool@sha256:" + strings.Repeat("ef", 32)

	if _, _, err := r.ensureLauncherDisk(ctx, sb); err != nil {
		t.Fatal(err)
	}
	oldName, _ := diskJobName(sb.Spec.Image, testDiskKeyID)
	old := &batchv1.Job{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: "setec-system", Name: oldName}, old); err != nil {
		t.Fatalf("the Job of the previous key: %v", err)
	}
	old.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	if err := r.Status().Update(ctx, old); err != nil {
		t.Fatal(err)
	}

	secret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: "setec-system", Name: "disk-seed"}, secret); err != nil {
		t.Fatal(err)
	}
	secret.Data["seed"] = []byte(newSeed)
	if err := r.Update(ctx, secret); err != nil {
		t.Fatal(err)
	}

	keyID, done, err := r.ensureLauncherDisk(ctx, sb)
	if err != nil || done || keyID != keyIDOf(newSeed) {
		t.Fatalf("after the rotation = %q, %t, %v; want a new Job for key %s and a wait", keyID, done, err, keyIDOf(newSeed))
	}
	newName, _ := diskJobName(sb.Spec.Image, keyIDOf(newSeed))
	if newName == oldName {
		t.Fatal("the Job of the new key has the name of the Job of the previous key")
	}
	if err := r.Get(ctx, types.NamespacedName{Namespace: "setec-system", Name: newName}, &batchv1.Job{}); err != nil {
		t.Fatalf("no Job for the new key: %v", err)
	}
}

// TestEnsureLauncherDisk_RefusesASeedThatNoLauncherTrusts: a seed whose
// public key is not in --disk-public-key signs disks that each launcher
// refuses, so no Job starts.
func TestEnsureLauncherDisk_RefusesASeedThatNoLauncherTrusts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := diskReconciler(t)
	r.DiskKeys = []string{publicKeyOf(seedOf(9))}
	sb := &setecv1alpha1.Sandbox{Name: "a", Namespace: "tenant"}
	sb.Spec.Image = "ghcr.io/org/tool@sha256:" + strings.Repeat("ab", 32)
	if _, _, err := r.ensureLauncherDisk(ctx, sb); !errors.Is(err, errSigningKeyNotTrusted) {
		t.Fatalf("ensureLauncherDisk = %v, want errSigningKeyNotTrusted", err)
	}
	jobs := &batchv1.JobList{}
	_ = r.List(ctx, jobs)
	if len(jobs.Items) != 0 {
		t.Fatalf("jobs = %d, want none", len(jobs.Items))
	}

	r = diskReconciler(t)
	r.DiskBuilder.SigningSecret = "missing"
	if _, _, err := r.ensureLauncherDisk(ctx, sb); err == nil || !strings.Contains(err.Error(), "disk signing Secret") {
		t.Fatalf("ensureLauncherDisk with no Secret = %v", err)
	}
}
