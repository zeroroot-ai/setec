// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package controller

import (
	"context"
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
)

func diskReconciler(t *testing.T) *SandboxReconciler {
	t.Helper()
	s := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(s))
	utilruntime.Must(setecv1alpha1.AddToScheme(s))
	return &SandboxReconciler{
		Client:   fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(&setecv1alpha1.Sandbox{}).Build(),
		DiskRepo: "ghcr.io/org/setec-disks",
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

	done, err := r.ensureLauncherDisk(ctx, sb)
	if err != nil || done {
		t.Fatalf("first call = %t, %v; want a new Job and a wait", done, err)
	}
	name, _ := diskJobName(image)
	job := &batchv1.Job{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: "setec-system", Name: name}, job); err != nil {
		t.Fatalf("the Job: %v", err)
	}
	c := job.Spec.Template.Spec.Containers[0]
	if c.Image != "ghcr.io/org/setec-disk-builder:v1" || c.Args[len(c.Args)-1] != image ||
		*job.Spec.Template.Spec.AutomountServiceAccountToken || !*job.Spec.Template.Spec.SecurityContext.RunAsNonRoot {
		t.Fatalf("the Job pod = %+v", job.Spec.Template.Spec)
	}

	if done, err := r.ensureLauncherDisk(ctx, sb); err != nil || done {
		t.Fatalf("a running Job = %t, %v; want a wait", done, err)
	}
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	if err := r.Status().Update(ctx, job); err != nil {
		t.Fatal(err)
	}
	other := sb.DeepCopy()
	other.Name = "b"
	if done, err := r.ensureLauncherDisk(ctx, other); err != nil || !done {
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
	if _, err := r.ensureLauncherDisk(ctx, sb); err == nil {
		t.Fatal("an image with no digest was accepted")
	}
	sb.Spec.Image = "ghcr.io/org/tool@sha256:" + strings.Repeat("cd", 32)
	name, _ := diskJobName(sb.Spec.Image)
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
	proceed, _, err := r.waitForLauncherDisk(ctx, sb)
	if proceed || err != nil {
		t.Fatalf("waitForLauncherDisk = %t, %v", proceed, err)
	}
	got := &setecv1alpha1.Sandbox{}
	_ = r.Get(ctx, types.NamespacedName{Namespace: "tenant", Name: "a"}, got)
	if got.Status.Phase != setecv1alpha1.SandboxPhaseFailed || got.Status.Reason != ReasonDiskBuildFailed {
		t.Fatalf("status = %s/%s, want Failed/%s", got.Status.Phase, got.Status.Reason, ReasonDiskBuildFailed)
	}
}
