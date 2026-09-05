// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package frontend

import (
	"context"
	"fmt"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	setecv1grpc "github.com/zeroroot-ai/setec/api/grpc/v1"
	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// sessionCR builds a session-lifecycle Sandbox CR in the given phase
// with a fixed UID, as the frontend would find it in the cluster.
func sessionCR(ns, name, uid string, phase setecv1alpha1.SandboxPhase) *setecv1alpha1.Sandbox {
	return &setecv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns,
			Name:      name,
			UID:       types.UID(uid),
		},
		Spec: setecv1alpha1.SandboxSpec{
			Image:   "alpine:3.19",
			Command: []string{"sh", "-c", "sleep infinity"},
			Lifecycle: &setecv1alpha1.Lifecycle{
				Mode: setecv1alpha1.LifecycleModeSession,
			},
		},
		Status: setecv1alpha1.SandboxStatus{Phase: phase},
	}
}

// handleOf formats the session handle the way Launch returns it.
func handleOf(sb *setecv1alpha1.Sandbox) string {
	return fmt.Sprintf("%s/%s/%s", sb.Namespace, sb.Name, sb.UID)
}

// attachFailureDetail extracts the AttachFailure detail from a gRPC
// error, failing the test when it is absent.
func attachFailureDetail(t *testing.T, err error) *setecv1grpc.AttachFailure {
	t.Helper()
	st := status.Convert(err)
	for _, d := range st.Details() {
		if f, ok := d.(*setecv1grpc.AttachFailure); ok {
			return f
		}
	}
	t.Fatalf("error %v carries no AttachFailure detail", err)
	return nil
}

func TestAttach_RunningSessionResolvesAndStampsActivity(t *testing.T) {
	t.Parallel()
	sb := sessionCR("team-a", "sess-1", "uid-1", setecv1alpha1.SandboxPhaseRunning)
	c := newClient(t, sb)
	s := &Service{Client: c, AuthDisabled: true, DefaultNamespace: "team-a"}

	before := time.Now().Add(-time.Second)
	resp, err := s.Attach(context.Background(), &setecv1grpc.AttachRequest{SandboxId: handleOf(sb)})
	if err != nil {
		t.Fatalf("Attach(): %v", err)
	}
	if resp.GetSandboxId() != handleOf(sb) || resp.GetName() != "sess-1" ||
		resp.GetNamespace() != "team-a" || resp.GetPhase() != string(setecv1alpha1.SandboxPhaseRunning) {
		t.Fatalf("AttachResponse = %+v, want the live session's coordinates", resp)
	}

	// Attach registered activity on the CR so the idle reaper can see it.
	got := &setecv1alpha1.Sandbox{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(sb), got); err != nil {
		t.Fatalf("get Sandbox: %v", err)
	}
	raw, ok := got.Annotations[setecv1alpha1.AnnotationLastActivity]
	if !ok {
		t.Fatalf("last-activity annotation not stamped")
	}
	stamped, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		t.Fatalf("last-activity %q is not RFC3339: %v", raw, err)
	}
	if stamped.Before(before.Truncate(time.Second)) {
		t.Fatalf("last-activity %v predates the Attach", stamped)
	}
}

// TestAttach_ReportsClassAndRuntime asserts a reattaching caller sees
// the class the Sandbox is bound to and the backend the operator
// selected, since it never saw the LaunchResponse.
func TestAttach_ReportsClassAndRuntime(t *testing.T) {
	t.Parallel()
	sb := sessionCR("team-a", "sess-rt", "uid-rt", setecv1alpha1.SandboxPhaseRunning)
	sb.Spec.SandboxClassName = testSandboxClass
	sb.Status.Runtime = &setecv1alpha1.SandboxRuntimeStatus{Chosen: "kata-fc"}
	c := newClient(t, sb)
	s := &Service{Client: c, AuthDisabled: true, DefaultNamespace: "team-a"}

	resp, err := s.Attach(context.Background(), &setecv1grpc.AttachRequest{SandboxId: handleOf(sb)})
	if err != nil {
		t.Fatalf("Attach(): %v", err)
	}
	if resp.GetSandboxClass() != testSandboxClass {
		t.Fatalf("sandbox_class = %q, want standard", resp.GetSandboxClass())
	}
	if resp.GetRuntime() != "kata-fc" {
		t.Fatalf("runtime = %q, want kata-fc", resp.GetRuntime())
	}
}

// TestAttach_RuntimeEmptyWhilePending asserts an attach to a session
// whose backend is not yet selected reports an empty runtime — the
// documented "not yet resolved" shape.
func TestAttach_RuntimeEmptyWhilePending(t *testing.T) {
	t.Parallel()
	sb := sessionCR("team-a", "sess-pending", "uid-p", setecv1alpha1.SandboxPhasePending)
	c := newClient(t, sb)
	s := &Service{Client: c, AuthDisabled: true, DefaultNamespace: "team-a"}

	resp, err := s.Attach(context.Background(), &setecv1grpc.AttachRequest{SandboxId: handleOf(sb)})
	if err != nil {
		t.Fatalf("Attach(): %v", err)
	}
	if resp.GetRuntime() != "" {
		t.Fatalf("runtime = %q, want empty while no backend is selected", resp.GetRuntime())
	}
}

// TestAttach_StatelessAcrossFrontendRestart drives the reattach through
// a brand-new Service instance sharing nothing but cluster state with
// the one that served the first attach — exactly a frontend restart.
func TestAttach_StatelessAcrossFrontendRestart(t *testing.T) {
	t.Parallel()
	sb := sessionCR("team-a", "sess-restart", "uid-r", setecv1alpha1.SandboxPhaseRunning)
	c := newClient(t, sb)

	first := &Service{Client: c, AuthDisabled: true, DefaultNamespace: "team-a"}
	if _, err := first.Attach(context.Background(), &setecv1grpc.AttachRequest{SandboxId: handleOf(sb)}); err != nil {
		t.Fatalf("first Attach(): %v", err)
	}

	restarted := &Service{Client: c, AuthDisabled: true, DefaultNamespace: "team-a"}
	resp, err := restarted.Attach(context.Background(), &setecv1grpc.AttachRequest{SandboxId: handleOf(sb)})
	if err != nil {
		t.Fatalf("Attach() after frontend restart: %v", err)
	}
	if resp.GetName() != sb.Name {
		t.Fatalf("reattach resolved %q, want %q", resp.GetName(), sb.Name)
	}
}

func TestAttach_TypedFailures(t *testing.T) {
	t.Parallel()

	ended := sessionCR("team-a", "sess-done", "uid-d", setecv1alpha1.SandboxPhaseFailed)
	ended.Status.Reason = "Timeout"

	terminating := sessionCR("team-a", "sess-bye", "uid-b", setecv1alpha1.SandboxPhaseRunning)
	terminating.Finalizers = []string{"setec.zeroroot.ai/workspace-teardown"}

	ephemeral := sessionCR("team-a", "eph-1", "uid-e", setecv1alpha1.SandboxPhaseRunning)
	ephemeral.Spec.Lifecycle = nil

	c := newClient(t, ended, terminating, ephemeral)
	s := &Service{Client: c, AuthDisabled: true, DefaultNamespace: "team-a"}
	if err := c.Delete(context.Background(), terminating); err != nil {
		t.Fatalf("start teardown: %v", err)
	}

	cases := []struct {
		name       string
		handle     string
		wantCode   codes.Code
		wantReason setecv1grpc.AttachFailure_Reason
		wantPhase  string
	}{
		{
			name:       "nonexistent session",
			handle:     "team-a/never-was/uid-x",
			wantCode:   codes.NotFound,
			wantReason: setecv1grpc.AttachFailure_REASON_SESSION_NOT_FOUND,
		},
		{
			name:       "stale handle: same name, different UID",
			handle:     "team-a/sess-done/uid-elder",
			wantCode:   codes.NotFound,
			wantReason: setecv1grpc.AttachFailure_REASON_SESSION_NOT_FOUND,
		},
		{
			name:       "ended session",
			handle:     handleOf(ended),
			wantCode:   codes.FailedPrecondition,
			wantReason: setecv1grpc.AttachFailure_REASON_SESSION_ENDED,
			wantPhase:  string(setecv1alpha1.SandboxPhaseFailed),
		},
		{
			name:       "session teardown in progress",
			handle:     handleOf(terminating),
			wantCode:   codes.FailedPrecondition,
			wantReason: setecv1grpc.AttachFailure_REASON_SESSION_ENDED,
		},
		{
			name:       "ephemeral sandbox rejects reattach",
			handle:     handleOf(ephemeral),
			wantCode:   codes.FailedPrecondition,
			wantReason: setecv1grpc.AttachFailure_REASON_NOT_A_SESSION,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.Attach(context.Background(), &setecv1grpc.AttachRequest{SandboxId: tc.handle})
			if status.Code(err) != tc.wantCode {
				t.Fatalf("code = %s (err=%v), want %s", status.Code(err), err, tc.wantCode)
			}
			detail := attachFailureDetail(t, err)
			if detail.GetReason() != tc.wantReason {
				t.Fatalf("reason = %s, want %s", detail.GetReason(), tc.wantReason)
			}
			if tc.wantPhase != "" && detail.GetPhase() != tc.wantPhase {
				t.Fatalf("phase = %q, want %q", detail.GetPhase(), tc.wantPhase)
			}
		})
	}
}

func TestAttach_HandleShapeAndTenantScope(t *testing.T) {
	t.Parallel()
	sb := sessionCR("team-b", "sess-b", "uid-b1", setecv1alpha1.SandboxPhaseRunning)
	c := newClient(t, sb)
	s := &Service{Client: c, AuthDisabled: true, DefaultNamespace: "team-a"}

	for name, handle := range map[string]string{
		"two components":  "team-a/only-name",
		"empty uid":       "team-a/sess/",
		"empty namespace": "/sess/uid",
	} {
		if _, err := s.Attach(context.Background(), &setecv1grpc.AttachRequest{SandboxId: handle}); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("%s: code = %s, want InvalidArgument", name, status.Code(err))
		}
	}

	// A caller must not reach a session outside its tenant namespace,
	// and the refusal must not leak whether the session exists.
	if _, err := s.Attach(context.Background(), &setecv1grpc.AttachRequest{SandboxId: handleOf(sb)}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("cross-tenant attach: code = %s, want PermissionDenied", status.Code(err))
	}
}
