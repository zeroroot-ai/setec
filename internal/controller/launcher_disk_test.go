// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package controller

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
			RegistrySecret: "disk-push", ImageRegistries: []string{"ghcr.io"},
		},
	}
}

// completeJob marks the Job name as complete.
func completeJob(t *testing.T, r *SandboxReconciler, name string) {
	t.Helper()
	job := &batchv1.Job{}
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: "setec-system", Name: name}, job); err != nil {
		t.Fatalf("the Job %s: %v", name, err)
	}
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	if err := r.Status().Update(context.Background(), job); err != nil {
		t.Fatal(err)
	}
}

// buildPod is the succeeded Pod of the build Job name, with message as its
// termination message.
func buildPod(t *testing.T, r *SandboxReconciler, name, message string) {
	t.Helper()
	pod := &corev1.Pod{Name: name + "-x", Namespace: "setec-system",
		Labels: map[string]string{batchv1.JobNameLabel: name}}
	pod.Spec.Containers = []corev1.Container{{Name: builderContainer, Image: "x"}}
	if err := r.Create(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	pod.Status.Phase = corev1.PodSucceeded
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: builderContainer,
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Message: message}}}}
	if err := r.Status().Update(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
}

// testBuildResult is a build report of a disk with the SHA-256 of 32 bytes
// of 0xd.
func testBuildResult() diskbuilder.BuildResult {
	return diskbuilder.BuildResult{
		Layer: diskbuilder.LayerRef{
			Digest: "sha256:" + strings.Repeat("a", 64), DiffID: "sha256:" + strings.Repeat("b", 64), Size: 4096,
		},
		DiskSHA256: strings.Repeat("d", 64),
	}
}

// noSeed fails when a Job pod mounts the signing Secret, or carries a
// volume of any Secret other than the push credential.
func noSeed(t *testing.T, job *batchv1.Job, signingSecret string) {
	t.Helper()
	for _, v := range job.Spec.Template.Spec.Volumes {
		if v.Secret != nil && (v.Secret.SecretName == signingSecret || v.Secret.SecretName != "disk-push") {
			t.Fatalf("the Job %s mounts the Secret %s", job.Name, v.Secret.SecretName)
		}
		if v.Projected != nil {
			t.Fatalf("the Job %s has a projected volume", job.Name)
		}
	}
	for _, c := range job.Spec.Template.Spec.Containers {
		for _, e := range c.Env {
			if e.ValueFrom != nil {
				t.Fatalf("the Job %s reads %s from a reference", job.Name, e.Name)
			}
		}
	}
}

// TestEnsureLauncherDisk_OneJobPerDigest covers the disk step of a launcher
// Sandbox: the first call makes the build Job, a running Job waits, a
// complete Job whose report says the disk exists lets the Pod start, and a
// second Sandbox of the same digest shares the Job.
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
	if got := job.Spec.Template.Labels["app.kubernetes.io/component"]; got != componentDiskBuilder {
		t.Fatalf("the Job pod component label = %q; the chart policy and the e2e diagnostics select the pod by it", got)
	}
	noSeed(t, job, "disk-seed")

	if _, done, err := r.ensureLauncherDisk(ctx, sb); err != nil || done {
		t.Fatalf("a running Job = %t, %v; want a wait", done, err)
	}
	completeJob(t, r, name)
	buildPod(t, r, name, `{"exists":true,"layer":{"digest":"","diffID":"","size":0}}`)
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

// TestEnsureLauncherDisk_TheOperatorSignsAndThePublishJobHasNoSeed is the
// split of the disk step: the build Job reports a disk, the operator signs
// it, and the publish Job carries the signature and no seed. The signature
// verifies under the public key of the seed and names the image and the
// disk of the report.
func TestEnsureLauncherDisk_TheOperatorSignsAndThePublishJobHasNoSeed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := diskReconciler(t)
	image := "ghcr.io/org/tool@sha256:" + strings.Repeat("12", 32)
	sb := &setecv1alpha1.Sandbox{Name: "a", Namespace: "tenant"}
	sb.Spec.Image = image

	if _, _, err := r.ensureLauncherDisk(ctx, sb); err != nil {
		t.Fatal(err)
	}
	buildName, _ := diskJobName(image, testDiskKeyID)
	completeJob(t, r, buildName)
	res := testBuildResult()
	raw, _ := json.Marshal(res)
	buildPod(t, r, buildName, string(raw))

	if _, done, err := r.ensureLauncherDisk(ctx, sb); err != nil || done {
		t.Fatalf("after the build = %t, %v; want a publish Job and a wait", done, err)
	}
	pubName, _ := diskPublishJobName(image, testDiskKeyID)
	pub := &batchv1.Job{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: "setec-system", Name: pubName}, pub); err != nil {
		t.Fatalf("the publish Job: %v", err)
	}
	if got := pub.Spec.Template.Labels["app.kubernetes.io/component"]; got != componentDiskPublisher {
		t.Fatalf("the publish Job component label = %q", got)
	}
	noSeed(t, pub, "disk-seed")
	c := pub.Spec.Template.Spec.Containers[0]
	if !slices.Contains(c.Args, "publish") || c.Args[len(c.Args)-1] != image {
		t.Fatalf("the publish Job args = %v", c.Args)
	}
	env := map[string]string{}
	for _, e := range c.Env {
		env[e.Name] = e.Value
	}
	var sig diskbuilder.Signature
	if err := json.Unmarshal([]byte(env["SETEC_DISK_SIGNATURE"]), &sig); err != nil {
		t.Fatalf("SETEC_DISK_SIGNATURE: %v", err)
	}
	if got, err := diskbuilder.ParseBuildResult([]byte(env["SETEC_DISK_BUILD"])); err != nil || got != res {
		t.Fatalf("SETEC_DISK_BUILD = %+v, %v", got, err)
	}
	pubKey, _ := base64.StdEncoding.DecodeString(testDiskPublicKey)
	if sig.ImageDigest != "sha256:"+strings.Repeat("12", 32) || sig.DiskSHA256 != res.DiskSHA256 ||
		!ed25519.Verify(pubKey, sig.Payload(), sig.Value) {
		t.Fatalf("the signature %+v does not sign the image and the disk of the report", sig)
	}

	completeJob(t, r, pubName)
	if keyID, done, err := r.ensureLauncherDisk(ctx, sb); err != nil || !done || keyID != testDiskKeyID {
		t.Fatalf("after the publish = %q, %t, %v; want done", keyID, done, err)
	}
}

// A complete build Job whose Pod is gone (Pod GC, a lost node) is deleted,
// so the next pass builds again. It is not a failed disk.
func TestEnsureLauncherDisk_ABuildJobWithNoPodBuildsAgain(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := diskReconciler(t)
	image := "ghcr.io/org/tool@sha256:" + strings.Repeat("56", 32)
	sb := &setecv1alpha1.Sandbox{Name: "a", Namespace: "tenant"}
	sb.Spec.Image = image
	if _, _, err := r.ensureLauncherDisk(ctx, sb); err != nil {
		t.Fatal(err)
	}
	buildName, _ := diskJobName(image, testDiskKeyID)
	completeJob(t, r, buildName)
	if _, done, err := r.ensureLauncherDisk(ctx, sb); err != nil || done {
		t.Fatalf("= %t, %v; want a wait and no error", done, err)
	}
	if err := r.Get(ctx, types.NamespacedName{Namespace: "setec-system", Name: buildName}, &batchv1.Job{}); !apierrors.IsNotFound(err) {
		t.Fatalf("the build Job with no Pod: %v, want it deleted", err)
	}
	if _, _, err := r.ensureLauncherDisk(ctx, sb); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(ctx, types.NamespacedName{Namespace: "setec-system", Name: buildName}, &batchv1.Job{}); err != nil {
		t.Fatalf("the next pass made no build Job: %v", err)
	}
}

// A build report that is not strict JSON, or that the Pod never wrote, is a
// failed disk: the operator signs nothing and starts no publish Job.
func TestEnsureLauncherDisk_ABadBuildReportFails(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for name, message := range map[string]string{
		"not JSON":      "pwned",
		"short digest":  `{"layer":{"digest":"sha256:1","diffID":"sha256:1","size":1},"diskSHA256":"x"}`,
		"unknown field": `{"exists":true,"layer":{"digest":"","diffID":"","size":0},"key":"x"}`,
	} {
		r := diskReconciler(t)
		image := "ghcr.io/org/tool@sha256:" + strings.Repeat("34", 32)
		sb := &setecv1alpha1.Sandbox{Name: "a", Namespace: "tenant"}
		sb.Spec.Image = image
		if _, _, err := r.ensureLauncherDisk(ctx, sb); err != nil {
			t.Fatal(err)
		}
		buildName, _ := diskJobName(image, testDiskKeyID)
		completeJob(t, r, buildName)
		buildPod(t, r, buildName, message)
		_, done, err := r.ensureLauncherDisk(ctx, sb)
		var dbe *diskBuildError
		if done || !errors.As(err, &dbe) {
			t.Fatalf("%s: = %t, %v; want a diskBuildError", name, done, err)
		}
		jobs := &batchv1.JobList{}
		_ = r.List(ctx, jobs)
		if len(jobs.Items) != 1 {
			t.Fatalf("%s: jobs = %d, want only the build Job", name, len(jobs.Items))
		}
	}
}

// An image of a registry that the build Job cannot reach is refused before
// any Job starts.
func TestEnsureLauncherDisk_RefusesAnImageOfAnotherRegistry(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := diskReconciler(t)
	sb := &setecv1alpha1.Sandbox{Name: "a", Namespace: "tenant"}
	sb.Spec.Image = "evil.example.com/tool@sha256:" + strings.Repeat("ab", 32)
	_, done, err := r.ensureLauncherDisk(ctx, sb)
	var dbe *diskBuildError
	if done || !errors.As(err, &dbe) || !strings.Contains(err.Error(), "evil.example.com") {
		t.Fatalf("= %t, %v; want a diskBuildError that names the registry", done, err)
	}
	jobs := &batchv1.JobList{}
	_ = r.List(ctx, jobs)
	if len(jobs.Items) != 0 {
		t.Fatalf("jobs = %d, want none", len(jobs.Items))
	}

	// docker.io names the registry index.docker.io, as a reference does.
	r.DiskBuilder.ImageRegistries = []string{"docker.io"}
	sb.Spec.Image = "docker.io/library/busybox@sha256:" + strings.Repeat("ab", 32)
	if _, _, err := r.ensureLauncherDisk(ctx, sb); err != nil {
		t.Fatalf("an image of an allowed registry = %v", err)
	}
}

// No Job of the disk builder image mounts the signing seed: not the build
// Job, not the publish Job, not the image verify Job. The verify Job also
// gets no push credential.
func TestBuilderJobs_NoJobMountsTheSeed(t *testing.T) {
	t.Parallel()
	cfg := DiskBuilderConfig{Image: "b:v1", Namespace: "setec-system", SigningSecret: "disk-seed", RegistrySecret: "disk-push"}
	image := "ghcr.io/org/tool@sha256:" + strings.Repeat("ab", 32)
	key, _ := diskbuilder.ParsePrivateKey([]byte(testDiskSeed))
	jobs := []*batchv1.Job{
		diskJob(cfg, "r", "b", image, testDiskKeyID),
		publishJob(cfg, "r", "p", image, testDiskKeyID, testBuildResult(),
			diskbuilder.SignDigest(key, "sha256:"+strings.Repeat("ab", 32), strings.Repeat("d", 64))),
	}
	for _, j := range jobs {
		noSeed(t, j, cfg.SigningSecret)
		if j.Spec.Template.Spec.Containers[0].TerminationMessagePolicy != corev1.TerminationMessageReadFile {
			t.Fatalf("the Job %s does not read its result from the termination file", j.Name)
		}
	}
	v := builderJob(cfg, "v", componentImageVerify, []string{"verify", image}, nil, false)
	if len(v.Spec.Template.Spec.Volumes) != 1 {
		t.Fatalf("the verify Job has volumes %v; want only the work volume", v.Spec.Template.Spec.Volumes)
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
	completeJob(t, r, oldName)

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
