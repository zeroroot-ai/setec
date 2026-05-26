//go:build e2e

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

// runtime_backends_runc_test.go contains the runc (container-only isolation)
// smoke test. It is intentionally gated behind two conditions:
//
//  1. The environment variable SETEC_E2E_ALLOW_RUNC=1 must be set. Without it,
//     the test is unconditionally skipped so that the default E2E run (targeting
//     secure isolation) does not exercise runc, which provides only container-level
//     isolation and should never be used in production.
//
//  2. The namespace "setec-runc-dev" must exist and carry the label
//     setec.zeroroot.ai/allow-dev-runtimes=true. The admission webhook rejects
//     runc SandboxClass objects for namespaces (or, for cluster-scoped
//     SandboxClasses, the "default" namespace) that lack this label. The test
//     creates the SandboxClass in the "setec-runc-dev" namespace convention.
//
// To run this test:
//
//	kubectl create ns setec-runc-dev
//	kubectl label ns setec-runc-dev setec.zeroroot.ai/allow-dev-runtimes=true
//	SETEC_E2E_ALLOW_RUNC=1 go test -tags=e2e ./test/e2e -run TestRunc

package e2e

import (
	"context"
	"os"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
)

const (
	// runcDevNamespace is the namespace that must exist and carry the
	// allow-dev-runtimes label for the runc test to run.
	runcDevNamespace = "setec-runc-dev"

	// allowDevRuntimesLabel is the key the SandboxClassWebhook checks to
	// permit runc SandboxClass objects. It must be "true" on devGateNamespace
	// ("default") for the admission to pass — and on runcDevNamespace here for
	// documentation clarity.
	allowDevRuntimesLabel = "setec.zeroroot.ai/allow-dev-runtimes"

	// isolationContainerOnlyLabel is the pod label MutatePod adds for runc
	// pods (see internal/runtime/runc.go). Its value is "container-only".
	isolationContainerOnlyLabel = "setec.zeroroot.ai/isolation"
	isolationContainerOnlyValue = "container-only"
)

// TestRunc_ContainerOnly verifies the runc backend end-to-end:
//
//  1. A SandboxClass with backend=runc is created (cluster-scoped; the webhook
//     checks the "default" namespace for the gate label, not runcDevNamespace —
//     runcDevNamespace is where the Sandbox lives).
//  2. A Sandbox is created in the runcDevNamespace.
//  3. The Pod becomes Ready.
//  4. The Pod carries the label setec.zeroroot.ai/isolation=container-only.
func TestRunc_ContainerOnly(t *testing.T) {
	// Hard gate: skip unless explicitly opted in.
	if os.Getenv("SETEC_E2E_ALLOW_RUNC") != "1" {
		t.Skip("runc E2E skipped: set SETEC_E2E_ALLOW_RUNC=1 to enable (runc provides container-only isolation)")
	}

	ctx := context.Background()

	// Verify runcDevNamespace exists and carries the gate label.
	var ns corev1.Namespace
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: runcDevNamespace}, &ns); err != nil {
		if apierrors.IsNotFound(err) {
			t.Skipf("namespace %q not found; create it and apply label %s=true before running runc E2E",
				runcDevNamespace, allowDevRuntimesLabel)
		}
		t.Fatalf("get namespace %q: %v", runcDevNamespace, err)
	}
	if ns.Labels[allowDevRuntimesLabel] != "true" {
		t.Skipf("namespace %q does not have label %s=true; apply it before running runc E2E",
			runcDevNamespace, allowDevRuntimesLabel)
	}

	// Also verify the "default" namespace has the gate label because the
	// SandboxClassWebhook checks that namespace, not runcDevNamespace, for
	// cluster-wide dev-runtime consent.
	var defaultNS corev1.Namespace
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: "default"}, &defaultNS); err != nil {
		t.Fatalf("get default namespace: %v", err)
	}
	if defaultNS.Labels[allowDevRuntimesLabel] != "true" {
		t.Skipf("'default' namespace does not have label %s=true; the admission webhook will reject the runc SandboxClass",
			allowDevRuntimesLabel)
	}

	// Create the SandboxClass with backend=runc (cluster-scoped).
	cls := &setecv1alpha1.SandboxClass{
		ObjectMeta: metav1.ObjectMeta{Name: "runc-dev-cls"},
		Spec: setecv1alpha1.SandboxClassSpec{
			// VMM is set to satisfy the +required marker on the field; the
			// webhook will see Runtime.Backend and use that instead of VMM.
			VMM: setecv1alpha1.VMMFirecracker,
			Runtime: &setecv1alpha1.SandboxClassRuntime{
				Backend: "runc",
			},
		},
	}
	if err := k8sClient.Create(ctx, cls); err != nil {
		t.Fatalf("create runc SandboxClass: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), cls) })

	// Create the Sandbox in the dev namespace.
	sb := &setecv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "runc-dev-sb",
			Namespace: runcDevNamespace,
		},
		Spec: setecv1alpha1.SandboxSpec{
			SandboxClassName: "runc-dev-cls",
			Image:            "busybox:1.36",
			Command:          []string{"sleep", "5"},
			Resources: setecv1alpha1.Resources{
				VCPU:   1,
				Memory: resource.MustParse("64Mi"),
			},
		},
	}
	if err := k8sClient.Create(ctx, sb); err != nil {
		t.Fatalf("create runc Sandbox: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), sb) })

	// Wait for the backing Pod to be Ready.
	podName := "runc-dev-sb-vm"
	if err := waitForPodReady(ctx, k8sClient, runcDevNamespace, podName, backendSmokeDuration); err != nil {
		dumpDiagnostics(t, client.ObjectKey{Namespace: runcDevNamespace, Name: sb.Name})
		t.Fatalf("runc Pod %s not Ready within %s: %v", podName, backendSmokeDuration, err)
	}

	// Assert the isolation label is present on the Pod.
	var pod corev1.Pod
	if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: runcDevNamespace, Name: podName}, &pod); err != nil {
		t.Fatalf("get runc Pod: %v", err)
	}
	gotIsolation := pod.Labels[isolationContainerOnlyLabel]
	if gotIsolation != isolationContainerOnlyValue {
		t.Errorf("pod label %s = %q; want %q (RuncDispatcher.MutatePod should set this)",
			isolationContainerOnlyLabel, gotIsolation, isolationContainerOnlyValue)
	}

	// Confirm status.runtime.chosen=runc.
	runcTimeout := 30 * time.Second
	chosen, err := waitForSandboxRuntimeChosen(ctx, k8sClient, runcDevNamespace, "runc-dev-sb", runcTimeout)
	if err != nil {
		t.Fatalf("status.runtime.chosen not set within %s: %v", runcTimeout, err)
	}
	if chosen != "runc" {
		t.Errorf("status.runtime.chosen = %q; want runc", chosen)
	}
}
