/*
Copyright 2026 The Setec Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"fmt"
	"testing"
	"time"

	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	setecgrpcv1 "github.com/zeroroot-ai/setec/api/grpc/v1"
	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
)

// newPreWarmSandboxClass creates a cluster-scoped SandboxClass with an
// active pre-warm pool and registers cleanup.
func newPreWarmSandboxClass(t *testing.T, name, image string) *setecv1alpha1.SandboxClass {
	t.Helper()
	cls := &setecv1alpha1.SandboxClass{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: setecv1alpha1.SandboxClassSpec{
			Runtime:         &setecv1alpha1.SandboxClassRuntime{Backend: "kata-fc"},
			PreWarmPoolSize: 2,
			PreWarmImage:    image,
			PreWarmTTL:      &metav1.Duration{Duration: time.Hour},
		},
	}
	if err := testClient.Create(testCtx, cls); err != nil {
		t.Fatalf("create SandboxClass: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(testCtx, cls) })
	return cls
}

// driveSandboxPodRunning waits for the Sandbox's Pod, binds it to a
// node, and forces PodRunning status (no scheduler/kubelet in envtest).
func driveSandboxPodRunning(t *testing.T, g *gomega.WithT, ns, sbName string) {
	t.Helper()
	g.Eventually(func() bool {
		pod, err := getPod(testCtx, ns, sbName+"-vm")
		return err == nil && pod != nil
	}, 10*time.Second, 250*time.Millisecond).Should(gomega.BeTrue())

	pod, err := getPod(testCtx, ns, sbName+"-vm")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	bindPodToNode(t, pod, "kata-node-1")
	pod, err = getPod(testCtx, ns, sbName+"-vm")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	pod.Status.Phase = corev1.PodRunning
	pod.Status.StartTime = &metav1.Time{Time: time.Now()}
	g.Expect(testClient.Status().Update(testCtx, pod)).To(gomega.Succeed())
}

// TestWarmStart_PoolRestoredStampedOnce drives a pre-warm-eligible
// Sandbox to Running with a node-agent that grants a pool entry, and
// asserts the outcome is stamped exactly once as PoolRestored.
func TestWarmStart_PoolRestoredStampedOnce(t *testing.T) {
	g := gomega.NewWithT(t)
	ns := newNamespace(t, "ws-hit")

	const image = "ghcr.io/org/prewarm:v1"
	clsName := fmt.Sprintf("prewarm-hit-%d", time.Now().UnixNano())
	newPreWarmSandboxClass(t, clsName, image)

	testDialer.client.ClaimRes = &setecgrpcv1.ClaimPoolEntryResponse{
		Claimed: true, Success: true, EntryId: "pool-entry-7", EntropyReseeded: true,
	}
	t.Cleanup(func() { testDialer.client.ClaimRes = nil })

	sb := &setecv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sb", Namespace: ns},
		Spec: setecv1alpha1.SandboxSpec{
			SandboxClassName: clsName,
			Image:            image,
			Command:          []string{"sh"},
			Resources: setecv1alpha1.Resources{
				VCPU:   1,
				Memory: resource.MustParse("512Mi"),
			},
		},
	}
	g.Expect(testClient.Create(testCtx, sb)).To(gomega.Succeed())
	driveSandboxPodRunning(t, g, ns, sb.Name)

	g.Eventually(func() string {
		got, err := getSandbox(testCtx, ns, sb.Name)
		if err != nil || got.Status.WarmStart == nil {
			return ""
		}
		return string(got.Status.WarmStart.Outcome)
	}, 10*time.Second, 250*time.Millisecond).Should(
		gomega.Equal(string(setecv1alpha1.SandboxWarmStartPoolRestored)))

	got, err := getSandbox(testCtx, ns, sb.Name)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(got.Status.WarmStart.EntryID).To(gomega.Equal("pool-entry-7"))
	g.Expect(got.Status.Phase).To(gomega.Equal(setecv1alpha1.SandboxPhaseRunning))
}

// TestWarmStart_MissFallsBackToColdBoot asserts an empty pool never
// fails the Sandbox: it reaches Running with a ColdBoot outcome.
func TestWarmStart_MissFallsBackToColdBoot(t *testing.T) {
	g := gomega.NewWithT(t)
	ns := newNamespace(t, "ws-miss")

	const image = "ghcr.io/org/prewarm:v2"
	clsName := fmt.Sprintf("prewarm-miss-%d", time.Now().UnixNano())
	newPreWarmSandboxClass(t, clsName, image)

	// Default fake: ClaimRes nil → claimed=false (pool miss).
	sb := &setecv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sb", Namespace: ns},
		Spec: setecv1alpha1.SandboxSpec{
			SandboxClassName: clsName,
			Image:            image,
			Command:          []string{"sh"},
			Resources: setecv1alpha1.Resources{
				VCPU:   1,
				Memory: resource.MustParse("512Mi"),
			},
		},
	}
	g.Expect(testClient.Create(testCtx, sb)).To(gomega.Succeed())
	driveSandboxPodRunning(t, g, ns, sb.Name)

	g.Eventually(func() string {
		got, err := getSandbox(testCtx, ns, sb.Name)
		if err != nil || got.Status.WarmStart == nil {
			return ""
		}
		return string(got.Status.WarmStart.Outcome)
	}, 10*time.Second, 250*time.Millisecond).Should(
		gomega.Equal(string(setecv1alpha1.SandboxWarmStartColdBoot)))

	got, err := getSandbox(testCtx, ns, sb.Name)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(got.Status.Phase).To(gomega.Equal(setecv1alpha1.SandboxPhaseRunning),
		"a pool miss must never fail the Sandbox")
	g.Expect(got.Status.WarmStart.Reason).To(gomega.Equal("miss"))
}

// TestWarmStart_ImageMismatchSkipsAttempt pins that a Sandbox running
// a different image than the class's preWarmImage never attempts a
// claim — status.warmStart stays nil.
func TestWarmStart_ImageMismatchSkipsAttempt(t *testing.T) {
	g := gomega.NewWithT(t)
	ns := newNamespace(t, "ws-skip")

	clsName := fmt.Sprintf("prewarm-skip-%d", time.Now().UnixNano())
	newPreWarmSandboxClass(t, clsName, "ghcr.io/org/prewarm:v3")

	sb := &setecv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sb", Namespace: ns},
		Spec: setecv1alpha1.SandboxSpec{
			SandboxClassName: clsName,
			Image:            "ghcr.io/org/other:v9",
			Command:          []string{"sh"},
			Resources: setecv1alpha1.Resources{
				VCPU:   1,
				Memory: resource.MustParse("512Mi"),
			},
		},
	}
	g.Expect(testClient.Create(testCtx, sb)).To(gomega.Succeed())
	driveSandboxPodRunning(t, g, ns, sb.Name)

	g.Eventually(func() setecv1alpha1.SandboxPhase {
		got, err := getSandbox(testCtx, ns, sb.Name)
		if err != nil {
			return ""
		}
		return got.Status.Phase
	}, 10*time.Second, 250*time.Millisecond).Should(gomega.Equal(setecv1alpha1.SandboxPhaseRunning))

	got, err := getSandbox(testCtx, ns, sb.Name)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(got.Status.WarmStart).To(gomega.BeNil(),
		"an image mismatch must skip the warm-start attempt entirely")
}
