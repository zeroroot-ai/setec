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

	"k8s.io/client-go/util/retry"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
	"github.com/zeroroot-ai/setec/internal/errwrap"
	"github.com/zeroroot-ai/setec/internal/metrics"
	"github.com/zeroroot-ai/setec/internal/podspec"
	runtimepkg "github.com/zeroroot-ai/setec/internal/runtime"
	"github.com/zeroroot-ai/setec/internal/snapshot"
)

// The warm pool of a launcher class (setec#103, setec#238). A base is a
// full Snapshot of a launcher machine that booted a pool image of the class
// and ran no workload. A pool image is an image by digest that a Sandbox of
// the class asked for with the default resources, in the last 7 days. The
// pool keeps PreWarmPoolSize Ready bases of each pool image, each on another
// node.
const (
	// warmPoolIdle is how long a pool keeps the bases of an image with no
	// Sandbox that asks for it.
	warmPoolIdle = 7 * 24 * time.Hour
	// maxPoolImages caps the pool images of one class. A class whose
	// Sandboxes ask for more images keeps the most recent ones.
	maxPoolImages = 32
	// warmPoolRequeue is the interval of a pool check.
	warmPoolRequeue = 30 * time.Second
	// lastUsedStep is the smallest step of the LastUsed stamp of an image,
	// so a busy class is not written on each Sandbox.
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
	return cls != nil && cls.Spec.PreWarmPoolSize > 0 &&
		cls.Spec.DefaultResources != nil && cls.Spec.Runtime != nil &&
		cls.Spec.Runtime.Backend == runtimepkg.BackendLauncher
}

// poolEligible reports whether sb can use the pool of its class: it asks
// for an image by digest with the default resources of the class, it names
// no snapshot, and it is not a session. A session needs its workspace
// device, which a base never had.
func poolEligible(sb *setecv1alpha1.Sandbox, cls *setecv1alpha1.SandboxClass) bool {
	return poolActive(cls) && strings.Contains(sb.Spec.Image, "@sha256:") &&
		(sb.Spec.SnapshotRef == nil || sb.Spec.SnapshotRef.Name == "") && !sb.Spec.IsSession() &&
		sameResources(sb.Spec.Resources, *cls.Spec.DefaultResources)
}

// baseKeyOf is the key of the current base of image in a pool class.
func baseKeyOf(cls *setecv1alpha1.SandboxClass, image, launcherImage string) string {
	return snapshot.BaseKey(image, launcherImage, cls.Spec.CPUTemplate, *cls.Spec.DefaultResources)
}

// poolImages returns the pool images of cls at now, with their keys: each
// image of the status that a Sandbox asked for in the last 7 days, and the
// first pool image (spec.preWarmImage), which counts as asked for at the
// creation of the class.
func (r *WarmPoolReconciler) poolImages(cls *setecv1alpha1.SandboxClass, now time.Time) []setecv1alpha1.SandboxClassWarmPoolImage {
	var out []setecv1alpha1.SandboxClassWarmPoolImage
	seen := map[string]bool{}
	add := func(image string, lastUsed metav1.Time) {
		if seen[image] || now.Sub(lastUsed.Time) > warmPoolIdle {
			return
		}
		seen[image] = true
		out = append(out, setecv1alpha1.SandboxClassWarmPoolImage{
			Image: image, LastUsed: lastUsed, Key: baseKeyOf(cls, image, r.LauncherImage),
		})
	}
	if ws := cls.Status.WarmPool; ws != nil {
		for _, im := range ws.Images {
			add(im.Image, im.LastUsed)
		}
	}
	if img := cls.Spec.PreWarmImage; strings.Contains(img, "@sha256:") {
		add(img, cls.CreationTimestamp)
	}
	return out
}

// Reconcile keeps the pool of one class.
func (r *WarmPoolReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("class", req.Name)
	cls := &setecv1alpha1.SandboxClass{}
	if err := r.Get(ctx, req.NamespacedName, cls); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, r.deleteAll(ctx, req.Name, nil, nil)
		}
		return ctrl.Result{}, errwrap.Wrap(err, "client.Reader.Get")
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
	images := r.poolImages(cls, r.now())
	// The signature of each pool image comes before any base of it. The
	// bases of an image that does not verify go, because its key leaves
	// keys. An image whose check runs keeps its bases and starts none.
	states, err := r.checkImages(ctx, cls, images)
	if err != nil {
		return ctrl.Result{}, err
	}
	keys := make(map[string]string, len(images))
	for _, im := range images {
		if states[im.Image] != verifyFailed {
			keys[im.Key] = im.Image
		}
	}
	want := int(cls.Spec.PreWarmPoolSize)

	ready, nodes, err := r.pruneBases(ctx, bases, pods, keys, want)
	if err != nil {
		return ctrl.Result{}, err
	}
	building := r.stepBasePods(ctx, cls, pods, keys, bases, nodes)

	// One new base Pod for each image that is short of bases, each pass.
	for _, im := range images {
		if states[im.Image] == verifyPassed && ready[im.Key]+building[im.Key] < want {
			if err := r.startBase(ctx, cls, im.Image, im.Key, nodes[im.Key]); err != nil {
				logger.Error(err, "start a base", "image", im.Image)
			}
		}
	}
	// A base Pod of this pass may have made a base, so the count is read
	// again.
	counts, err := r.readyBases(ctx, cls.Name)
	if err != nil {
		return ctrl.Result{}, err
	}
	total := 0
	for i := range images {
		images[i].Ready = int32(min(counts[images[i].Key], want)) //nolint:gosec // a count of a small pool
		total += counts[images[i].Key]
	}
	r.Metrics.SetWarmPool(cls.Name, total, want*len(images))
	if err := r.patchStatus(ctx, cls, images); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: warmPoolRequeue}, nil
}

// pruneBases deletes a stale or failed base, and each base of an image
// above the wanted count. keys maps the current key of each pool image to
// the image. It returns the count of Ready bases and the nodes that hold a
// base, for each key.
func (r *WarmPoolReconciler) pruneBases(
	ctx context.Context, bases []setecv1alpha1.Snapshot, pods []corev1.Pod, keys map[string]string, want int,
) (ready map[string]int, nodes map[string]map[string]bool, err error) {
	ready = map[string]int{}
	nodes = map[string]map[string]bool{}
	for i := range bases {
		b := &bases[i]
		k := b.Annotations[snapshot.BaseKeyAnnotation]
		_, current := keys[k]
		switch {
		case !current, b.Status.Phase == setecv1alpha1.SnapshotPhaseFailed,
			b.Status.Phase == setecv1alpha1.SnapshotPhaseReady && ready[k] >= want:
			if err := client.IgnoreNotFound(r.Delete(ctx, b)); err != nil {
				return nil, nil, errwrap.Wrap(err, "client.Writer.Delete")
			}
		case b.Status.Phase == setecv1alpha1.SnapshotPhaseReady:
			ready[k]++
			addNode(nodes, k, b.Spec.Node)
		case !slices.ContainsFunc(pods, func(p corev1.Pod) bool { return p.Name == b.Name }) &&
			r.now().Sub(b.CreationTimestamp.Time) > baseWriteTimeout:
			// The write of this base stopped with its Pod, for example
			// at a restart of the operator. It never becomes Ready.
			if err := client.IgnoreNotFound(r.Delete(ctx, b)); err != nil {
				return nil, nil, errwrap.Wrap(err, "client.Writer.Delete")
			}
		default:
			addNode(nodes, k, b.Spec.Node)
		}
	}
	return ready, nodes, nil
}

// addNode records that node holds or soon holds a base of key.
func addNode(nodes map[string]map[string]bool, key, node string) {
	if node == "" {
		return
	}
	if nodes[key] == nil {
		nodes[key] = map[string]bool{}
	}
	nodes[key][node] = true
}

// stepBasePods moves each base Pod on, and adds its node to nodes. It
// returns the count of Pods that still build a base, for each key.
func (r *WarmPoolReconciler) stepBasePods(
	ctx context.Context, cls *setecv1alpha1.SandboxClass, pods []corev1.Pod, keys map[string]string,
	bases []setecv1alpha1.Snapshot, nodes map[string]map[string]bool,
) map[string]int {
	building := map[string]int{}
	for i := range pods {
		p := &pods[i]
		k := p.Annotations[snapshot.BaseKeyAnnotation]
		done, err := r.stepBasePod(ctx, cls, p, keys, bases)
		if err != nil {
			log.FromContext(ctx).Error(err, "base Pod step", "pod", p.Name)
		}
		if !done {
			building[k]++
		}
		// The node of a base Pod holds a base now or soon.
		addNode(nodes, k, p.Spec.NodeName)
	}
	return building
}

// stepBasePod moves one base Pod on. A Ready Pod (its guest agent answers)
// gets its base snapshot, and then the Pod goes. A Pod that ended or that
// has an old key goes. It reports whether the Pod is done.
func (r *WarmPoolReconciler) stepBasePod(
	ctx context.Context, cls *setecv1alpha1.SandboxClass, p *corev1.Pod, keys map[string]string, bases []setecv1alpha1.Snapshot,
) (bool, error) {
	key := p.Annotations[snapshot.BaseKeyAnnotation]
	image, current := keys[key]
	ended := p.Status.Phase == corev1.PodFailed || p.Status.Phase == corev1.PodSucceeded
	if ended || !current || !p.DeletionTimestamp.IsZero() {
		return true, client.IgnoreNotFound(r.Delete(ctx, p))
	}
	if !isPodReady(p) {
		return false, nil
	}
	if slices.ContainsFunc(bases, func(b setecv1alpha1.Snapshot) bool { return b.Name == p.Name }) {
		// The snapshot exists or is in flight: the Pod is done.
		return true, client.IgnoreNotFound(r.Delete(ctx, p))
	}
	err := r.Coordinator.CreateBase(ctx, p, cls, image, key)
	if derr := r.Delete(ctx, p); client.IgnoreNotFound(derr) != nil && err == nil {
		err = derr
	}
	return true, err
}

// startBase creates one base Pod of image on a node that holds no base of
// the image yet. The disk of the image comes first.
func (r *WarmPoolReconciler) startBase(
	ctx context.Context, cls *setecv1alpha1.SandboxClass, image, key string, nodes map[string]bool,
) error {
	done, err := ensureDisk(ctx, r.Client, r.DiskBuilder, r.DiskRepo, image)
	if err != nil || !done {
		return err
	}
	suffix := make([]byte, 3)
	_, _ = rand.Read(suffix)
	name := fmt.Sprintf("base-%s-%s-%s", cls.Name, key[:8], hex.EncodeToString(suffix))
	if len(name) > 63 {
		name = fmt.Sprintf("base-%s-%s", key[:12], hex.EncodeToString(suffix))
	}
	pod, err := podspec.BuildLauncherBase(name, r.Namespace, image, *cls.Spec.DefaultResources, podspec.LauncherOptions{
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
	return errwrap.Wrap(client.IgnoreAlreadyExists(r.Create(ctx, pod)), "client.IgnoreAlreadyExists")
}

// list returns the bases and the base Pods of a class.
func (r *WarmPoolReconciler) list(ctx context.Context, class string) ([]setecv1alpha1.Snapshot, []corev1.Pod, error) {
	sel := client.MatchingLabels{snapshot.BaseLabel: snapshot.BaseLabelValue, snapshot.BaseClassLabel: class}
	bases := &setecv1alpha1.SnapshotList{}
	if err := r.List(ctx, bases, client.InNamespace(r.Namespace), sel); err != nil {
		return nil, nil, errwrap.Wrap(err, "client.Reader.List")
	}
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, client.InNamespace(r.Namespace), sel); err != nil {
		return nil, nil, errwrap.Wrap(err, "client.Reader.List")
	}
	return bases.Items, pods.Items, nil
}

// readyBases counts the Ready bases of a class, for each key.
func (r *WarmPoolReconciler) readyBases(ctx context.Context, class string) (map[string]int, error) {
	bases, _, err := r.list(ctx, class)
	if err != nil {
		return nil, err
	}
	n := map[string]int{}
	for i := range bases {
		if bases[i].Status.Phase == setecv1alpha1.SnapshotPhaseReady {
			n[bases[i].Annotations[snapshot.BaseKeyAnnotation]]++
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
		if err := client.IgnoreNotFound(r.Delete(ctx, &bases[i])); err != nil {
			return errwrap.Wrap(err, "client.Writer.Delete")
		}
	}
	for i := range pods {
		if err := client.IgnoreNotFound(r.Delete(ctx, &pods[i])); err != nil {
			return errwrap.Wrap(err, "client.Writer.Delete")
		}
	}
	return nil
}

// patchStatus writes the pool images of a class. A Sandbox may have
// stamped an image since the class was read, so it reads the class again
// and keeps each image that the new read holds and that is not idle. A
// conflict with such a stamp retries. A nil images clears the pool.
func (r *WarmPoolReconciler) patchStatus(
	ctx context.Context, cls *setecv1alpha1.SandboxClass, images []setecv1alpha1.SandboxClassWarmPoolImage,
) error {
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		fresh := &setecv1alpha1.SandboxClass{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(cls), fresh); err != nil {
			return errwrap.Wrap(err, "client.Reader.Get")
		}
		var ws *setecv1alpha1.SandboxClassWarmPoolStatus
		if images != nil {
			ws = &setecv1alpha1.SandboxClassWarmPoolStatus{Images: mergePoolImages(fresh.Status.WarmPool, images, r.now())}
		}
		if equality.Semantic.DeepEqual(fresh.Status.WarmPool, ws) {
			return nil
		}
		orig := fresh.DeepCopy()
		fresh.Status.WarmPool = ws
		return errwrap.Wrap(r.Status().Patch(ctx, fresh, client.MergeFromWithOptions(orig, client.MergeFromWithOptimisticLock{})),
			"client.SubResourceWriter.Patch")
	})
	return errwrap.Wrap(err, "retry.RetryOnConflict")
}

// mergePoolImages joins the computed pool images with the images of the
// live status. A live image that is not idle stays, with the later of the
// two stamps, so a Sandbox that asked for it since the read is not lost.
func mergePoolImages(
	live *setecv1alpha1.SandboxClassWarmPoolStatus, computed []setecv1alpha1.SandboxClassWarmPoolImage, now time.Time,
) []setecv1alpha1.SandboxClassWarmPoolImage {
	out := slices.Clone(computed)
	if live != nil {
		for _, l := range live.Images {
			i := slices.IndexFunc(out, func(c setecv1alpha1.SandboxClassWarmPoolImage) bool { return c.Image == l.Image })
			switch {
			case i >= 0 && l.LastUsed.After(out[i].LastUsed.Time):
				out[i].LastUsed = l.LastUsed
			case i < 0 && now.Sub(l.LastUsed.Time) <= warmPoolIdle:
				out = append(out, l)
			}
		}
	}
	slices.SortFunc(out, func(a, b setecv1alpha1.SandboxClassWarmPoolImage) int { return strings.Compare(a.Image, b.Image) })
	return out
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
	return errwrap.Wrap(ctrl.NewControllerManagedBy(mgr).
		Named("warmpool").
		For(&setecv1alpha1.SandboxClass{}).
		Watches(&corev1.Pod{}, toClass).
		Watches(&setecv1alpha1.Snapshot{}, toClass).
		Complete(r), "builder.TypedBuilder.Complete")
}

// --- the Sandbox side ---------------------------------------------------

// selectBase returns a Ready, clean base with the current key of the image
// of sb, or nil. Only a Sandbox that is poolEligible can warm start.
func (r *SandboxReconciler) selectBase(
	ctx context.Context, sb *setecv1alpha1.Sandbox, cls *setecv1alpha1.SandboxClass,
) (*setecv1alpha1.Snapshot, error) {
	if r.WarmPoolNamespace == "" || !poolEligible(sb, cls) {
		return nil, nil
	}
	key := baseKeyOf(cls, sb.Spec.Image, r.LauncherImage)
	bases := &setecv1alpha1.SnapshotList{}
	if err := r.List(ctx, bases, client.InNamespace(r.WarmPoolNamespace),
		client.MatchingLabels{snapshot.BaseLabel: snapshot.BaseLabelValue, snapshot.BaseClassLabel: cls.Name}); err != nil {
		return nil, errwrap.Wrap(err, "client.Reader.List")
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

// markPoolUsed stamps the image of sb in the pool status of its class, at
// most once an hour for each image. A new image joins the pool. A class
// keeps at most maxPoolImages images, the most recent ones.
func (r *SandboxReconciler) markPoolUsed(ctx context.Context, sb *setecv1alpha1.Sandbox, cls *setecv1alpha1.SandboxClass) error {
	if !poolEligible(sb, cls) {
		return nil
	}
	now := time.Now()
	if ws := cls.Status.WarmPool; ws != nil {
		for _, im := range ws.Images {
			if im.Image == sb.Spec.Image && now.Sub(im.LastUsed.Time) < lastUsedStep {
				return nil
			}
		}
	}
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		fresh := &setecv1alpha1.SandboxClass{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(cls), fresh); err != nil {
			return errwrap.Wrap(err, "client.Reader.Get")
		}
		orig := fresh.DeepCopy()
		if fresh.Status.WarmPool == nil {
			fresh.Status.WarmPool = &setecv1alpha1.SandboxClassWarmPoolStatus{}
		}
		fresh.Status.WarmPool.Images = stampImage(fresh.Status.WarmPool.Images, sb.Spec.Image, metav1.NewTime(now))
		return errwrap.Wrap(r.Status().Patch(ctx, fresh, client.MergeFromWithOptions(orig, client.MergeFromWithOptimisticLock{})),
			"client.SubResourceWriter.Patch")
	})
	return errwrap.Wrap(err, "retry.RetryOnConflict")
}

// stampImage sets the LastUsed of image to now, and adds the image when it
// is new. Past maxPoolImages, the image with the oldest stamp goes.
func stampImage(images []setecv1alpha1.SandboxClassWarmPoolImage, image string, now metav1.Time) []setecv1alpha1.SandboxClassWarmPoolImage {
	if i := slices.IndexFunc(images, func(im setecv1alpha1.SandboxClassWarmPoolImage) bool { return im.Image == image }); i >= 0 {
		images[i].LastUsed = now
		return images
	}
	images = append(images, setecv1alpha1.SandboxClassWarmPoolImage{Image: image, LastUsed: now})
	if len(images) > maxPoolImages {
		oldest := 0
		for i := range images {
			if images[i].LastUsed.Before(&images[oldest].LastUsed) {
				oldest = i
			}
		}
		images = slices.Delete(images, oldest, oldest+1)
	}
	slices.SortFunc(images, func(a, b setecv1alpha1.SandboxClassWarmPoolImage) int { return strings.Compare(a.Image, b.Image) })
	return images
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
