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
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	setecv1grpc "github.com/zeroroot-ai/setec/api/grpc/v1"
	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
	"github.com/zeroroot-ai/setec/internal/frontend"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// tickerCommand runs an in-guest process whose liveness is observable
// from outside: it prints a strictly increasing TICK counter once a
// second. If the process (or its VM) ever restarts, the counter resets.
var tickerCommand = []string{"/bin/sh", "-c",
	"i=0; while true; do i=$((i+1)); echo \"TICK $i\"; sleep 1; done"}

// tickPattern extracts the counter values from the workload log.
var tickPattern = regexp.MustCompile(`TICK (\d+)`)

// highestTick returns the largest TICK value in the Pod's logs so far.
func highestTick(t *testing.T, podName string) int {
	t.Helper()
	top := 0
	for _, m := range tickPattern.FindAllStringSubmatch(podLogs(t, podName), -1) {
		if v, err := strconv.Atoi(m[1]); err == nil && v > top {
			top = v
		}
	}
	return top
}

// newInProcessFrontend builds a frontend.Service against the live
// cluster, the way the deployed frontend binary wires one. Each call
// returns a completely fresh instance sharing no state with any
// earlier one — the frontend is stateless by design (setec#193), so a
// new instance IS a frontend restart as far as session resolution is
// concerned.
func newInProcessFrontend() *frontend.Service {
	return &frontend.Service{
		Client:           k8sClient,
		AuthDisabled:     true,
		DefaultNamespace: testNamespace,
	}
}

// TestSession_ReattachByHandle exercises the ADR-0006 reattach contract
// end to end against a real microVM (setec#193):
//
//  1. attach to a Running session by handle;
//  2. disconnect (drop the frontend instance entirely);
//  3. reattach through a brand-new frontend instance — a frontend
//     restart — using only the handle;
//  4. verify the in-guest process kept running throughout: same Pod
//     (same VM), and the guest's TICK counter kept climbing without a
//     reset across the disconnect window;
//  5. typed failures: an ephemeral Sandbox refuses Attach, an unknown
//     handle is NotFound, and an ended session refuses reattach.
func TestSession_ReattachByHandle(t *testing.T) {
	// Pin to the suite-owned class: it carries the sandbox-host toleration
	// without which this Sandbox never leaves Pending on staging (setec#330
	// for the suite-wide case; see sandboxHostTolerations).
	installSessionClass(t)

	spec := minimalSpec(tickerCommand...)
	spec.SandboxClassName = sessionClassName()
	size := resource.MustParse("1Gi")
	spec.Lifecycle = &setecv1alpha1.Lifecycle{
		Mode:      setecv1alpha1.LifecycleModeSession,
		Workspace: &setecv1alpha1.WorkspaceSpec{Size: &size},
	}
	sb := newSandbox("e2e-reattach", spec)
	createAndCleanup(t, sb)

	key := client.ObjectKeyFromObject(sb)
	podKey := types.NamespacedName{Namespace: testNamespace, Name: sb.Name + "-vm"}
	waitForPhase(t, key, defaultWait, setecv1alpha1.SandboxPhaseRunning)

	var current setecv1alpha1.Sandbox
	if err := k8sClient.Get(context.Background(), key, &current); err != nil {
		t.Fatalf("get session sandbox: %v", err)
	}
	handle := fmt.Sprintf("%s/%s/%s", current.Namespace, current.Name, current.UID)

	var podBefore corev1.Pod
	if err := k8sClient.Get(context.Background(), podKey, &podBefore); err != nil {
		t.Fatalf("get session pod: %v", err)
	}

	// (1) First attach. The frontend instance lives only for this call:
	// after it, the "connection" is gone and no state survives outside
	// the cluster.
	resp, err := newInProcessFrontend().Attach(context.Background(), &setecv1grpc.AttachRequest{SandboxId: handle})
	if err != nil {
		dumpDiagnostics(t, key)
		t.Fatalf("first Attach: %v", err)
	}
	if resp.GetPhase() != string(setecv1alpha1.SandboxPhaseRunning) {
		t.Fatalf("first Attach phase = %q, want Running", resp.GetPhase())
	}

	// Attach must have registered activity on the CR.
	if err := k8sClient.Get(context.Background(), key, &current); err != nil {
		t.Fatalf("re-get session sandbox: %v", err)
	}
	if _, ok := current.Annotations[setecv1alpha1.AnnotationLastActivity]; !ok {
		t.Fatalf("Attach did not stamp the %s annotation", setecv1alpha1.AnnotationLastActivity)
	}

	// Observe the guest's counter, then (2) sit disconnected for a
	// while: no frontend instance exists and the guest keeps ticking.
	tickAtDisconnect := highestTick(t, podKey.Name)
	time.Sleep(5 * time.Second)

	// (3) Reattach through a brand-new frontend instance, handle only.
	resp2, err := newInProcessFrontend().Attach(context.Background(),
		&setecv1grpc.AttachRequest{SandboxId: handle})
	if err != nil {
		dumpDiagnostics(t, key)
		t.Fatalf("reattach after frontend restart: %v", err)
	}
	if resp2.GetSandboxId() != handle {
		t.Fatalf("reattach resolved %q, want %q", resp2.GetSandboxId(), handle)
	}

	// (4) Continuity: same Pod (same VM), and the counter climbed past
	// its pre-disconnect value without restarting from 1.
	var podAfter corev1.Pod
	if err := k8sClient.Get(context.Background(), podKey, &podAfter); err != nil {
		t.Fatalf("get session pod after reattach: %v", err)
	}
	if podAfter.UID != podBefore.UID {
		t.Fatalf("session Pod changed identity across the disconnect (%s -> %s); the VM restarted",
			podBefore.UID, podAfter.UID)
	}
	deadline := time.Now().Add(briefWait)
	for {
		if tick := highestTick(t, podKey.Name); tick > tickAtDisconnect {
			break
		}
		if time.Now().After(deadline) {
			dumpDiagnostics(t, key)
			t.Fatalf("guest process did not keep running: TICK stuck at %d", tickAtDisconnect)
		}
		time.Sleep(defaultPoll)
	}

	// (5a) Unknown handle → NotFound.
	if _, err := newInProcessFrontend().Attach(context.Background(),
		&setecv1grpc.AttachRequest{SandboxId: testNamespace + "/never-was/uid-x"}); status.Code(err) != codes.NotFound {
		t.Fatalf("attach to unknown handle: code = %v, want NotFound", status.Code(err))
	}

	// (5b) Ephemeral Sandbox refuses Attach.
	ephSpec := minimalSpec("/bin/sh", "-c", "sleep 300")
	// Same class, same reason: this one has to reach Running before the
	// Attach refusal can be asserted, so it has to be schedulable too.
	ephSpec.SandboxClassName = sessionClassName()
	eph := newSandbox("e2e-reattach-eph", ephSpec)
	createAndCleanup(t, eph)
	waitForPhase(t, client.ObjectKeyFromObject(eph), defaultWait, setecv1alpha1.SandboxPhaseRunning)
	var ephCurrent setecv1alpha1.Sandbox
	if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(eph), &ephCurrent); err != nil {
		t.Fatalf("get ephemeral sandbox: %v", err)
	}
	ephHandle := fmt.Sprintf("%s/%s/%s", ephCurrent.Namespace, ephCurrent.Name, ephCurrent.UID)
	_, err = newInProcessFrontend().Attach(context.Background(),
		&setecv1grpc.AttachRequest{SandboxId: ephHandle})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("attach to ephemeral: code = %v, want FailedPrecondition", status.Code(err))
	}
	if !strings.Contains(err.Error(), "ephemeral") {
		t.Fatalf("attach to ephemeral: error %q does not name the lifecycle", err)
	}

	// (5c) Ended session refuses reattach: tear the session down, then
	// attach with the old handle. Deletion may still be in flight
	// (SESSION_ENDED, FailedPrecondition) or complete (NotFound); both
	// are typed refusals, and success is the only wrong answer.
	if err := k8sClient.Delete(context.Background(), &current); err != nil {
		t.Fatalf("delete session sandbox: %v", err)
	}
	deadline = time.Now().Add(briefWait)
	for {
		_, err = newInProcessFrontend().Attach(context.Background(),
			&setecv1grpc.AttachRequest{SandboxId: handle})
		if code := status.Code(err); code == codes.FailedPrecondition || code == codes.NotFound {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("attach to ended session: err = %v, want FailedPrecondition or NotFound", err)
		}
		time.Sleep(defaultPoll)
	}
}
