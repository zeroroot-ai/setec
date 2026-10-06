// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package controller

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
	"github.com/zeroroot-ai/setec/internal/metrics"
	"github.com/zeroroot-ai/setec/internal/podspec"
	runtimepkg "github.com/zeroroot-ai/setec/internal/runtime"
	"github.com/zeroroot-ai/setec/internal/snapshot"
)

// The warm pool of a launcher class (setec#103). A base is a full Snapshot
// of a launcher machine that booted the pool image of the class and ran no
// workload. The pool keeps PreWarmPoolSize Ready bases, each on another
// node, for as long as Sandboxes ask for the image.
const (
	// warmPoolIdle is how long a pool keeps its bases with no Sandbox
	// that asks for its image.
	warmPoolIdle = 7 * 24 * time.Hour
	// warmPoolRequeue is the interval of a pool check.
	warmPoolRequeue = 30 * time.Second
	// lastUsedStep is the smallest step of the LastUsed stamp, so a busy
	// class is not written on each Sandbox.
	lastUsedStep = time.Hour
	// baseWriteTimeout is how long a base may stay in Creating after its
	// Pod is gone before the pool drops it.
	baseWriteTimeout = 15 * time.Minute
	// WarmBaseAnnotation records on a Sandbox the base that its launcher
	// loads, as <namespace>/<name>.
	WarmBaseAnnotation = "setec.zeroroot.ai/warm-base"
)

// WarmPoolReconciler keeps the bases of each launcher class with a pool.
type WarmPoolReconciler struct {
	client.Client
	Coordinator *snapshot.Coordinator
	// Namespace is the namespace of the pool: base Pods and base
	// Snapshots live there. It is a Sandbox namespace that no tenant uses.
	Namespace     string
	LauncherImage string
	DiskRepo      string
	DiskKeys      []string
	DiskBuilder   DiskBuilderConfig
	// Metrics records the Ready bases of each class. Nil records nothing.
	Metrics *metrics.Collectors
	// Now is the clock. Nil uses time.Now.
	Now func() time.Time
}

func (r *WarmPoolReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// poolActive reports whether a class keeps a warm pool of launcher bases.
func poolActive(cls *setecv1alpha1.SandboxClass) bool {
	return cls != nil && cls.Spec.PreWarmPoolSize > 0 && strings.Contains(cls.Spec.PreWarmImage, "@sha256:") &&
		cls.Spec.DefaultResources != nil && cls.Spec.Runtime != nil &&
		cls.Spec.Runtime.Backend == runtimepkg.BackendLauncher
}

// baseKeyOf is the key of the current base of a pool class.
func baseKeyOf(cls *setecv1alpha1.SandboxClass, launcherImage string) string {
	return snapshot.BaseKey(cls.Spec.PreWarmImage, launcherImage, cls.Spec.CPUTemplate, *cls.Spec.DefaultResources)
}

// Reconcile keeps the pool of one class.
func (r *WarmPoolReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("class", req.Name)
	cls := &setecv1alpha1.SandboxClass{}
	if err := r.Get(ctx, req.NamespacedName, cls); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, r.deleteAll(ctx, req.Name, nil, nil)
		}
		return ctrl.Result{}, err
	}
	bases, pods, err := r.list(ctx, cls.Name)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !poolActive(cls) || r.Namespace == "" || r.Coordinator == nil {
		if err := r.deleteAll(ctx, cls.Name, bases, pods); err != nil {
			return ctrl.Result{}, err
		}
		r.Metrics.SetWarmPool(cls.Name, 0, 0)
		return ctrl.Result{}, r.patchStatus(ctx, cls, nil)
	}
	key := baseKeyOf(cls, r.LauncherImage)
	want := int(cls.Spec.PreWarmPoolSize)
	if lu := cls.Status.WarmPool; lu != nil && lu.LastUsed != nil && r.now().Sub(lu.LastUsed.Time) > warmPoolIdle {
		want = 0
	}

	// A stale or failed base goes, and so does each base above the wanted
	// count.
	var ready []setecv1alpha1.Snapshot
	nodes := map[string]bool{}
	for i := range bases {
		b := &bases[i]
		switch {
		case b.Annotations[snapshot.BaseKeyAnnotation] != key, b.Status.Phase == setecv1alpha1.SnapshotPhaseFailed,
			b.Status.Phase == setecv1alpha1.SnapshotPhaseReady && len(ready) >= want:
			if err := r.Delete(ctx, b); client.IgnoreNotFound(err) != nil {
				return ctrl.Result{}, err
			}
		case b.Status.Phase == setecv1alpha1.SnapshotPhaseReady:
			ready = append(ready, *b)
			nodes[b.Spec.Node] = true
		case !slices.ContainsFunc(pods, func(p corev1.Pod) bool { return p.Name == b.Name }) &&
			r.now().Sub(b.CreationTimestamp.Time) > baseWriteTimeout:
			// The write of this base stopped with its Pod, for example
			// at a restart of the operator. It never becomes Ready.
			if err := r.Delete(ctx, b); client.IgnoreNotFound(err) != nil {
				return ctrl.Result{}, err
			}
		default:
			nodes[b.Spec.Node] = true
		}
	}

	building := 0
	for i := range pods {
		p := &pods[i]
		done, err := r.stepBasePod(ctx, cls, p, key, bases)
		if err != nil {
			logger.Error(err, "base Pod step", "pod", p.Name)
		}
		if !done {
			building++
		}
		// The node of a base Pod holds a base now or soon.
		if p.Spec.NodeName != "" {
			nodes[p.Spec.NodeName] = true
		}
	}

	if len(ready)+building < want {
		if err := r.startBase(ctx, cls, key, nodes); err != nil {
			logger.Error(err, "start a base")
		}
	}
	// A base Pod of this pass may have made a base, so the count is read
	// again.
	n, err := r.readyBases(ctx, cls.Name, key)
	if err != nil {
		return ctrl.Result{}, err
	}
	r.Metrics.SetWarmPool(cls.Name, n, want)
	if err := r.patchStatus(ctx, cls, &setecv1alpha1.SandboxClassWarmPoolStatus{Ready: int32(n), Key: key}); err != nil { //nolint:gosec // a count of a small pool
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: warmPoolRequeue}, nil
}

// stepBasePod moves one base Pod on. A Ready Pod (its guest agent answers)
// gets its base snapshot, and then the Pod goes. A Pod that ended or that
// has an old key goes. It reports whether the Pod is done.
func (r *WarmPoolReconciler) stepBasePod(
	ctx context.Context, cls *setecv1alpha1.SandboxClass, p *corev1.Pod, key string, bases []setecv1alpha1.Snapshot,
) (bool, error) {
	ended := p.Status.Phase == corev1.PodFailed || p.Status.Phase == corev1.PodSucceeded
	if ended || p.Annotations[snapshot.BaseKeyAnnotation] != key || !p.DeletionTimestamp.IsZero() {
		return true, client.IgnoreNotFound(r.Delete(ctx, p))
	}
	if !isPodReady(p) {
		return false, nil
	}
	if slices.ContainsFunc(bases, func(b setecv1alpha1.Snapshot) bool { return b.Name == p.Name }) {
		// The snapshot exists or is in flight: the Pod is done.
		return true, client.IgnoreNotFound(r.Delete(ctx, p))
	}
	err := r.Coordinator.CreateBase(ctx, p, cls, cls.Spec.PreWarmImage, key)
	if derr := r.Delete(ctx, p); client.IgnoreNotFound(derr) != nil && err == nil {
		err = derr
	}
	return true, err
}

// startBase creates one base Pod on a node that holds no base of the
// class yet. The disk of the image comes first.
func (r *WarmPoolReconciler) startBase(ctx context.Context, cls *setecv1alpha1.SandboxClass, key string, nodes map[string]bool) error {
	done, err := ensureDisk(ctx, r.Client, r.DiskBuilder, r.DiskRepo, cls.Spec.PreWarmImage)
	if err != nil || !done {
		return err
	}
	suffix := make([]byte, 3)
	_, _ = rand.Read(suffix)
	name := fmt.Sprintf("base-%s-%s-%s", cls.Name, key[:8], hex.EncodeToString(suffix))
	if len(name) > 63 {
		name = fmt.Sprintf("base-%s-%s", key[:12], hex.EncodeToString(suffix))
	}
	pod, err := podspec.BuildLauncherBase(name, r.Namespace, cls.Spec.PreWarmImage, *cls.Spec.DefaultResources, podspec.LauncherOptions{
		Image: r.LauncherImage, DiskRepo: r.DiskRepo, DiskKeys: r.DiskKeys, CPUTemplate: cls.Spec.CPUTemplate,
	})
	if err != nil {
		return err
	}
	pod.Labels[snapshot.BaseClassLabel] = cls.Name
	pod.Annotations = map[string]string{snapshot.BaseKeyAnnotation: key}
	if len(nodes) > 0 {
		term := &pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0]
		used := make([]string, 0, len(nodes))
		for n := range nodes {
			used = append(used, n)
		}
		slices.Sort(used)
		term.MatchFields = append(term.MatchFields, corev1.NodeSelectorRequirement{
			Key: "metadata.name", Operator: corev1.NodeSelectorOpNotIn, Values: used,
		})
	}
	pod.Spec.NodeSelector = cls.Spec.NodeSelector
	pod.Spec.Tolerations = cls.Spec.Tolerations
	return client.IgnoreAlreadyExists(r.Create(ctx, pod))
}

// list returns the bases and the base Pods of a class.
func (r *WarmPoolReconciler) list(ctx context.Context, class string) ([]setecv1alpha1.Snapshot, []corev1.Pod, error) {
	sel := client.MatchingLabels{snapshot.BaseLabel: snapshot.BaseLabelValue, snapshot.BaseClassLabel: class}
	bases := &setecv1alpha1.SnapshotList{}
	if err := r.List(ctx, bases, client.InNamespace(r.Namespace), sel); err != nil {
		return nil, nil, err
	}
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, client.InNamespace(r.Namespace), sel); err != nil {
		return nil, nil, err
	}
	return bases.Items, pods.Items, nil
}

// readyBases counts the Ready bases of a class with key.
func (r *WarmPoolReconciler) readyBases(ctx context.Context, class, key string) (int, error) {
	bases, _, err := r.list(ctx, class)
	if err != nil {
		return 0, err
	}
	n := 0
	for i := range bases {
		if bases[i].Status.Phase == setecv1alpha1.SnapshotPhaseReady && bases[i].Annotations[snapshot.BaseKeyAnnotation] == key {
			n++
		}
	}
	return n, nil
}

// deleteAll removes the pool of a class.
func (r *WarmPoolReconciler) deleteAll(ctx context.Context, class string, bases []setecv1alpha1.Snapshot, pods []corev1.Pod) error {
	if bases == nil && pods == nil && r.Namespace != "" {
		var err error
		if bases, pods, err = r.list(ctx, class); err != nil {
			return err
		}
	}
	for i := range bases {
		if err := r.Delete(ctx, &bases[i]); client.IgnoreNotFound(err) != nil {
			return err
		}
	}
	for i := range pods {
		if err := r.Delete(ctx, &pods[i]); client.IgnoreNotFound(err) != nil {
			return err
		}
	}
	return nil
}

// patchStatus writes the pool status of a class and keeps LastUsed.
func (r *WarmPoolReconciler) patchStatus(ctx context.Context, cls *setecv1alpha1.SandboxClass, ws *setecv1alpha1.SandboxClassWarmPoolStatus) error {
	if ws != nil && cls.Status.WarmPool != nil {
		ws.LastUsed = cls.Status.WarmPool.LastUsed
	}
	if equality.Semantic.DeepEqual(cls.Status.WarmPool, ws) {
		return nil
	}
	orig := cls.DeepCopy()
	cls.Status.WarmPool = ws
	return r.Status().Patch(ctx, cls, client.MergeFrom(orig))
}

// SetupWithManager registers the reconciler. A change of a base Pod or a
// base Snapshot wakes the pool of its class.
func (r *WarmPoolReconciler) SetupWithManager(mgr ctrl.Manager) error {
	toClass := handler.EnqueueRequestsFromMapFunc(func(_ context.Context, obj client.Object) []reconcile.Request {
		if obj.GetNamespace() != r.Namespace || obj.GetLabels()[snapshot.BaseLabel] != snapshot.BaseLabelValue {
			return nil
		}
		return []reconcile.Request{{Name: obj.GetLabels()[snapshot.BaseClassLabel]}}
	})
	return ctrl.NewControllerManagedBy(mgr).
		Named("warmpool").
		For(&setecv1alpha1.SandboxClass{}).
		Watches(&corev1.Pod{}, toClass).
		Watches(&setecv1alpha1.Snapshot{}, toClass).
		Complete(r)
}

// --- the Sandbox side ---------------------------------------------------

// selectBase returns a Ready, clean base with the current key for a
// Sandbox that can warm start, or nil. A Sandbox can when its class keeps
// a pool, it asks for the pool image with the default size of the class,
// and it names no snapshot.
func (r *SandboxReconciler) selectBase(
	ctx context.Context, sb *setecv1alpha1.Sandbox, cls *setecv1alpha1.SandboxClass,
) (*setecv1alpha1.Snapshot, error) {
	// A session needs its workspace device, which a base never had, so a
	// session always boots or loads its own checkpoint.
	if !poolActive(cls) || r.WarmPoolNamespace == "" || (sb.Spec.SnapshotRef != nil && sb.Spec.SnapshotRef.Name != "") ||
		sb.Spec.IsSession() ||
		sb.Spec.Image != cls.Spec.PreWarmImage || !sameResources(sb.Spec.Resources, *cls.Spec.DefaultResources) {
		return nil, nil
	}
	key := baseKeyOf(cls, r.LauncherImage)
	bases := &setecv1alpha1.SnapshotList{}
	if err := r.List(ctx, bases, client.InNamespace(r.WarmPoolNamespace),
		client.MatchingLabels{snapshot.BaseLabel: snapshot.BaseLabelValue, snapshot.BaseClassLabel: cls.Name}); err != nil {
		return nil, err
	}
	slices.SortFunc(bases.Items, func(a, b setecv1alpha1.Snapshot) int { return strings.Compare(a.Name, b.Name) })
	for i := range bases.Items {
		b := &bases.Items[i]
		if b.Status.Phase == setecv1alpha1.SnapshotPhaseReady && b.Annotations[snapshot.BaseKeyAnnotation] == key &&
			b.Annotations[snapshot.CleanBaseAnnotation] == snapshot.BaseLabelValue && b.DeletionTimestamp.IsZero() {
			return b, nil
		}
	}
	return nil, nil
}

func sameResources(a, b setecv1alpha1.Resources) bool {
	return a.VCPU == b.VCPU && a.Memory.Cmp(b.Memory) == 0
}

// markPoolUsed stamps LastUsed on the pool status of a class when a
// Sandbox asks for the pool image, at most once an hour.
func (r *SandboxReconciler) markPoolUsed(ctx context.Context, sb *setecv1alpha1.Sandbox, cls *setecv1alpha1.SandboxClass) error {
	if !poolActive(cls) || sb.Spec.Image != cls.Spec.PreWarmImage {
		return nil
	}
	now := time.Now()
	if ws := cls.Status.WarmPool; ws != nil && ws.LastUsed != nil && now.Sub(ws.LastUsed.Time) < lastUsedStep {
		return nil
	}
	orig := cls.DeepCopy()
	if cls.Status.WarmPool == nil {
		cls.Status.WarmPool = &setecv1alpha1.SandboxClassWarmPoolStatus{}
	}
	t := metav1.NewTime(now)
	cls.Status.WarmPool.LastUsed = &t
	return r.Status().Patch(ctx, cls, client.MergeFrom(orig))
}

// isPodReady reports whether the Ready condition of p is True: for a
// launcher Pod, the guest agent answers.
func isPodReady(p *corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// countWarmStart records the outcome of a launcher warm start: "restored",
// "miss" (no Ready base, so a boot) or "error" (the load failed).
func (r *SandboxReconciler) countWarmStart(cls *setecv1alpha1.SandboxClass, outcome string) {
	if cls != nil {
		r.MetricsCollector.IncWarmStart(outcome, cls.Name)
	}
}

// recordColdBoot records in status.warmStart that a Sandbox of a pool class
// found no Ready base and boots cold. A failed write is not fatal: the
// Sandbox boots either way, and the counter already holds the miss.
func (r *SandboxReconciler) recordColdBoot(ctx context.Context, sb *setecv1alpha1.Sandbox) {
	if sb.Status.WarmStart != nil {
		return
	}
	original := sb.DeepCopy()
	sb.Status.WarmStart = &setecv1alpha1.SandboxWarmStartStatus{
		Outcome: setecv1alpha1.SandboxWarmStartColdBoot, Reason: "miss",
	}
	if err := r.Status().Patch(ctx, sb, client.MergeFrom(original)); err != nil {
		log.FromContext(ctx).Info("record the cold boot of a pool miss", "error", err.Error())
	}
}
