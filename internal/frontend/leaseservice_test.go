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

package frontend

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	setecv1grpc "github.com/zeroroot-ai/setec/api/grpc/v1"
	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func leaseClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	utilruntime.Must(setecv1alpha1.AddToScheme(scheme))
	utilruntime.Must(corev1.AddToScheme(scheme))
	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&setecv1alpha1.Sandbox{}).
		Build()
}

// warmClass builds a SandboxClass with a pre-warm image and pool size.
//
//nolint:unparam // name is a parameter for call-site clarity even though every current caller uses "fast".
func warmClass(name, image string, size int32) *setecv1alpha1.SandboxClass {
	return &setecv1alpha1.SandboxClass{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: setecv1alpha1.SandboxClassSpec{
			PreWarmImage:    image,
			PreWarmPoolSize: size,
		},
	}
}

// markAllSandboxesRunning flips every Sandbox in the namespace to Running
// so the pool readiness probe reports them ready. Returns the count.
// Errors are tolerated (best-effort) because it runs in a polling
// goroutine where the object set churns under it; it never calls t.Fatal
// so it is safe to run detached from the test goroutine.
func markAllSandboxesRunning(c client.Client, ns string) int {
	list := &setecv1alpha1.SandboxList{}
	if err := c.List(context.Background(), list, client.InNamespace(ns)); err != nil {
		return 0
	}
	updated := 0
	for i := range list.Items {
		sb := &list.Items[i]
		if sb.Status.Phase == setecv1alpha1.SandboxPhaseRunning {
			updated++
			continue
		}
		sb.Status.Phase = setecv1alpha1.SandboxPhaseRunning
		if err := c.Status().Update(context.Background(), sb); err == nil {
			updated++
		}
	}
	return updated
}

func TestLease_NotFoundClass(t *testing.T) {
	t.Parallel()
	c := leaseClient(t)
	s := &LeaseService{Client: c, AuthDisabled: true, DefaultNamespace: "team-a"}

	_, err := s.Lease(context.Background(), &setecv1grpc.LeaseRequest{SandboxClass: "ghost"})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("want NotFound for missing class, got %v", err)
	}
}

func TestLease_ClassWithoutWarmImageRejected(t *testing.T) {
	t.Parallel()
	c := leaseClient(t, &setecv1alpha1.SandboxClass{ObjectMeta: metav1.ObjectMeta{Name: "bare"}})
	s := &LeaseService{Client: c, AuthDisabled: true, DefaultNamespace: "team-a"}

	_, err := s.Lease(context.Background(), &setecv1grpc.LeaseRequest{SandboxClass: "bare"})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("want FailedPrecondition for class without preWarmImage, got %v", err)
	}
}

func TestLease_EmptyClassIsInvalidArgument(t *testing.T) {
	t.Parallel()
	c := leaseClient(t)
	s := &LeaseService{Client: c, AuthDisabled: true, DefaultNamespace: "team-a"}
	_, err := s.Lease(context.Background(), &setecv1grpc.LeaseRequest{})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("want InvalidArgument for empty class, got %v", err)
	}
}

func TestLease_FailIfEmptyReturnsResourceExhausted(t *testing.T) {
	t.Parallel()
	c := leaseClient(t, warmClass("fast", "img:1", 2))
	s := &LeaseService{Client: c, AuthDisabled: true, DefaultNamespace: "team-a"}

	// Pool not yet replenished, so no warm entry is ready.
	_, err := s.Lease(context.Background(), &setecv1grpc.LeaseRequest{
		SandboxClass: "fast",
		FailIfEmpty:  true,
	})
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("want ResourceExhausted for empty pool + fail_if_empty, got %v", err)
	}
}

func TestLease_ColdLaunchCreatesWarmSandbox(t *testing.T) {
	t.Parallel()
	c := leaseClient(t, warmClass("fast", "img:1", 1))
	s := &LeaseService{Client: c, AuthDisabled: true, DefaultNamespace: "team-a"}

	// Cold launch: drive readiness by flipping any created Sandbox to
	// Running in a background goroutine while Lease polls.
	startMarkRunning(t, c, "team-a")

	resp, err := s.Lease(context.Background(), &setecv1grpc.LeaseRequest{SandboxClass: "fast"})
	if err != nil {
		t.Fatalf("Lease: %v", err)
	}
	if resp.GetWarm() {
		t.Fatalf("first lease on empty pool should be cold (warm=false)")
	}
	if resp.GetLeaseId() == "" || resp.GetSandboxId() == "" {
		t.Fatalf("lease response missing ids: %+v", resp)
	}

	// A warm Sandbox CR exists in the namespace, labelled as a pool entry.
	list := &setecv1alpha1.SandboxList{}
	if err := c.List(context.Background(), list, client.InNamespace("team-a")); err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list.Items) == 0 {
		t.Fatalf("expected at least one Sandbox CR after cold lease")
	}
	if list.Items[0].Spec.SandboxClassName != "fast" || list.Items[0].Spec.Image != "img:1" {
		t.Fatalf("warm sandbox not built from class template: %+v", list.Items[0].Spec)
	}
}

func TestRelease_DestroysLeasedSandbox(t *testing.T) {
	t.Parallel()
	c := leaseClient(t, warmClass("fast", "img:1", 1))
	s := &LeaseService{Client: c, AuthDisabled: true, DefaultNamespace: "team-a"}

	startMarkRunning(t, c, "team-a")
	resp, err := s.Lease(context.Background(), &setecv1grpc.LeaseRequest{SandboxClass: "fast"})
	if err != nil {
		t.Fatalf("Lease: %v", err)
	}
	leasedID := resp.GetSandboxId()
	_, name, _ := parseSandboxID(leasedID)

	if _, err := s.Release(context.Background(), &setecv1grpc.ReleaseRequest{LeaseId: resp.GetLeaseId()}); err != nil {
		t.Fatalf("Release: %v", err)
	}

	// destroy-on-release: the leased Sandbox CR is gone.
	sb := &setecv1alpha1.Sandbox{}
	err = c.Get(context.Background(), types.NamespacedName{Namespace: "team-a", Name: name}, sb)
	if err == nil {
		t.Fatalf("leased Sandbox %q should be destroyed on release", name)
	}
}

func TestRelease_ForeignTenantTokenDenied(t *testing.T) {
	t.Parallel()
	c := leaseClient(t, warmClass("fast", "img:1", 1))
	s := &LeaseService{Client: c, AuthDisabled: true, DefaultNamespace: "team-a"}

	// A token minted for a different namespace must be rejected.
	foreign := leaseTokenFor("team-b", "lease-abc")
	_, err := s.Release(context.Background(), &setecv1grpc.ReleaseRequest{LeaseId: foreign})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("want PermissionDenied for foreign-tenant lease token, got %v", err)
	}
}

func TestRelease_MalformedTokenInvalidArgument(t *testing.T) {
	t.Parallel()
	c := leaseClient(t)
	s := &LeaseService{Client: c, AuthDisabled: true, DefaultNamespace: "team-a"}
	_, err := s.Release(context.Background(), &setecv1grpc.ReleaseRequest{LeaseId: "no-delimiter"})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("want InvalidArgument for malformed token, got %v", err)
	}
}

func TestRelease_UnknownLeaseIsNoop(t *testing.T) {
	t.Parallel()
	c := leaseClient(t)
	s := &LeaseService{Client: c, AuthDisabled: true, DefaultNamespace: "team-a"}
	tok := leaseTokenFor("team-a", "lease-unknown")
	if _, err := s.Release(context.Background(), &setecv1grpc.ReleaseRequest{LeaseId: tok}); err != nil {
		t.Fatalf("releasing an unknown (but well-formed) lease should be a no-op, got %v", err)
	}
}

func TestPoolStatus_ReportsTargetAndReady(t *testing.T) {
	t.Parallel()
	c := leaseClient(t, warmClass("fast", "img:1", 3))
	s := &LeaseService{Client: c, AuthDisabled: true, DefaultNamespace: "team-a"}

	// Register + replenish the pool by leasing once (cold), which also
	// kicks a background replenish toward target.
	startMarkRunning(t, c, "team-a")
	if _, err := s.Lease(context.Background(), &setecv1grpc.LeaseRequest{SandboxClass: "fast"}); err != nil {
		t.Fatalf("Lease: %v", err)
	}

	// Eventually the pool reports target=3 and a non-zero leased count.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := s.PoolStatus(context.Background(), &setecv1grpc.PoolStatusRequest{SandboxClass: "fast"})
		if err != nil {
			t.Fatalf("PoolStatus: %v", err)
		}
		if resp.GetTarget() == 3 && resp.GetLeased() == 1 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("pool never reported target=3 leased=1")
}

func TestPoolStatus_EmptyClassInvalid(t *testing.T) {
	t.Parallel()
	c := leaseClient(t)
	s := &LeaseService{Client: c, AuthDisabled: true, DefaultNamespace: "team-a"}
	_, err := s.PoolStatus(context.Background(), &setecv1grpc.PoolStatusRequest{})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("want InvalidArgument, got %v", err)
	}
}

// startMarkRunning launches a goroutine that flips Sandboxes to Running
// so pool readiness resolves during a test. It is cancelled (and joined)
// via t.Cleanup so it never runs past the test and never calls t.Fatal.
func startMarkRunning(t *testing.T, c client.Client, ns string) {
	t.Helper()
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				markAllSandboxesRunning(c, ns)
			}
		}
	}()
	t.Cleanup(func() {
		close(stop)
		<-done
	})
}
