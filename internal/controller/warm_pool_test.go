// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package controller

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	setecgrpcv1 "github.com/zeroroot-ai/setec/api/grpc/v1"
	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
	runtimepkg "github.com/zeroroot-ai/setec/internal/runtime"
	snapshotpkg "github.com/zeroroot-ai/setec/internal/snapshot"
)

const (
	poolNS     = "setec-pool"
	poolLaunch = "ghcr.io/zeroroot-ai/setec-launcher:v1"
)

var poolImage = "ghcr.io/org/tools@sha256:" + strings.Repeat("ab", 32)

func poolClass() *setecv1alpha1.SandboxClass {
	cls := &setecv1alpha1.SandboxClass{Name: "tools"}
	// The first pool image counts as asked for at the creation of the class.
	cls.CreationTimestamp = metav1.Now()
	cls.Spec.Runtime = &setecv1alpha1.SandboxClassRuntime{Backend: runtimepkg.BackendLauncher}
	cls.Spec.PreWarmPoolSize = 2
	cls.Spec.PreWarmImage = poolImage
	cls.Spec.DefaultResources = &setecv1alpha1.Resources{VCPU: 1, Memory: resource.MustParse("1Gi")}
	cls.Spec.PreWarmImageSignature = &setecv1alpha1.ImageSignature{
		Issuer: "https://token.actions.githubusercontent.com", Identity: "https://github.com/org/tools/.github/workflows/release.yml@refs/tags/v1",
	}
	return cls
}

// finishVerifyJob ends the image check Job of the pool class, with success
// or with failure.
func finishVerifyJob(t *testing.T, r *WarmPoolReconciler, ok bool) {
	t.Helper()
	finishVerifyJobOf(t, r, poolImage, ok)
}

// finishVerifyJobOf ends the image check Job of one image of the class.
func finishVerifyJobOf(t *testing.T, r *WarmPoolReconciler, image string, ok bool) {
	t.Helper()
	job := &batchv1.Job{}
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: "setec-system", Name: verifyJobName(poolClass(), image)}, job); err != nil {
		t.Fatalf("the image check Job: %v", err)
	}
	cond := batchv1.JobCondition{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}
	if !ok {
		cond = batchv1.JobCondition{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Message: "no signature"}
	}
	job.Status.Conditions = []batchv1.JobCondition{cond}
	if err := r.Status().Update(context.Background(), job); err != nil {
		t.Fatal(err)
	}
}

func poolReconciler(t *testing.T, na *fakeNodeAgentClient, objs ...client.Object) *WarmPoolReconciler {
	t.Helper()
	s := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(s))
	utilruntime.Must(setecv1alpha1.AddToScheme(s))
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).
		WithStatusSubresource(&setecv1alpha1.SandboxClass{}, &setecv1alpha1.Snapshot{}, &batchv1.Job{}).Build()
	return &WarmPoolReconciler{
		Client: c, Namespace: poolNS, LauncherImage: poolLaunch,
		DiskRepo: "ghcr.io/org/setec-disks", DiskKeys: []string{"key"},
		DiskBuilder: DiskBuilderConfig{Image: "builder:v1", Namespace: "setec-system", SigningSecret: "seed"},
		Coordinator: &snapshotpkg.Coordinator{Client: c, Dialer: &fakeNodeAgentDialer{client: na}, PoolNamespace: poolNS},
	}
}

func reconcileClass(t *testing.T, r *WarmPoolReconciler) {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), ctrl.Request{Name: "tools"}); err != nil {
		t.Fatal(err)
	}
}

func completeDiskJob(t *testing.T, r *WarmPoolReconciler) {
	t.Helper()
	name, _ := diskJobName(poolImage)
	job := &batchv1.Job{}
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: "setec-system", Name: name}, job); err != nil {
		t.Fatalf("the disk Job: %v", err)
	}
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	if err := r.Status().Update(context.Background(), job); err != nil {
		t.Fatal(err)
	}
}

func basePods(t *testing.T, r *WarmPoolReconciler) []corev1.Pod {
	t.Helper()
	pods := &corev1.PodList{}
	_ = r.List(context.Background(), pods, client.InNamespace(poolNS), client.MatchingLabels{snapshotpkg.BaseLabel: "true"})
	return pods.Items
}

// TestWarmPool_BuildsABaseFromAReadyBasePod walks one base: the disk, the
// base Pod, the snapshot of its Ready machine with the secret scan, and
// the end of the Pod.
func TestWarmPool_BuildsABaseFromAReadyBasePod(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	na := &fakeNodeAgentClient{CreateResp: &setecgrpcv1.CreateSnapshotResponse{StorageRef: "ref", CleanBaseVerified: true}}
	r := poolReconciler(t, na, poolClass())

	reconcileClass(t, r)
	finishVerifyJob(t, r, true)
	reconcileClass(t, r)
	if len(basePods(t, r)) != 0 {
		t.Fatal("a base Pod started before the disk of the image")
	}
	completeDiskJob(t, r)
	reconcileClass(t, r)
	pods := basePods(t, r)
	if len(pods) != 1 {
		t.Fatalf("base Pods = %d, want one per pass", len(pods))
	}
	p := pods[0]
	key := baseKeyOf(poolClass(), poolImage, poolLaunch)
	if p.Annotations[snapshotpkg.BaseKeyAnnotation] != key || p.Labels[snapshotpkg.BaseClassLabel] != "tools" {
		t.Fatalf("base Pod = %v %v", p.Labels, p.Annotations)
	}

	markBasePodReady(t, r, &p, "node-a")
	reconcileClass(t, r)
	snap := &setecv1alpha1.Snapshot{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: poolNS, Name: p.Name}, snap); err != nil {
		t.Fatalf("the base Snapshot: %v", err)
	}
	if snap.Status.Phase != setecv1alpha1.SnapshotPhaseReady || snap.Annotations[snapshotpkg.CleanBaseAnnotation] != "true" ||
		snap.Spec.Node != "node-a" || snap.Spec.SourceSandbox != "" {
		t.Fatalf("base = %+v %v %v", snap.Spec, snap.Status, snap.Annotations)
	}
	if na.LastCreate == nil || !na.LastCreate.GetScanForSecrets() {
		t.Fatal("the base was taken with no secret scan")
	}
	assertNextBasePods(t, r, p.Name, "node-a")
	cls := &setecv1alpha1.SandboxClass{}
	_ = r.Get(ctx, types.NamespacedName{Name: "tools"}, cls)
	if ws := cls.Status.WarmPool; ws == nil || len(ws.Images) != 1 || ws.Images[0].Ready != 1 || ws.Images[0].Key != key {
		t.Fatalf("pool status = %+v", cls.Status.WarmPool)
	}
}

// markBasePodReady puts the base Pod p on node, running and Ready.
func markBasePodReady(t *testing.T, r *WarmPoolReconciler, p *corev1.Pod, node string) {
	t.Helper()
	ctx := context.Background()
	p.Spec.NodeName = node
	if err := r.Update(ctx, p); err != nil {
		t.Fatal(err)
	}
	p.Status.Phase = corev1.PodRunning
	p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	if err := r.Status().Update(ctx, p); err != nil {
		t.Fatal(err)
	}
}

// assertNextBasePods checks that the base Pod done is gone, and that each
// next base Pod avoids node.
func assertNextBasePods(t *testing.T, r *WarmPoolReconciler, done, node string) {
	t.Helper()
	for _, q := range basePods(t, r) {
		if q.Name == done {
			t.Fatal("the base Pod stays after its snapshot")
		}
		mf := q.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0].MatchFields
		if len(mf) == 0 || mf[len(mf)-1].Operator != corev1.NodeSelectorOpNotIn || mf[len(mf)-1].Values[0] != node {
			t.Fatalf("the next base Pod may land on %s: %+v", node, mf)
		}
	}
}

// TestWarmPool_DropsStaleAndIdleBases proves that a base with an old key
// goes at once, and that a pool whose image nobody ran for 7 days keeps
// no base.
func TestWarmPool_DropsStaleAndIdleBases(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	key := baseKeyOf(poolClass(), poolImage, poolLaunch)
	base := func(name, k string) *setecv1alpha1.Snapshot {
		s := &setecv1alpha1.Snapshot{Name: name, Namespace: poolNS,
			Labels:      map[string]string{snapshotpkg.BaseLabel: "true", snapshotpkg.BaseClassLabel: "tools"},
			Annotations: map[string]string{snapshotpkg.BaseKeyAnnotation: k, snapshotpkg.CleanBaseAnnotation: "true"}}
		s.Spec = setecv1alpha1.SnapshotSpec{SandboxClass: "tools", StorageBackend: "local-disk", StorageRef: name, Node: "n-" + name}
		s.Status.Phase = setecv1alpha1.SnapshotPhaseReady
		return s
	}
	r := poolReconciler(t, &fakeNodeAgentClient{}, poolClass(), base("old", "stale-key"), base("cur", key))
	reconcileClass(t, r)
	finishVerifyJob(t, r, true)
	reconcileClass(t, r)
	if err := r.Get(ctx, types.NamespacedName{Namespace: poolNS, Name: "old"}, &setecv1alpha1.Snapshot{}); err == nil {
		t.Fatal("a stale base stays")
	}
	if err := r.Get(ctx, types.NamespacedName{Namespace: poolNS, Name: "cur"}, &setecv1alpha1.Snapshot{}); err != nil {
		t.Fatalf("the current base went: %v", err)
	}

	// The image becomes idle: no first pool image keeps it, and the last
	// Sandbox that asked for it was 8 days ago.
	cls := &setecv1alpha1.SandboxClass{}
	_ = r.Get(ctx, types.NamespacedName{Name: "tools"}, cls)
	cls.Spec.PreWarmImage = ""
	if err := r.Update(ctx, cls); err != nil {
		t.Fatal(err)
	}
	cls.Status.WarmPool.Images[0].LastUsed = metav1.NewTime(time.Now().Add(-8 * 24 * time.Hour))
	if err := r.Status().Update(ctx, cls); err != nil {
		t.Fatal(err)
	}
	reconcileClass(t, r)
	if err := r.Get(ctx, types.NamespacedName{Namespace: poolNS, Name: "cur"}, &setecv1alpha1.Snapshot{}); err == nil {
		t.Fatal("an idle pool keeps its base")
	}
	if len(basePods(t, r)) != 0 {
		t.Fatal("an idle pool starts a base")
	}
}

// TestWarmPool_KeepsBasesForEachImageAndDropsAnIdleOne covers two images
// of one class (setec#238): each gets its own check, disk and bases, and
// the image that nobody asked for in 7 days loses its bases and leaves the
// status, while the other keeps them.
func TestWarmPool_KeepsBasesForEachImageAndDropsAnIdleOne(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	second := "ghcr.io/org/scanner@sha256:" + strings.Repeat("cd", 32)
	cls := poolClass()
	cls.Status.WarmPool = &setecv1alpha1.SandboxClassWarmPoolStatus{Images: []setecv1alpha1.SandboxClassWarmPoolImage{
		{Image: second, LastUsed: metav1.Now()},
	}}
	base := func(name, image string) *setecv1alpha1.Snapshot {
		s := &setecv1alpha1.Snapshot{Name: name, Namespace: poolNS,
			Labels: map[string]string{snapshotpkg.BaseLabel: "true", snapshotpkg.BaseClassLabel: "tools"},
			Annotations: map[string]string{snapshotpkg.BaseKeyAnnotation: baseKeyOf(poolClass(), image, poolLaunch),
				snapshotpkg.CleanBaseAnnotation: "true"}}
		s.Spec = setecv1alpha1.SnapshotSpec{SandboxClass: "tools", StorageBackend: "local-disk", Node: "n-" + name}
		s.Status.Phase = setecv1alpha1.SnapshotPhaseReady
		return s
	}
	r := poolReconciler(t, &fakeNodeAgentClient{}, cls, base("first", poolImage), base("second", second))

	reconcileClass(t, r)
	finishVerifyJobOf(t, r, poolImage, true)
	finishVerifyJobOf(t, r, second, true)
	reconcileClass(t, r)
	for _, img := range []string{poolImage, second} {
		name, _ := diskJobName(img)
		if err := r.Get(ctx, types.NamespacedName{Namespace: "setec-system", Name: name}, &batchv1.Job{}); err != nil {
			t.Fatalf("no disk Job for the pool image %s: %v", img, err)
		}
	}
	got := &setecv1alpha1.SandboxClass{}
	_ = r.Get(ctx, types.NamespacedName{Name: "tools"}, got)
	if ws := got.Status.WarmPool; ws == nil || len(ws.Images) != 2 || ws.Images[0].Ready != 1 || ws.Images[1].Ready != 1 {
		t.Fatalf("pool status = %+v, want two images with one Ready base each", got.Status.WarmPool)
	}

	// Nobody asked for the second image in 8 days.
	for i := range got.Status.WarmPool.Images {
		if got.Status.WarmPool.Images[i].Image == second {
			got.Status.WarmPool.Images[i].LastUsed = metav1.NewTime(time.Now().Add(-8 * 24 * time.Hour))
		}
	}
	if err := r.Status().Update(ctx, got); err != nil {
		t.Fatal(err)
	}
	reconcileClass(t, r)
	if err := r.Get(ctx, types.NamespacedName{Namespace: poolNS, Name: "second"}, &setecv1alpha1.Snapshot{}); err == nil {
		t.Fatal("the idle image keeps its base")
	}
	if err := r.Get(ctx, types.NamespacedName{Namespace: poolNS, Name: "first"}, &setecv1alpha1.Snapshot{}); err != nil {
		t.Fatalf("the busy image lost its base: %v", err)
	}
	_ = r.Get(ctx, types.NamespacedName{Name: "tools"}, got)
	if ws := got.Status.WarmPool; ws == nil || len(ws.Images) != 1 || ws.Images[0].Image != poolImage {
		t.Fatalf("pool status = %+v, want the busy image only", got.Status.WarmPool)
	}
}

// TestWarmPool_AnImageThatDoesNotVerifyGetsNoBase checks the signature
// gate: no base starts while the check runs, and an image whose check
// fails loses its bases and gets the condition ImageNotVerified.
func TestWarmPool_AnImageThatDoesNotVerifyGetsNoBase(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	key := baseKeyOf(poolClass(), poolImage, poolLaunch)
	b := &setecv1alpha1.Snapshot{Name: "cur", Namespace: poolNS,
		Labels:      map[string]string{snapshotpkg.BaseLabel: "true", snapshotpkg.BaseClassLabel: "tools"},
		Annotations: map[string]string{snapshotpkg.BaseKeyAnnotation: key, snapshotpkg.CleanBaseAnnotation: "true"}}
	b.Status.Phase = setecv1alpha1.SnapshotPhaseReady
	r := poolReconciler(t, &fakeNodeAgentClient{}, poolClass(), b)

	reconcileClass(t, r)
	name, _ := diskJobName(poolImage)
	if err := r.Get(ctx, types.NamespacedName{Namespace: "setec-system", Name: name}, &batchv1.Job{}); err == nil {
		t.Fatal("the disk of the image builds before its signature is checked")
	}
	finishVerifyJob(t, r, false)
	reconcileClass(t, r)
	if err := r.Get(ctx, types.NamespacedName{Namespace: poolNS, Name: "cur"}, &setecv1alpha1.Snapshot{}); err == nil {
		t.Fatal("an image that does not verify keeps its base")
	}
	if len(basePods(t, r)) != 0 {
		t.Fatal("an image that does not verify starts a base")
	}
	cls := &setecv1alpha1.SandboxClass{}
	_ = r.Get(ctx, types.NamespacedName{Name: "tools"}, cls)
	c := meta.FindStatusCondition(cls.Status.Conditions, setecv1alpha1.ConditionImageNotVerified)
	if c == nil || c.Status != metav1.ConditionTrue || c.Reason != reasonImageNotVerified {
		t.Fatalf("condition = %+v, want ImageNotVerified True", c)
	}
}

// TestSelectBase_OnlyACleanCurrentBaseOfTheSameSize pins which Sandbox may
// warm start and from which base.
func TestSelectBase_OnlyACleanCurrentBaseOfTheSameSize(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	key := baseKeyOf(poolClass(), poolImage, poolLaunch)
	mk := func(name string, clean bool, phase setecv1alpha1.SnapshotPhase, k string) *setecv1alpha1.Snapshot {
		s := &setecv1alpha1.Snapshot{Name: name, Namespace: poolNS,
			Labels:      map[string]string{snapshotpkg.BaseLabel: "true", snapshotpkg.BaseClassLabel: "tools"},
			Annotations: map[string]string{snapshotpkg.BaseKeyAnnotation: k}}
		if clean {
			s.Annotations[snapshotpkg.CleanBaseAnnotation] = "true"
		}
		s.Spec = setecv1alpha1.SnapshotSpec{SandboxClass: "tools", StorageBackend: "local-disk", Node: "node-" + name}
		s.Status.Phase = phase
		return s
	}
	pr := poolReconciler(t, &fakeNodeAgentClient{},
		mk("a-dirty", false, setecv1alpha1.SnapshotPhaseReady, key),
		mk("b-old", true, setecv1alpha1.SnapshotPhaseReady, "old"),
		mk("c-building", true, setecv1alpha1.SnapshotPhaseCreating, key),
		mk("d-good", true, setecv1alpha1.SnapshotPhaseReady, key))
	r := &SandboxReconciler{Client: pr.Client, LauncherImage: poolLaunch, WarmPoolNamespace: poolNS}
	sb := &setecv1alpha1.Sandbox{Name: "s", Namespace: "tenant"}
	sb.Spec.Image = poolImage
	sb.Spec.Resources = *poolClass().Spec.DefaultResources
	base, err := r.selectBase(ctx, sb, poolClass())
	if err != nil || base == nil || base.Name != "d-good" {
		t.Fatalf("selectBase = %v, %v; want d-good", base, err)
	}
	other := sb.DeepCopy()
	other.Spec.Resources.Memory = resource.MustParse("2Gi")
	if base, _ := r.selectBase(ctx, other, poolClass()); base != nil {
		t.Fatal("a Sandbox of another size loads a base")
	}
	ref := sb.DeepCopy()
	ref.Spec.SnapshotRef = &setecv1alpha1.SandboxSnapshotRef{Name: "x"}
	if base, _ := r.selectBase(ctx, ref, poolClass()); base != nil {
		t.Fatal("a Sandbox with a snapshotRef loads a base")
	}
}

// TestStampImage records the request of an image: a new image joins, a
// known image gets the new stamp, and past maxPoolImages the image with
// the oldest stamp goes.
func TestStampImage(t *testing.T) {
	t.Parallel()
	img := func(i int) string { return fmt.Sprintf("ghcr.io/org/t%02d@sha256:%064d", i, i) }
	base := time.Now().Add(-time.Hour)
	var images []setecv1alpha1.SandboxClassWarmPoolImage
	for i := range maxPoolImages {
		images = stampImage(images, img(i), metav1.NewTime(base.Add(time.Duration(i)*time.Second)))
	}
	if len(images) != maxPoolImages {
		t.Fatalf("images = %d, want %d", len(images), maxPoolImages)
	}
	now := metav1.Now()
	images = stampImage(images, img(0), now)
	if len(images) != maxPoolImages || !images[0].LastUsed.Equal(&now) {
		t.Fatalf("a known image is not stamped: %+v", images[0])
	}
	images = stampImage(images, img(99), now)
	if len(images) != maxPoolImages {
		t.Fatalf("images = %d after a new image, want the cap %d", len(images), maxPoolImages)
	}
	for _, im := range images {
		if im.Image == img(1) {
			t.Fatal("the image with the oldest stamp stays past the cap")
		}
	}
}
