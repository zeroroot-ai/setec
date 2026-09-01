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

package e2e

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
)

// sessionProbeCommand records whether the durable workspace already
// carries the marker from a previous VM incarnation, then keeps the VM
// alive. The first boot prints MARKER-CREATED; every boot after a VM
// restart against the same workspace prints MARKER-FOUND.
var sessionProbeCommand = []string{"/bin/sh", "-c",
	"if [ -f /workspace/marker ]; then echo MARKER-FOUND; else echo MARKER-CREATED; echo alive > /workspace/marker; fi; sleep 3600"}

// sessionSpec returns a session-lifecycle SandboxSpec with a small
// workspace on the cluster-default StorageClass.
func sessionSpec() setecv1alpha1.SandboxSpec {
	spec := minimalSpec(sessionProbeCommand...)
	spec.SandboxClassName = sessionClassName()
	size := resource.MustParse("1Gi")
	spec.Lifecycle = &setecv1alpha1.Lifecycle{
		Mode:      setecv1alpha1.LifecycleModeSession,
		Workspace: &setecv1alpha1.WorkspaceSpec{Size: &size},
	}
	return spec
}

// sessionClassName is the suite-owned SandboxClass the session-lifecycle
// scenarios pin themselves to.
//
// SandboxClass is CLUSTER-scoped, so the name carries the suite's namespace:
// a fixed name would make two concurrent e2e runs on one cluster fight over a
// single object and delete each other's fixture. Same reasoning, and same
// shape, as checkpointClassName().
func sessionClassName() string { return "e2e-session-" + testNamespace }

// installSessionClass creates that class and removes it with the test.
//
// It exists because the scenarios cannot use the cluster default. The
// throwaway release installs with sandboxClasses.enabled=false (the chart's
// cluster-scoped `tool`/`connector` classes cannot be imported into a second
// release), so an unpinned Sandbox resolves the LIVE Argo-managed `tool`
// class — which carries no tolerations and which this suite has no business
// editing.
func installSessionClass(t *testing.T) {
	t.Helper()
	cls := newSandboxClass(sessionClassName(), setecv1alpha1.SandboxClassSpec{
		Runtime: &setecv1alpha1.SandboxClassRuntime{Backend: kataRuntimeClass},
	})
	if err := k8sClient.Create(context.Background(), cls); err != nil {
		t.Fatalf("create session SandboxClass %q: %v", cls.Name, err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), cls)
	})
}

// podLogs returns the workload container logs of the named Pod via
// kubectl, consistent with the harness's other cluster inspection.
func podLogs(t *testing.T, podName string) string {
	t.Helper()
	out, err := exec.Command("kubectl", "logs", podName, "-n", testNamespace, "-c", "workload").CombinedOutput()
	if err != nil {
		t.Logf("kubectl logs %s: %v (output: %s)", podName, err, out)
	}
	return string(out)
}

// waitForLogMarker polls the Pod's logs until `marker` appears.
func waitForLogMarker(t *testing.T, podName, marker string, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if strings.Contains(podLogs(t, podName), marker) {
			return true
		}
		time.Sleep(defaultPoll)
	}
	return false
}

// TestSession_WorkspaceSurvivesPodKill exercises the session-lifecycle
// durability contract end to end (ADR-0006/0007, setec#192):
//
//  1. a session Sandbox writes a marker into its /workspace PVC;
//  2. the backing Pod is killed (VM destroyed);
//  3. the controller recreates the Pod, the fresh microVM re-mounts the
//     workspace, and the workload finds the marker — data survived;
//  4. deleting the Sandbox (explicit teardown) deletes the workspace
//     PVC, so nothing is reusable across sessions (ADR-0005 inv. 3).
func TestSession_WorkspaceSurvivesPodKill(t *testing.T) {
	installSessionClass(t)
	sb := newSandbox("e2e-session", sessionSpec())
	createAndCleanup(t, sb)

	key := client.ObjectKeyFromObject(sb)
	podKey := types.NamespacedName{Namespace: testNamespace, Name: sb.Name + "-vm"}

	// (1) First incarnation boots and creates the marker.
	waitForPhase(t, key, defaultWait, setecv1alpha1.SandboxPhaseRunning)
	var firstPod corev1.Pod
	if err := k8sClient.Get(context.Background(), podKey, &firstPod); err != nil {
		t.Fatalf("get session pod: %v", err)
	}
	if !waitForLogMarker(t, firstPod.Name, "MARKER-CREATED", briefWait) {
		dumpDiagnostics(t, key)
		t.Fatalf("first session VM did not report MARKER-CREATED")
	}

	// (2) Kill the Pod out from under the session.
	if err := k8sClient.Delete(context.Background(), &firstPod); err != nil {
		t.Fatalf("delete session pod: %v", err)
	}

	// (3) A fresh Pod must appear (new UID) and find the marker on the
	// re-attached workspace.
	deadline := time.Now().Add(defaultWait)
	for {
		var pod corev1.Pod
		err := k8sClient.Get(context.Background(), podKey, &pod)
		if err == nil && pod.UID != firstPod.UID && pod.DeletionTimestamp == nil {
			break
		}
		if time.Now().After(deadline) {
			dumpDiagnostics(t, key)
			t.Fatalf("session Pod was not recreated after kill (err=%v)", err)
		}
		time.Sleep(defaultPoll)
	}
	waitForPhase(t, key, defaultWait, setecv1alpha1.SandboxPhaseRunning)
	if !waitForLogMarker(t, podKey.Name, "MARKER-FOUND", defaultWait) {
		dumpDiagnostics(t, key)
		t.Fatalf("restarted session VM did not find the workspace marker; session data was lost")
	}

	// (4) Explicit teardown deletes the workspace PVC.
	if err := k8sClient.Delete(context.Background(), sb); err != nil {
		t.Fatalf("delete session sandbox: %v", err)
	}
	pvcKey := types.NamespacedName{Namespace: testNamespace, Name: sb.Name + "-workspace"}
	deadline = time.Now().Add(defaultWait)
	for {
		var pvc corev1.PersistentVolumeClaim
		err := k8sClient.Get(context.Background(), pvcKey, &pvc)
		if apierrors.IsNotFound(err) {
			break
		}
		if time.Now().After(deadline) {
			dumpDiagnostics(t, key)
			t.Fatalf("workspace PVC %s still present after session teardown (err=%v)", pvcKey, err)
		}
		time.Sleep(defaultPoll)
	}
}
