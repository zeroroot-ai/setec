// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
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
	cls.Spec.Runtime = &setecv1alpha1.SandboxClassRuntime{Backend: runtimepkg.BackendLauncher}
	cls.Spec.PreWarmPoolSize = 2
	cls.Spec.PreWarmImage = poolImage
	cls.Spec.DefaultResources = &setecv1alpha1.Resources{VCPU: 1, Memory: resource.MustParse("1Gi")}
	return cls
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
	key := baseKeyOf(poolClass(), poolLaunch)
	if p.Annotations[snapshotpkg.BaseKeyAnnotation] != key || p.Labels[snapshotpkg.BaseClassLabel] != "tools" {
		t.Fatalf("base Pod = %v %v", p.Labels, p.Annotations)
	}

	p.Spec.NodeName = "node-a"
	if err := r.Update(ctx, &p); err != nil {
		t.Fatal(err)
	}
	p.Status.Phase = corev1.PodRunning
	p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	if err := r.Status().Update(ctx, &p); err != nil {
		t.Fatal(err)
	}
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
	for _, q := range basePods(t, r) {
		if q.Name == p.Name {
			t.Fatal("the base Pod stays after its snapshot")
		}
		// The next base goes to another node.
		mf := q.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0].MatchFields
		if len(mf) == 0 || mf[len(mf)-1].Operator != corev1.NodeSelectorOpNotIn || mf[len(mf)-1].Values[0] != "node-a" {
			t.Fatalf("the second base Pod may land on node-a: %+v", mf)
		}
	}
	cls := &setecv1alpha1.SandboxClass{}
	_ = r.Get(ctx, types.NamespacedName{Name: "tools"}, cls)
	if cls.Status.WarmPool == nil || cls.Status.WarmPool.Ready != 1 || cls.Status.WarmPool.Key != key {
		t.Fatalf("pool status = %+v", cls.Status.WarmPool)
	}
}

// TestWarmPool_DropsStaleAndIdleBases proves that a base with an old key
// goes at once, and that a pool whose image nobody ran for 7 days keeps
// no base.
func TestWarmPool_DropsStaleAndIdleBases(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	key := baseKeyOf(poolClass(), poolLaunch)
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
	if err := r.Get(ctx, types.NamespacedName{Namespace: poolNS, Name: "old"}, &setecv1alpha1.Snapshot{}); err == nil {
		t.Fatal("a stale base stays")
	}
	if err := r.Get(ctx, types.NamespacedName{Namespace: poolNS, Name: "cur"}, &setecv1alpha1.Snapshot{}); err != nil {
		t.Fatalf("the current base went: %v", err)
	}

	cls := &setecv1alpha1.SandboxClass{}
	_ = r.Get(ctx, types.NamespacedName{Name: "tools"}, cls)
	old := metav1.NewTime(time.Now().Add(-8 * 24 * time.Hour))
	cls.Status.WarmPool.LastUsed = &old
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

// TestSelectBase_OnlyACleanCurrentBaseOfTheSameSize pins which Sandbox may
// warm start and from which base.
func TestSelectBase_OnlyACleanCurrentBaseOfTheSameSize(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	key := baseKeyOf(poolClass(), poolLaunch)
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
