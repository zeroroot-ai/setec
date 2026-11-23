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
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
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

// diskPod is the Pod of the disk Job name after its init containers ended:
// the check container with checkMsg and the build container with buildMsg.
func diskPod(t *testing.T, r *SandboxReconciler, name, checkMsg, buildMsg string) {
	t.Helper()
	pod := &corev1.Pod{Name: name + "-x", Namespace: "setec-system",
		Labels: map[string]string{batchv1.JobNameLabel: name}}
	pod.Spec.Containers = []corev1.Container{{Name: publishContainer, Image: "x"}}
	if err := r.Create(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	ended := func(n, msg string) corev1.ContainerStatus {
		return corev1.ContainerStatus{Name: n, State: corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{Message: msg},
		}}
	}
	pod.Status.Phase = corev1.PodRunning
	pod.Status.InitContainerStatuses = []corev1.ContainerStatus{ended(checkContainer, checkMsg), ended(builderContainer, buildMsg)}
	if err := r.Status().Update(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
}

// noSecretIn fails when container c of the Job mounts a Secret volume, or
// reads an env value from a reference.
func noSecretIn(t *testing.T, job *batchv1.Job, c corev1.Container) {
	t.Helper()
	vols := map[string]corev1.Volume{}
	for _, v := range job.Spec.Template.Spec.Volumes {
		vols[v.Name] = v
	}
	for _, m := range c.VolumeMounts {
		if v := vols[m.Name]; v.Secret != nil || v.Projected != nil {
			t.Fatalf("the container %s of the Job %s mounts the Secret volume %s", c.Name, job.Name, m.Name)
		}
	}
	for _, e := range c.Env {
		if e.ValueFrom != nil {
			t.Fatalf("the container %s reads %s from a reference", c.Name, e.Name)
		}
	}
}

// noSeed fails when a volume of the Job is the signing Secret.
func noSeed(t *testing.T, job *batchv1.Job, signingSecret string) {
	t.Helper()
	for _, v := range job.Spec.Template.Spec.Volumes {
		if v.Secret != nil && v.Secret.SecretName == signingSecret {
			t.Fatalf("the Job %s mounts the signing Secret", job.Name)
		}
	}
}

// checkDiskJobShape checks the containers, the arguments, the pod settings
// and the secrets of the disk Job of image.
func checkDiskJobShape(t *testing.T, job *batchv1.Job, image string) {
	t.Helper()
	spec := job.Spec.Template.Spec
	if len(spec.InitContainers) != 2 || spec.InitContainers[0].Name != checkContainer ||
		spec.InitContainers[1].Name != builderContainer || len(spec.Containers) != 1 || spec.Containers[0].Name != publishContainer {
		t.Fatalf("the Job containers = %v / %v; want check, build, publish", spec.InitContainers, spec.Containers)
	}
	for _, c := range append(append([]corev1.Container{}, spec.InitContainers...), spec.Containers...) {
		if c.Image != "ghcr.io/org/setec-disk-builder:v1" || c.Args[len(c.Args)-1] != image {
			t.Fatalf("the container %s = %s %v", c.Name, c.Image, c.Args)
		}
	}
	if i := slices.Index(spec.Containers[0].Args, "--key-id"); i < 0 || spec.Containers[0].Args[i+1] != testDiskKeyID {
		t.Fatalf("the publish args %v do not pass the key id %s", spec.Containers[0].Args, testDiskKeyID)
	}
	if *spec.AutomountServiceAccountToken || !*spec.SecurityContext.RunAsNonRoot {
		t.Fatalf("the Job pod = %+v", spec)
	}
	if got := job.Spec.Template.Labels[componentLabel]; got != componentDiskBuilder {
		t.Fatalf("the Job pod component label = %q; the chart policy and the e2e diagnostics select the pod by it", got)
	}
	noSeed(t, job, "disk-seed")
	// The container that reads the image holds no secret at all.
	noSecretIn(t, job, spec.InitContainers[1])
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
	checkDiskJobShape(t, job, image)

	if _, done, err := r.ensureLauncherDisk(ctx, sb); err != nil || done {
		t.Fatalf("a running Job = %t, %v; want a wait", done, err)
	}
	completeJob(t, r, name)
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

// TestEnsureLauncherDisk_TheOperatorSignsTheBuildReport: after the build
// container reports, the operator signs the image digest and the disk of the
// report, and writes the ConfigMap that the publish container mounts. The
// signature verifies under the public key of the seed.
func TestEnsureLauncherDisk_TheOperatorSignsTheBuildReport(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := diskReconciler(t)
	image := "ghcr.io/org/tool@sha256:" + strings.Repeat("12", 32)
	sb := &setecv1alpha1.Sandbox{Name: "a", Namespace: "tenant"}
	sb.Spec.Image = image
	if _, _, err := r.ensureLauncherDisk(ctx, sb); err != nil {
		t.Fatal(err)
	}
	name, _ := diskJobName(image, testDiskKeyID)
	sha := strings.Repeat("d", 64)
	diskPod(t, r, name, `{"exists":false}`, `{"diskSHA256":"`+sha+`"}`)
	if _, done, err := r.ensureLauncherDisk(ctx, sb); err != nil || done {
		t.Fatalf("after the build = %t, %v; want a wait", done, err)
	}
	cm := &corev1.ConfigMap{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: "setec-system", Name: name + "-sig"}, cm); err != nil {
		t.Fatalf("the signature ConfigMap: %v", err)
	}
	if len(cm.OwnerReferences) != 1 || cm.OwnerReferences[0].Name != name {
		t.Fatalf("the signature ConfigMap owners = %v; want the Job", cm.OwnerReferences)
	}
	var sig diskbuilder.Signature
	if err := json.Unmarshal([]byte(cm.Data[signatureKey]), &sig); err != nil {
		t.Fatal(err)
	}
	pub, _ := base64.StdEncoding.DecodeString(testDiskPublicKey)
	if sig.ImageDigest != "sha256:"+strings.Repeat("12", 32) || sig.DiskSHA256 != sha || !ed25519.Verify(pub, sig.Payload(), sig.Value) {
		t.Fatalf("the signature %+v does not sign the image and the disk of the report", sig)
	}
	// The publish container mounts the ConfigMap; the build container
	// mounts neither it nor the credential.
	job := &batchv1.Job{}
	_ = r.Get(ctx, types.NamespacedName{Namespace: "setec-system", Name: name}, job)
	if !slices.ContainsFunc(job.Spec.Template.Spec.Containers[0].VolumeMounts, func(m corev1.VolumeMount) bool {
		return m.Name == "signature"
	}) || slices.ContainsFunc(job.Spec.Template.Spec.InitContainers[1].VolumeMounts, func(m corev1.VolumeMount) bool {
		return m.Name == "signature" || m.Name == "registry"
	}) {
		t.Fatal("the signature or the credential is in the wrong container")
	}
	completeJob(t, r, name)
	if keyID, done, err := r.ensureLauncherDisk(ctx, sb); err != nil || !done || keyID != testDiskKeyID {
		t.Fatalf("after the publish = %q, %t, %v; want done", keyID, done, err)
	}
}

// When the check found the disk, the operator signs nothing.
func TestEnsureLauncherDisk_AnExistingDiskIsNotSigned(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := diskReconciler(t)
	image := "ghcr.io/org/tool@sha256:" + strings.Repeat("78", 32)
	sb := &setecv1alpha1.Sandbox{Name: "a", Namespace: "tenant"}
	sb.Spec.Image = image
	if _, _, err := r.ensureLauncherDisk(ctx, sb); err != nil {
		t.Fatal(err)
	}
	name, _ := diskJobName(image, testDiskKeyID)
	diskPod(t, r, name, `{"exists":true}`, `{"diskSHA256":"`+strings.Repeat("e", 64)+`"}`)
	if _, _, err := r.ensureLauncherDisk(ctx, sb); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(ctx, types.NamespacedName{Namespace: "setec-system", Name: name + "-sig"}, &corev1.ConfigMap{}); !apierrors.IsNotFound(err) {
		t.Fatalf("the signature ConfigMap of an existing disk: %v, want none", err)
	}
}

// A build report that is not strict JSON is a failed disk: the operator
// signs nothing.
func TestEnsureLauncherDisk_ABadBuildReportFails(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for what, message := range map[string]string{
		"not JSON":      "pwned",
		"short sha":     `{"diskSHA256":"abc"}`,
		"unknown field": `{"diskSHA256":"` + strings.Repeat("a", 64) + `","key":"x"}`,
	} {
		r := diskReconciler(t)
		image := "ghcr.io/org/tool@sha256:" + strings.Repeat("34", 32)
		sb := &setecv1alpha1.Sandbox{Name: "a", Namespace: "tenant"}
		sb.Spec.Image = image
		if _, _, err := r.ensureLauncherDisk(ctx, sb); err != nil {
			t.Fatal(err)
		}
		name, _ := diskJobName(image, testDiskKeyID)
		diskPod(t, r, name, `{"exists":false}`, message)
		_, done, err := r.ensureLauncherDisk(ctx, sb)
		var dbe *diskBuildError
		if done || !errors.As(err, &dbe) {
			t.Fatalf("%s: = %t, %v; want a diskBuildError", what, done, err)
		}
		if err := r.Get(ctx, types.NamespacedName{Namespace: "setec-system", Name: name + "-sig"}, &corev1.ConfigMap{}); !apierrors.IsNotFound(err) {
			t.Fatalf("%s: a signature exists: %v", what, err)
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

// No container of a disk Job mounts the signing seed. The build container,
// which reads the image, mounts no secret and no signature. The verify Job
// gets no push credential.
func TestBuilderJobs_NoJobMountsTheSeed(t *testing.T) {
	t.Parallel()
	cfg := DiskBuilderConfig{Image: "b:v1", Namespace: "setec-system", SigningSecret: "disk-seed", RegistrySecret: "disk-push"}
	image := "ghcr.io/org/tool@sha256:" + strings.Repeat("ab", 32)
	j := diskJob(cfg, "r", "b", image, testDiskKeyID)
	noSeed(t, j, cfg.SigningSecret)
	spec := j.Spec.Template.Spec
	noSecretIn(t, j, spec.InitContainers[1])
	for _, c := range append(append([]corev1.Container{}, spec.InitContainers...), spec.Containers...) {
		if c.TerminationMessagePolicy != corev1.TerminationMessageReadFile {
			t.Fatalf("the container %s does not write its report to the termination file", c.Name)
		}
	}
	v := builderJob(cfg, "v", componentImageVerify, []string{"verify", image}, nil)
	if len(v.Spec.Template.Spec.Volumes) != 1 {
		t.Fatalf("the verify Job has volumes %v; want only the work volume", v.Spec.Template.Spec.Volumes)
	}
}

func TestEnsureLauncherDisk_BadImage(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := diskReconciler(t)
	sb := &setecv1alpha1.Sandbox{Name: "a", Namespace: "tenant"}
	sb.Spec.Image = "ghcr.io/org/tool:latest"
	if _, _, err := r.ensureLauncherDisk(ctx, sb); err == nil {
		t.Fatal("an image with no digest was accepted")
	}
}

// clock is a movable time for the retry backoff.
type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

// failJob marks the Job name as failed at the time of the clock.
func failJob(t *testing.T, r *SandboxReconciler, c *clock, name, msg string) {
	t.Helper()
	job := &batchv1.Job{}
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: "setec-system", Name: name}, job); err != nil {
		t.Fatalf("the Job %s: %v", name, err)
	}
	job.Status.Conditions = []batchv1.JobCondition{{
		Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Message: msg, LastTransitionTime: metav1.NewTime(c.now()),
	}}
	if err := r.Status().Update(context.Background(), job); err != nil {
		t.Fatal(err)
	}
}

func jobNames(t *testing.T, r *SandboxReconciler) []string {
	t.Helper()
	jobs := &batchv1.JobList{}
	if err := r.List(context.Background(), jobs); err != nil {
		t.Fatal(err)
	}
	var names []string
	for i := range jobs.Items {
		names = append(names, jobs.Items[i].Name)
	}
	slices.Sort(names)
	return names
}

// TestEnsureLauncherDisk_AFailedBuildStartsANewAttempt is setec#380: a failed
// Job does not block its image for the life of the Job. A request in the
// backoff waits and is told when the next attempt starts. A request after
// the backoff starts a new Job, which the next requests share.
func TestEnsureLauncherDisk_AFailedBuildStartsANewAttempt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := diskReconciler(t)
	clk := &clock{t: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
	r.DiskBuilder.Now = clk.now
	image := "ghcr.io/org/tool@sha256:" + strings.Repeat("cd", 32)
	sb := &setecv1alpha1.Sandbox{Name: "a", Namespace: "tenant"}
	sb.Spec.Image = image
	base, _ := diskJobName(image, testDiskKeyID)

	if _, done, err := r.ensureLauncherDisk(ctx, sb); err != nil || done {
		t.Fatalf("first call = %t, %v; want a new Job", done, err)
	}
	failJob(t, r, clk, base, "BackoffLimitExceeded")

	clk.advance(10 * time.Second)
	_, done, err := r.ensureLauncherDisk(ctx, sb)
	var retry *diskRetryError
	if done || !errors.As(err, &retry) {
		t.Fatalf("in the backoff = %t, %v; want a diskRetryError", done, err)
	}
	if retry.attempt != 1 || retry.after != 20*time.Second || !strings.Contains(retry.Error(), "BackoffLimitExceeded") {
		t.Fatalf("retry = %+v (%s); want attempt 1, 20s, the cause", retry, retry.Error())
	}
	if got := jobNames(t, r); !slices.Equal(got, []string{base}) {
		t.Fatalf("jobs in the backoff = %v; want only the failed one", got)
	}

	clk.advance(21 * time.Second)
	keyID, done, err := r.ensureLauncherDisk(ctx, sb)
	if err != nil || done || keyID != testDiskKeyID {
		t.Fatalf("after the backoff = %q, %t, %v; want a new Job and a wait", keyID, done, err)
	}
	second := base + "-a2"
	if got := jobNames(t, r); !slices.Equal(got, []string{base, second}) {
		t.Fatalf("jobs after the backoff = %v; want the failed Job and the new one", got)
	}
	job := &batchv1.Job{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: "setec-system", Name: second}, job); err != nil {
		t.Fatal(err)
	}
	checkDiskJobShape(t, job, image)
	if job.Annotations[attemptAnnotation] != "2" || *job.Spec.TTLSecondsAfterFinished != diskJobTTLSeconds {
		t.Fatalf("attempt 2 = %v, ttl %d", job.Annotations, *job.Spec.TTLSecondsAfterFinished)
	}

	// A second Sandbox shares the new Job. The failed Job of attempt 1 can
	// expire and the new Job still counts.
	if err := r.Delete(ctx, &batchv1.Job{Name: base, Namespace: "setec-system"}); err != nil {
		t.Fatal(err)
	}
	other := sb.DeepCopy()
	other.Name = "b"
	if _, done, err := r.ensureLauncherDisk(ctx, other); err != nil || done {
		t.Fatalf("a second request = %t, %v; want a wait on attempt 2", done, err)
	}
	if got := jobNames(t, r); !slices.Equal(got, []string{second}) {
		t.Fatalf("jobs = %v; want attempt 2 only", got)
	}
	completeJob(t, r, second)
	if _, done, err := r.ensureLauncherDisk(ctx, sb); err != nil || !done {
		t.Fatalf("a complete second attempt = %t, %v; want done", done, err)
	}
}

// TestEnsureLauncherDisk_AFailedSeriesStops is the bound of setec#380: an
// image that fails each time gets four attempts and then no more, however
// many requests come. The Job of the last attempt stays for an hour.
func TestEnsureLauncherDisk_AFailedSeriesStops(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := diskReconciler(t)
	clk := &clock{t: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
	r.DiskBuilder.Now = clk.now
	image := "ghcr.io/org/tool@sha256:" + strings.Repeat("ef", 32)
	sb := &setecv1alpha1.Sandbox{Name: "a", Namespace: "tenant"}
	sb.Spec.Image = image
	base, _ := diskJobName(image, testDiskKeyID)

	if _, _, err := r.ensureLauncherDisk(ctx, sb); err != nil {
		t.Fatal(err)
	}
	for n := 1; n < diskBuildAttempts(); n++ {
		failJob(t, r, clk, diskAttemptJobName(base, n), "BackoffLimitExceeded")
		clk.advance(diskBuildBackoff[n-1] + time.Second)
		if _, done, err := r.ensureLauncherDisk(ctx, sb); err != nil || done {
			t.Fatalf("attempt %d after its backoff = %t, %v; want the next attempt", n, done, err)
		}
	}
	last := diskAttemptJobName(base, diskBuildAttempts())
	if got := jobNames(t, r); len(got) != diskBuildAttempts() || !slices.Contains(got, last) {
		t.Fatalf("jobs = %v; want one Job for each of %d attempts", got, diskBuildAttempts())
	}
	job := &batchv1.Job{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: "setec-system", Name: last}, job); err != nil {
		t.Fatal(err)
	}
	if *job.Spec.TTLSecondsAfterFinished != diskJobLastTTLSeconds {
		t.Fatalf("the last attempt ttl = %d, want %d", *job.Spec.TTLSecondsAfterFinished, diskJobLastTTLSeconds)
	}

	failJob(t, r, clk, last, "BackoffLimitExceeded")
	for range 3 {
		clk.advance(30 * time.Minute)
		_, done, err := r.ensureLauncherDisk(ctx, sb)
		var dbe *diskBuildError
		if done || !errors.As(err, &dbe) {
			t.Fatalf("after the last attempt = %t, %v; want a diskBuildError", done, err)
		}
		if !strings.Contains(err.Error(), "failed 4 times") {
			t.Fatalf("error = %q; want the number of attempts", err)
		}
		if got := jobNames(t, r); len(got) != diskBuildAttempts() {
			t.Fatalf("jobs = %v; a request after the last attempt started a build", got)
		}
	}

	// When the Jobs have expired, the next request starts a new series.
	for _, name := range jobNames(t, r) {
		if err := r.Delete(ctx, &batchv1.Job{Name: name, Namespace: "setec-system"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, done, err := r.ensureLauncherDisk(ctx, sb); err != nil || done {
		t.Fatalf("after the Jobs expired = %t, %v; want a new build", done, err)
	}
	if got := jobNames(t, r); !slices.Equal(got, []string{base}) {
		t.Fatalf("jobs = %v; want attempt 1 of a new series", got)
	}
}

// TestEnsureLauncherDisk_AFailedJobOf0130 is the upgrade from a release that
// left a failed Job with the name of the digest and no attempt annotation:
// it counts as attempt 1.
func TestEnsureLauncherDisk_AFailedJobOf0130(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := diskReconciler(t)
	clk := &clock{t: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
	r.DiskBuilder.Now = clk.now
	image := "ghcr.io/org/tool@sha256:" + strings.Repeat("12", 32)
	sb := &setecv1alpha1.Sandbox{Name: "a", Namespace: "tenant"}
	sb.Spec.Image = image
	base, _ := diskJobName(image, testDiskKeyID)
	old := &batchv1.Job{Name: base, Namespace: "setec-system"}
	old.Spec.Template.Spec.Containers = []corev1.Container{{Name: "x", Image: "x"}}
	old.Spec.Template.Spec.RestartPolicy = corev1.RestartPolicyNever
	if err := r.Create(ctx, old); err != nil {
		t.Fatal(err)
	}
	failJob(t, r, clk, base, "BackoffLimitExceeded")
	clk.advance(time.Minute)
	if _, done, err := r.ensureLauncherDisk(ctx, sb); err != nil || done {
		t.Fatalf("a failed Job of 0.130 = %t, %v; want a new attempt", done, err)
	}
	if got := jobNames(t, r); !slices.Equal(got, []string{base, base + "-a2"}) {
		t.Fatalf("jobs = %v; want the old Job and attempt 2", got)
	}
}

// TestWaitForLauncherDisk_SaysWhatHappened: a Sandbox in the backoff is
// Pending with the reason DiskBuildRetrying and a Warning Event with the
// cause and the wait. A Sandbox whose image failed every attempt is Failed
// with DiskBuildFailed and an Event.
func TestWaitForLauncherDisk_SaysWhatHappened(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := diskReconciler(t)
	rec := events.NewFakeRecorder(10)
	r.Recorder = rec
	clk := &clock{t: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
	r.DiskBuilder.Now = clk.now
	image := "ghcr.io/org/tool@sha256:" + strings.Repeat("34", 32)
	sb := &setecv1alpha1.Sandbox{Name: "a", Namespace: "tenant"}
	sb.Spec.Image = image
	if err := r.Create(ctx, sb); err != nil {
		t.Fatal(err)
	}
	base, _ := diskJobName(image, testDiskKeyID)
	if _, _, err := r.ensureLauncherDisk(ctx, sb); err != nil {
		t.Fatal(err)
	}
	failJob(t, r, clk, base, "BackoffLimitExceeded")

	_, proceed, res, err := r.waitForLauncherDisk(ctx, sb)
	if proceed || err != nil || res.RequeueAfter != diskBuildBackoff[0] {
		t.Fatalf("in the backoff = %t, %v, %+v; want a requeue at the end of the backoff", proceed, err, res)
	}
	got := &setecv1alpha1.Sandbox{}
	_ = r.Get(ctx, types.NamespacedName{Namespace: "tenant", Name: "a"}, got)
	if got.Status.Phase != setecv1alpha1.SandboxPhasePending || got.Status.Reason != ReasonDiskBuildRetrying {
		t.Fatalf("status = %s/%s, want Pending/%s", got.Status.Phase, got.Status.Reason, ReasonDiskBuildRetrying)
	}
	if ev := <-rec.Events; !strings.Contains(ev, ReasonDiskBuildRetrying) || !strings.Contains(ev, "BackoffLimitExceeded") ||
		!strings.Contains(ev, "attempt 1 of 4") {
		t.Fatalf("event = %q; want the reason, the cause and the attempt", ev)
	}

	for n := 1; n < diskBuildAttempts(); n++ {
		clk.advance(diskBuildBackoff[n-1] + time.Second)
		if _, _, err := r.ensureLauncherDisk(ctx, sb); err != nil {
			t.Fatal(err)
		}
		failJob(t, r, clk, diskAttemptJobName(base, n+1), "BackoffLimitExceeded")
	}
	if _, proceed, _, err := r.waitForLauncherDisk(ctx, sb); proceed || err != nil {
		t.Fatalf("after the last attempt = %t, %v", proceed, err)
	}
	_ = r.Get(ctx, types.NamespacedName{Namespace: "tenant", Name: "a"}, got)
	if got.Status.Phase != setecv1alpha1.SandboxPhaseFailed || got.Status.Reason != ReasonDiskBuildFailed {
		t.Fatalf("status = %s/%s, want Failed/%s", got.Status.Phase, got.Status.Reason, ReasonDiskBuildFailed)
	}
	var last string
	for len(rec.Events) > 0 {
		last = <-rec.Events
	}
	if !strings.Contains(last, ReasonDiskBuildFailed) || !strings.Contains(last, "failed 4 times") {
		t.Fatalf("last event = %q; want the reason and the number of attempts", last)
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
