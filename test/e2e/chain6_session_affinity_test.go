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

// CHAIN 6 EXIT TEST — "two tool calls in one session reach the same microVM
// and see the same worktree."
//
// This is the only thing that closes chain 6. A merged handler does not, and
// neither does a unit test against a fake: the property under test is that a
// SECOND command lands in the SAME live sandbox and can read what the FIRST
// one wrote to /workspace. Only a real sandbox can demonstrate that.
//
// RUNS WITHOUT STAGING OR PROD. The estate is torn down, so this is written to
// pass on a kind cluster with the `runc` backend as well as on metal with
// kata-fc. That is sound rather than a shortcut: Exec is implemented over the
// Kubernetes pods/exec subresource (internal/frontend/exec.go), which is
// backend-agnostic — for kata-fc the kubelet routes through the Kata shim, for
// runc it is an ordinary container exec. The workspace-affinity property being
// asserted is a property of session lifecycle and the PVC, not of the
// isolation backend.
//
// What a runc run does NOT prove is isolation. It is not meant to: that is
// covered by TestRuntimeBackends_* and the invariant gates. Chain 6 asks
// whether a session keeps its worktree, and this answers exactly that.
//
// Select the backend with SETEC_E2E_BACKEND (default kata-fc).
package e2e

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	setecv1grpc "github.com/zeroroot-ai/setec/api/grpc/v1"
	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
	"github.com/zeroroot-ai/setec/internal/frontend"
)

// chain6Backend selects the isolation backend. kata-fc on metal; the kind
// exit-test workflow sets runc, which needs no KVM.
func chain6Backend() string {
	if b := strings.TrimSpace(os.Getenv("SETEC_E2E_BACKEND")); b != "" {
		return b
	}
	return "kata-fc"
}

const (
	chain6ClassName   = "chain6-session-cls"
	chain6SandboxName = "chain6-session"
	// chain6Marker is written by the first command and read by the second.
	// Its whole job is to be a value that cannot appear by accident.
	chain6Marker = "chain6-worktree-is-shared"

	// chain6BreakEnv opts into the failing fixture: it breaks the property on
	// purpose so the workflow can prove this test is able to fail.
	chain6BreakEnv      = "SETEC_E2E_CHAIN6_BREAK"
	chain6BreakRelaunch = "relaunch"
)

// chain6ExecStream is an in-memory SandboxService_ExecServer. It feeds a
// scripted client half and records everything the server sends back, so the
// frontend can be driven in-process exactly as session_reattach_test.go drives
// Attach — no gRPC listener, no mTLS, no deployed frontend.
type chain6ExecStream struct {
	setecv1grpc.SandboxService_ExecServer

	ctx  context.Context
	in   []*setecv1grpc.SandboxServiceExecRequest
	next int
	out  []*setecv1grpc.SandboxServiceExecResponse
}

func (s *chain6ExecStream) Context() context.Context { return s.ctx }

func (s *chain6ExecStream) Send(m *setecv1grpc.SandboxServiceExecResponse) error {
	s.out = append(s.out, m)
	return nil
}

func (s *chain6ExecStream) Recv() (*setecv1grpc.SandboxServiceExecRequest, error) {
	if s.next >= len(s.in) {
		// The client half is done. EOF is the half-close, not a failure.
		return nil, io.EOF
	}
	m := s.in[s.next]
	s.next++
	return m, nil
}

// chain6Frontend builds a frontend.Service that can actually run commands.
// RESTConfig is the part that matters: without it containerExec() returns nil
// and the frontend reports that it cannot exec rather than pretending to.
func chain6Frontend() *frontend.Service {
	return &frontend.Service{
		Client:           k8sClient,
		RESTConfig:       restConfig,
		AuthDisabled:     true,
		DefaultNamespace: testNamespace,
	}
}

// chain6ExecResult is the flattened outcome of one Exec.
type chain6ExecResult struct {
	stdout string
	stderr string
	exit   *setecv1grpc.SessionExecExit
}

// chain6Exec runs argv in the session named by handle and returns everything
// the stream produced.
func chain6Exec(t *testing.T, handle string, argv ...string) chain6ExecResult {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	stream := &chain6ExecStream{
		ctx: ctx,
		in: []*setecv1grpc.SandboxServiceExecRequest{
			{Request: &setecv1grpc.SandboxServiceExecRequest_Start{
				Start: &setecv1grpc.SessionExecStart{SandboxId: handle, Command: argv},
			}},
			// Close stdin immediately: nothing here feeds input, and a command
			// that reads to EOF would otherwise hang for the whole timeout.
			{Request: &setecv1grpc.SandboxServiceExecRequest_StdinEof{StdinEof: true}},
		},
	}

	if err := chain6Frontend().Exec(stream); err != nil {
		t.Fatalf("Exec(%v): %v", argv, err)
	}

	var res chain6ExecResult
	for _, m := range stream.out {
		switch r := m.GetResponse().(type) {
		case *setecv1grpc.SandboxServiceExecResponse_Output:
			if r.Output.GetStream() == "stderr" {
				res.stderr += string(r.Output.GetData())
			} else {
				res.stdout += string(r.Output.GetData())
			}
		case *setecv1grpc.SandboxServiceExecResponse_Exit:
			if res.exit != nil {
				t.Fatalf("Exec(%v): more than one terminal exit on the stream", argv)
			}
			res.exit = r.Exit
		}
	}
	if res.exit == nil {
		// A stream that ends without an exit is exactly the confusion
		// SessionExecExit.Status exists to prevent: "the command succeeded"
		// and "the VM vanished" must never look alike.
		t.Fatalf("Exec(%v): stream ended with no exit status; stdout=%q stderr=%q",
			argv, res.stdout, res.stderr)
	}
	return res
}

// requireCleanExit fails unless the command ran to completion with code 0.
func requireCleanExit(t *testing.T, what string, res chain6ExecResult) {
	t.Helper()
	if got := res.exit.GetStatus(); got != setecv1grpc.SessionExecExit_STATUS_EXITED {
		t.Fatalf("%s: status=%s (%q) — the command did not run to completion; stderr=%q",
			what, got, res.exit.GetMessage(), res.stderr)
	}
	if code := res.exit.GetExitCode(); code != 0 {
		t.Fatalf("%s: exit code %d; stdout=%q stderr=%q", what, code, res.stdout, res.stderr)
	}
}

// TestChain6_SessionAffinity is the chain-6 exit test.
//
//  1. launch ONE session sandbox with a durable /workspace;
//  2. run a first command that writes a marker into /workspace;
//  3. run a SECOND, independent command that reads it back;
//  4. assert the marker survived, and that both commands ran in the same Pod
//     (the same microVM) — the sandbox was never relaunched underneath them.
//
// Step 4 is not redundant with step 3. A relaunch that happened to remount the
// same PVC would still satisfy step 3 while breaking every in-memory
// expectation a coding agent has of its own session, so both are asserted.
func TestChain6_SessionAffinity(t *testing.T) {
	ctx := context.Background()
	backend := chain6Backend()

	cls := newSandboxClass(chain6ClassName, setecv1alpha1.SandboxClassSpec{
		// VMM satisfies the +required marker on the field; the webhook reads
		// Runtime.Backend and uses that instead.
		VMM:     setecv1alpha1.VMMFirecracker,
		Runtime: &setecv1alpha1.SandboxClassRuntime{Backend: backend},
	})
	if err := k8sClient.Create(ctx, cls); err != nil {
		t.Fatalf("create SandboxClass %q (backend %q): %v", chain6ClassName, backend, err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), &setecv1alpha1.SandboxClass{
			ObjectMeta: metav1.ObjectMeta{Name: chain6ClassName},
		})
	})

	size := resource.MustParse("1Gi")
	spec := minimalSpec("/bin/sh", "-c", "sleep 3600")
	spec.SandboxClassName = chain6ClassName
	spec.Lifecycle = &setecv1alpha1.Lifecycle{
		Mode:      setecv1alpha1.LifecycleModeSession,
		Workspace: &setecv1alpha1.WorkspaceSpec{Size: &size},
	}

	sb := newSandbox(chain6SandboxName, spec)
	createAndCleanup(t, sb)

	key := client.ObjectKeyFromObject(sb)
	waitForPhase(t, key, defaultWait, setecv1alpha1.SandboxPhaseRunning)

	var current setecv1alpha1.Sandbox
	if err := k8sClient.Get(ctx, key, &current); err != nil {
		t.Fatalf("get session sandbox: %v", err)
	}
	handle := fmt.Sprintf("%s/%s/%s", current.Namespace, current.Name, current.UID)

	podKey := types.NamespacedName{Namespace: testNamespace, Name: podNameForChain6(sb.Name)}
	var podBefore corev1.Pod
	if err := k8sClient.Get(ctx, podKey, &podBefore); err != nil {
		t.Fatalf("get session pod: %v", err)
	}

	// (1) First command writes the marker.
	write := chain6Exec(t, handle, "/bin/sh", "-c",
		fmt.Sprintf("printf '%%s' %q > /workspace/chain6-marker", chain6Marker))
	requireCleanExit(t, "first command (write)", write)

	// The failing fixture's hook. A guard that cannot fail is worse than no
	// guard, so the workflow runs this test a second time with
	// SETEC_E2E_CHAIN6_BREAK=relaunch and REQUIRES it to fail. Without that
	// second run, a green tick here is consistent with an assertion that never
	// really looked.
	if os.Getenv(chain6BreakEnv) == chain6BreakRelaunch {
		breakSessionAffinity(t, podKey)
	}

	// (2) Second command — a wholly separate Exec — reads it back.
	read := chain6Exec(t, handle, "/bin/sh", "-c", "cat /workspace/chain6-marker")
	requireCleanExit(t, "second command (read)", read)

	if got := strings.TrimSpace(read.stdout); got != chain6Marker {
		t.Fatalf("CHAIN 6 FAILED: the second command did not see the first command's worktree.\n"+
			"  wrote:  %q\n  read:   %q\n  stderr: %q",
			chain6Marker, got, read.stderr)
	}

	// (3) Same microVM throughout.
	var podAfter corev1.Pod
	if err := k8sClient.Get(ctx, podKey, &podAfter); err != nil {
		t.Fatalf("get session pod after both commands: %v", err)
	}
	if podAfter.UID != podBefore.UID {
		t.Fatalf("CHAIN 6 FAILED: the session was relaunched between commands "+
			"(pod UID %s -> %s). The worktree may have survived on the PVC, but the "+
			"session did not.", podBefore.UID, podAfter.UID)
	}

	t.Logf("CHAIN 6 EXIT TEST PASSED (backend=%s): two commands, one microVM (pod %s), shared /workspace",
		backend, podAfter.Name)
}

// breakSessionAffinity destroys the property under test, so the assertions
// below it can be shown to have teeth.
//
// Deleting the Pod is the sharpest available break: the session's identity IS
// its live sandbox, so a relaunch is exactly the failure a coding agent
// experiences as "my session lost my work". Two outcomes are both correct
// failures, and which one occurs depends on how fast the operator reconciles:
//
//   - the operator relaunches and remounts the same PVC — the marker read
//     SUCCEEDS and the pod-UID assertion fires, which is precisely why that
//     assertion is not redundant with the read;
//   - no pod is available when the second Exec runs — Exec fails instead.
//
// Either way the test must fail. What must never happen is a pass.
func breakSessionAffinity(t *testing.T, podKey types.NamespacedName) {
	t.Helper()

	var pod corev1.Pod
	if err := k8sClient.Get(context.Background(), podKey, &pod); err != nil {
		t.Fatalf("fixture: get session pod before deleting it: %v", err)
	}
	t.Logf("FIXTURE: deleting session pod %s (uid %s) to break affinity on purpose",
		pod.Name, pod.UID)
	if err := k8sClient.Delete(context.Background(), &pod); err != nil {
		t.Fatalf("fixture: delete session pod: %v", err)
	}

	// Give the operator a bounded chance to relaunch. Not waiting for Running:
	// the point is to reach the second Exec with the session broken, and both
	// "relaunched with a new UID" and "nothing there yet" are breaks.
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		var current corev1.Pod
		err := k8sClient.Get(context.Background(), podKey, &current)
		if err == nil && current.UID != pod.UID {
			t.Logf("FIXTURE: session relaunched as uid %s", current.UID)
			return
		}
		time.Sleep(3 * time.Second)
	}
	t.Logf("FIXTURE: no replacement pod within the wait; the second command will fail on a missing sandbox")
}

// podNameForChain6 mirrors the operator's Pod naming for a Sandbox.
func podNameForChain6(sandboxName string) string { return sandboxName + "-vm" }
