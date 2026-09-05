// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package frontend

import (
	"context"
	"sync"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	setecv1grpc "github.com/zeroroot-ai/setec/api/grpc/v1"
	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
)

// kindSandbox and kindPod name the two object kinds a Kill deletes.
const (
	kindSandbox = "Sandbox"
	kindPod     = "Pod"
)

// deleteRecord is one Delete call the frontend made, with the grace
// period it asked for. A nil grace means the call carried no explicit
// grace period.
type deleteRecord struct {
	kind  string
	name  string
	grace *int64
}

// recordingDeleteClient wraps a fake client and records every Delete
// call with its resolved DeleteOptions, which is the only place the
// grace period is observable.
func recordingDeleteClient(t *testing.T, rec *[]deleteRecord, mu *sync.Mutex, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	utilruntime.Must(setecv1alpha1.AddToScheme(scheme))
	utilruntime.Must(corev1.AddToScheme(scheme))
	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&setecv1alpha1.Sandbox{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				var o client.DeleteOptions
				o.ApplyOptions(opts)
				kind := kindSandbox
				if _, ok := obj.(*corev1.Pod); ok {
					kind = kindPod
				}
				mu.Lock()
				*rec = append(*rec, deleteRecord{kind: kind, name: obj.GetName(), grace: o.GracePeriodSeconds})
				mu.Unlock()
				return c.Delete(ctx, obj, opts...)
			},
		}).
		Build()
}

func killGraceFixtures() (*setecv1alpha1.Sandbox, *corev1.Pod) {
	sb := &setecv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sb", Namespace: "team-a", UID: types.UID("uid-1")},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "sb-vm", Namespace: "team-a"},
	}
	return sb, pod
}

// TestKill_GraceSecondsDeletesPodWithGraceFirst proves check 3 of
// setec#372: a Kill carrying grace_seconds deletes the Sandbox Pod with
// that grace period before it deletes the Sandbox object, so the
// kubelet sends SIGTERM at once and SIGKILL only after the window. A
// long-lived driver uses the window to checkpoint.
func TestKill_GraceSecondsDeletesPodWithGraceFirst(t *testing.T) {
	t.Parallel()
	var (
		mu   sync.Mutex
		recs []deleteRecord
	)
	sb, pod := killGraceFixtures()
	c := recordingDeleteClient(t, &recs, &mu, sb, pod)
	s := &Service{Client: c, AuthDisabled: true, DefaultNamespace: "team-a"}

	_, err := s.Kill(context.Background(), &setecv1grpc.KillRequest{
		SandboxId:    "team-a/sb/uid-1",
		GraceSeconds: 45,
	})
	if err != nil {
		t.Fatalf("Kill(): %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(recs) != 2 {
		t.Fatalf("Delete calls = %d (%v), want 2: the Pod with grace, then the Sandbox", len(recs), recs)
	}
	if recs[0].kind != kindPod {
		t.Fatalf("first delete = %s, want the Pod (grace must be set before the Sandbox goes)", recs[0].kind)
	}
	if recs[0].grace == nil || *recs[0].grace != 45 {
		t.Fatalf("Pod delete grace = %v, want 45", recs[0].grace)
	}
	if recs[1].kind != kindSandbox {
		t.Fatalf("second delete = %s, want the Sandbox", recs[1].kind)
	}

	got := &setecv1alpha1.Sandbox{}
	if err := c.Get(context.Background(),
		types.NamespacedName{Namespace: "team-a", Name: "sb"}, got); err == nil {
		t.Fatal("Sandbox still present after Kill")
	}
}

// TestKill_NoGraceLeavesPodToOwnerGC pins the default: with no
// grace_seconds the frontend touches only the Sandbox, and
// owner-reference GC collects the Pod on the Pod's own
// terminationGracePeriodSeconds.
func TestKill_NoGraceLeavesPodToOwnerGC(t *testing.T) {
	t.Parallel()
	var (
		mu   sync.Mutex
		recs []deleteRecord
	)
	sb, pod := killGraceFixtures()
	c := recordingDeleteClient(t, &recs, &mu, sb, pod)
	s := &Service{Client: c, AuthDisabled: true, DefaultNamespace: "team-a"}

	if _, err := s.Kill(context.Background(), &setecv1grpc.KillRequest{
		SandboxId: "team-a/sb/uid-1",
	}); err != nil {
		t.Fatalf("Kill(): %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(recs) != 1 || recs[0].kind != kindSandbox {
		t.Fatalf("Delete calls = %v, want exactly one Sandbox delete", recs)
	}
}

// TestKill_GraceWithMissingPodStillDeletesSandbox proves the grace path
// is idempotent: a Sandbox whose Pod is already gone is still deleted,
// with no error surfaced.
func TestKill_GraceWithMissingPodStillDeletesSandbox(t *testing.T) {
	t.Parallel()
	var (
		mu   sync.Mutex
		recs []deleteRecord
	)
	sb, _ := killGraceFixtures()
	c := recordingDeleteClient(t, &recs, &mu, sb)
	s := &Service{Client: c, AuthDisabled: true, DefaultNamespace: "team-a"}

	if _, err := s.Kill(context.Background(), &setecv1grpc.KillRequest{
		SandboxId:    "team-a/sb/uid-1",
		GraceSeconds: 10,
	}); err != nil {
		t.Fatalf("Kill() with a missing Pod should succeed, got %v", err)
	}

	got := &setecv1alpha1.Sandbox{}
	if err := c.Get(context.Background(),
		types.NamespacedName{Namespace: "team-a", Name: "sb"}, got); err == nil {
		t.Fatal("Sandbox still present after Kill")
	}
}

// TestKill_NegativeGraceRejected proves a negative window is refused
// rather than silently read as "kill now".
func TestKill_NegativeGraceRejected(t *testing.T) {
	t.Parallel()
	var (
		mu   sync.Mutex
		recs []deleteRecord
	)
	sb, pod := killGraceFixtures()
	c := recordingDeleteClient(t, &recs, &mu, sb, pod)
	s := &Service{Client: c, AuthDisabled: true, DefaultNamespace: "team-a"}

	_, err := s.Kill(context.Background(), &setecv1grpc.KillRequest{
		SandboxId:    "team-a/sb/uid-1",
		GraceSeconds: -1,
	})
	if got := status.Code(err); got != codes.InvalidArgument {
		t.Fatalf("code = %s, want InvalidArgument", got)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(recs) != 0 {
		t.Fatalf("a rejected Kill must delete nothing, got %v", recs)
	}
}

// TestKill_GraceTenantScopingEnforced proves the tenant guard runs
// before anything is deleted on the grace path.
func TestKill_GraceTenantScopingEnforced(t *testing.T) {
	t.Parallel()
	var (
		mu   sync.Mutex
		recs []deleteRecord
	)
	sb, pod := killGraceFixtures()
	c := recordingDeleteClient(t, &recs, &mu, sb, pod)
	s := &Service{Client: c, AuthDisabled: true, DefaultNamespace: "team-b"}

	_, err := s.Kill(context.Background(), &setecv1grpc.KillRequest{
		SandboxId:    "team-a/sb/uid-1",
		GraceSeconds: 30,
	})
	if got := status.Code(err); got != codes.PermissionDenied {
		t.Fatalf("code = %s, want PermissionDenied", got)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(recs) != 0 {
		t.Fatalf("a denied Kill must delete nothing, got %v", recs)
	}
}
