// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package controller

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	setecgrpcv1 "github.com/zeroroot-ai/setec/api/grpc/v1"
	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
	"github.com/zeroroot-ai/setec/internal/podspec"
	snapshotpkg "github.com/zeroroot-ai/setec/internal/snapshot"
)

// poolNode is a node that can run a base: it offers the KVM device.
func poolNode(name string) *corev1.Node {
	n := &corev1.Node{}
	n.Name = name
	n.Labels = map[string]string{"kubernetes.io/arch": "amd64"}
	n.Status.Allocatable = corev1.ResourceList{podspec.KVMResource: resource.MustParse("8")}
	return n
}

// firstBase walks the pool of r to its first Ready base on node, and returns
// the name of that base.
func firstBase(t *testing.T, r *WarmPoolReconciler, node string) string {
	t.Helper()
	reconcileClass(t, r)
	finishVerifyJob(t, r, true)
	reconcileClass(t, r)
	completeDiskJob(t, r)
	reconcileClass(t, r)
	pods := basePods(t, r)
	if len(pods) != 1 {
		t.Fatalf("base Pods = %d, want 1", len(pods))
	}
	markBasePodReady(t, r, &pods[0], node)
	reconcileClass(t, r)
	return pods[0].Name
}

func nodeCondition(t *testing.T, r *WarmPoolReconciler) *metav1.Condition {
	t.Helper()
	cls := &setecv1alpha1.SandboxClass{}
	if err := r.Get(context.Background(), types.NamespacedName{Name: "tools"}, cls); err != nil {
		t.Fatal(err)
	}
	return meta.FindStatusCondition(cls.Status.Conditions, setecv1alpha1.ConditionWarmPoolShortOfNodes)
}

func cleanBase() *fakeNodeAgentClient {
	return &fakeNodeAgentClient{CreateResp: &setecgrpcv1.CreateSnapshotResponse{StorageRef: "ref", CleanBaseVerified: true}}
}

// TestWarmPool_SizeOneMakesOneBasePod proves that a pool of size 1 makes no
// second base Pod. The pass that made the base used the count of bases from
// before the base existed, and it started one more Pod that no node could
// take (setec#422).
func TestWarmPool_SizeOneMakesOneBasePod(t *testing.T) {
	t.Parallel()
	cls := poolClass()
	cls.Spec.PreWarmPoolSize = 1
	r := poolReconciler(t, cleanBase(), cls, poolNode("node-a"), poolNode("node-b"))

	firstBase(t, r, "node-a")
	for range 3 {
		if n := len(basePods(t, r)); n != 0 {
			t.Fatalf("base Pods after the one base of the pool = %d, want 0", n)
		}
		reconcileClass(t, r)
	}
	if c := nodeCondition(t, r); c != nil {
		t.Fatalf("a pool with all its bases is not short of nodes: %+v", c)
	}
}

// TestWarmPool_NoBasePodWithNoOtherNode proves that a pool of size 2 on one
// node makes no second base Pod, says so on the class, and makes the Pod
// when a second node joins.
func TestWarmPool_NoBasePodWithNoOtherNode(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	// A node with no KVM device and a cordoned node are no place for a base.
	plain := poolNode("node-plain")
	plain.Status.Allocatable = nil
	cordoned := poolNode("node-cordoned")
	cordoned.Spec.Unschedulable = true
	r := poolReconciler(t, cleanBase(), poolClass(), poolNode("node-a"), plain, cordoned)

	done := firstBase(t, r, "node-a")
	reconcileClass(t, r)
	if n := len(basePods(t, r)); n != 0 {
		t.Fatalf("base Pods with no other node = %d, want 0", n)
	}
	c := nodeCondition(t, r)
	if c == nil || c.Status != metav1.ConditionTrue || c.Reason != reasonNoOtherNode || !strings.Contains(c.Message, poolImage) {
		t.Fatalf("condition = %+v", c)
	}

	if err := r.Create(ctx, poolNode("node-b")); err != nil {
		t.Fatal(err)
	}
	reconcileClass(t, r)
	assertNextBasePods(t, r, done, "node-a")
	if c := nodeCondition(t, r); c == nil || c.Status != metav1.ConditionFalse {
		t.Fatalf("condition after a node joined = %+v", c)
	}
}

// TestWarmPool_DropsABasePodAboveThePoolSize proves that a base Pod with no
// node goes when its key has all its bases. An operator before setec#422
// left such a Pod Pending with no end.
func TestWarmPool_DropsABasePodAboveThePoolSize(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cls := poolClass()
	cls.Spec.PreWarmPoolSize = 1
	r := poolReconciler(t, cleanBase(), cls, poolNode("node-a"))
	firstBase(t, r, "node-a")

	stuck := &corev1.Pod{}
	stuck.Name, stuck.Namespace = "base-tools-stuck", poolNS
	stuck.Labels = map[string]string{snapshotpkg.BaseLabel: "true", snapshotpkg.BaseClassLabel: "tools"}
	stuck.Annotations = map[string]string{snapshotpkg.BaseKeyAnnotation: baseKeyOf(cls, poolImage, poolLaunch)}
	stuck.Spec.Containers = []corev1.Container{{Name: "launcher", Image: poolLaunch}}
	if err := r.Create(ctx, stuck); err != nil {
		t.Fatal(err)
	}
	reconcileClass(t, r)
	if n := len(basePods(t, r)); n != 0 {
		t.Fatalf("base Pods = %d, want 0: the Pod above the pool size stays", n)
	}
}

// TestNodeTakes covers each placement rule that the pool reads.
func TestNodeTakes(t *testing.T) {
	t.Parallel()
	pod := func(mods ...func(*corev1.Pod)) *corev1.Pod {
		p := &corev1.Pod{}
		p.Spec.Affinity = &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
				NodeSelectorTerms: []corev1.NodeSelectorTerm{{
					MatchExpressions: []corev1.NodeSelectorRequirement{{
						Key: "kubernetes.io/arch", Operator: corev1.NodeSelectorOpIn, Values: []string{"amd64"},
					}},
					MatchFields: []corev1.NodeSelectorRequirement{{
						Key: "metadata.name", Operator: corev1.NodeSelectorOpNotIn, Values: []string{"used"},
					}},
				}},
			},
		}}
		for _, m := range mods {
			m(p)
		}
		return p
	}
	node := func(name string, mods ...func(*corev1.Node)) *corev1.Node {
		n := poolNode(name)
		for _, m := range mods {
			m(n)
		}
		return n
	}
	tainted := func(n *corev1.Node) {
		n.Spec.Taints = []corev1.Taint{{Key: "sandbox", Value: "only", Effect: corev1.TaintEffectNoSchedule}}
	}
	tolerates := func(tol corev1.Toleration) func(*corev1.Pod) {
		return func(p *corev1.Pod) { p.Spec.Tolerations = []corev1.Toleration{tol} }
	}
	selects := func(p *corev1.Pod) { p.Spec.NodeSelector = map[string]string{"pool": "sandbox"} }
	tests := []struct {
		name string
		node *corev1.Node
		pod  *corev1.Pod
		want bool
	}{
		{"a free node with the device", node("free"), pod(), true},
		{"the node that holds a base", node("used"), pod(), false},
		{"a cordoned node", node("free", func(n *corev1.Node) { n.Spec.Unschedulable = true }), pod(), false},
		{"a node with no KVM device", node("free", func(n *corev1.Node) { n.Status.Allocatable = nil }), pod(), false},
		{"a node of another architecture", node("free", func(n *corev1.Node) { n.Labels["kubernetes.io/arch"] = "arm64" }), pod(), false},
		{"a node with no label of the node selector", node("free"), pod(selects), false},
		{"a node with the label of the node selector", node("free", func(n *corev1.Node) { n.Labels["pool"] = "sandbox" }), pod(selects), true},
		{"a taint with no toleration", node("free", tainted), pod(), false},
		{"a taint with an equal toleration", node("free", tainted),
			pod(tolerates(corev1.Toleration{Key: "sandbox", Value: "only", Effect: corev1.TaintEffectNoSchedule})), true},
		{"a taint with an exists toleration", node("free", tainted),
			pod(tolerates(corev1.Toleration{Key: "sandbox", Operator: corev1.TolerationOpExists})), true},
		{"a toleration of another effect", node("free", tainted),
			pod(tolerates(corev1.Toleration{Key: "sandbox", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute})), false},
		{"a taint that only prefers no Pod", node("free", func(n *corev1.Node) {
			n.Spec.Taints = []corev1.Taint{{Key: "soft", Effect: corev1.TaintEffectPreferNoSchedule}}
		}), pod(), true},
		{"a Pod with no affinity", node("used"), &corev1.Pod{}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := nodeTakes(tc.node, tc.pod); got != tc.want {
				t.Fatalf("nodeTakes = %v, want %v", got, tc.want)
			}
		})
	}
}
