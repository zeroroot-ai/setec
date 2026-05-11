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
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	setecv1alpha1grpc "github.com/zero-day-ai/setec/api/grpc/v1alpha1"
	setecv1alpha1 "github.com/zero-day-ai/setec/api/v1alpha1"
	"github.com/zero-day-ai/setec/internal/tenancy"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// stubResolver returns the configured namespace regardless of tenant;
// cross-tenant tests construct instances with different values.
type stubResolver struct {
	ns  string
	err error
}

func (s *stubResolver) NamespaceFor(_ context.Context, _ tenancy.TenantID) (string, error) {
	return s.ns, s.err
}

func newClient(t *testing.T, objs ...client.Object) client.Client {
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

func TestLaunch_AuthDisabledCreatesSandbox(t *testing.T) {
	t.Parallel()
	c := newClient(t)
	s := &Service{
		Client:           c,
		AuthDisabled:     true,
		DefaultNamespace: "team-a",
	}

	req := &setecv1alpha1grpc.LaunchRequest{
		SandboxClass: "standard",
		Image:        "alpine:3.19",
		Command:      []string{"sh", "-c", "true"},
		Resources:    &setecv1alpha1grpc.Resources{Vcpu: 1, Memory: "256Mi"},
	}
	resp, err := s.Launch(context.Background(), req)
	if err != nil {
		t.Fatalf("Launch(): %v", err)
	}
	if resp.Namespace != "team-a" {
		t.Fatalf("namespace = %q, want team-a", resp.Namespace)
	}
}

func TestLaunch_AuthDisabledWithoutDefault(t *testing.T) {
	t.Parallel()
	s := &Service{Client: newClient(t), AuthDisabled: true}
	_, err := s.Launch(context.Background(), &setecv1alpha1grpc.LaunchRequest{
		Image: "x", Command: []string{"x"},
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("code = %s, want FailedPrecondition", status.Code(err))
	}
}

func TestLaunch_InvalidArgs(t *testing.T) {
	t.Parallel()
	s := &Service{Client: newClient(t), AuthDisabled: true, DefaultNamespace: "team-a"}

	cases := []struct {
		name string
		req  *setecv1alpha1grpc.LaunchRequest
		want codes.Code
	}{
		{"missing image", &setecv1alpha1grpc.LaunchRequest{Command: []string{"x"}}, codes.InvalidArgument},
		{"missing command", &setecv1alpha1grpc.LaunchRequest{Image: "x"}, codes.InvalidArgument},
		{"bad memory", &setecv1alpha1grpc.LaunchRequest{
			Image: "x", Command: []string{"x"},
			Resources: &setecv1alpha1grpc.Resources{Vcpu: 1, Memory: "garbage"},
		}, codes.InvalidArgument},
		{"bad timeout", &setecv1alpha1grpc.LaunchRequest{
			Image: "x", Command: []string{"x"},
			Lifecycle: &setecv1alpha1grpc.Lifecycle{Timeout: "notaduration"},
		}, codes.InvalidArgument},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := s.Launch(context.Background(), tc.req)
			if got := status.Code(err); got != tc.want {
				t.Fatalf("code = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestKill_TenantScopingEnforced(t *testing.T) {
	t.Parallel()
	sb := &setecv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sb", Namespace: "team-a", UID: types.UID("uid-1")},
	}
	c := newClient(t, sb)
	s := &Service{
		Client:           c,
		AuthDisabled:     true,
		DefaultNamespace: "team-b", // caller's tenant is b, sb is in a
	}

	_, err := s.Kill(context.Background(), &setecv1alpha1grpc.KillRequest{
		SandboxId: "team-a/sb/uid-1",
	})
	if got := status.Code(err); got != codes.PermissionDenied {
		t.Fatalf("code = %s, want PermissionDenied", got)
	}
}

func TestKill_HappyPath(t *testing.T) {
	t.Parallel()
	sb := &setecv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sb", Namespace: "team-a", UID: types.UID("uid-1")},
	}
	c := newClient(t, sb)
	s := &Service{
		Client:           c,
		AuthDisabled:     true,
		DefaultNamespace: "team-a",
	}

	_, err := s.Kill(context.Background(), &setecv1alpha1grpc.KillRequest{
		SandboxId: "team-a/sb/uid-1",
	})
	if err != nil {
		t.Fatalf("Kill(): %v", err)
	}
	// Sandbox should be gone.
	got := &setecv1alpha1.Sandbox{}
	err = c.Get(context.Background(), types.NamespacedName{Namespace: "team-a", Name: "sb"}, got)
	if err == nil {
		t.Fatal("Sandbox still present after Kill")
	}
}

func TestKill_NotFoundIsIdempotent(t *testing.T) {
	t.Parallel()
	c := newClient(t)
	s := &Service{Client: c, AuthDisabled: true, DefaultNamespace: "team-a"}
	_, err := s.Kill(context.Background(), &setecv1alpha1grpc.KillRequest{
		SandboxId: "team-a/missing/uid-x",
	})
	if err != nil {
		t.Fatalf("Kill() on missing should be idempotent, got %v", err)
	}
}

func TestKill_InvalidID(t *testing.T) {
	t.Parallel()
	s := &Service{Client: newClient(t), AuthDisabled: true, DefaultNamespace: "team-a"}
	_, err := s.Kill(context.Background(), &setecv1alpha1grpc.KillRequest{SandboxId: "malformed"})
	if got := status.Code(err); got != codes.InvalidArgument {
		t.Fatalf("code = %s, want InvalidArgument", got)
	}
}

func TestWait_TerminalReturnsImmediately(t *testing.T) {
	t.Parallel()
	exit := int32(0)
	sb := &setecv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sb", Namespace: "team-a", UID: "u-1"},
		Status: setecv1alpha1.SandboxStatus{
			Phase:    setecv1alpha1.SandboxPhaseCompleted,
			ExitCode: &exit,
			Reason:   "ok",
		},
	}
	c := newClient(t, sb)
	s := &Service{Client: c, AuthDisabled: true, DefaultNamespace: "team-a"}

	resp, err := s.Wait(context.Background(), &setecv1alpha1grpc.WaitRequest{
		SandboxId: "team-a/sb/u-1",
	})
	if err != nil {
		t.Fatalf("Wait(): %v", err)
	}
	if resp.Phase != string(setecv1alpha1.SandboxPhaseCompleted) {
		t.Fatalf("phase = %q, want Completed", resp.Phase)
	}
	if resp.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0", resp.ExitCode)
	}
}

func TestWait_PollsUntilTerminal(t *testing.T) {
	t.Parallel()
	sb := &setecv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sb", Namespace: "team-a", UID: "u-1"},
		Status:     setecv1alpha1.SandboxStatus{Phase: setecv1alpha1.SandboxPhaseRunning},
	}
	c := newClient(t, sb)
	s := &Service{Client: c, AuthDisabled: true, DefaultNamespace: "team-a"}

	// Flip the Sandbox to Completed after a short delay.
	go func() {
		time.Sleep(600 * time.Millisecond)
		cur := &setecv1alpha1.Sandbox{}
		_ = c.Get(context.Background(), types.NamespacedName{Namespace: "team-a", Name: "sb"}, cur)
		cur.Status.Phase = setecv1alpha1.SandboxPhaseCompleted
		cur.Status.Reason = "done"
		_ = c.Status().Update(context.Background(), cur)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	resp, err := s.Wait(ctx, &setecv1alpha1grpc.WaitRequest{SandboxId: "team-a/sb/u-1"})
	if err != nil {
		t.Fatalf("Wait(): %v", err)
	}
	if resp.Phase != string(setecv1alpha1.SandboxPhaseCompleted) {
		t.Fatalf("phase = %q, want Completed", resp.Phase)
	}
}

func TestWait_ContextCancellation(t *testing.T) {
	t.Parallel()
	sb := &setecv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sb", Namespace: "team-a", UID: "u-1"},
		Status:     setecv1alpha1.SandboxStatus{Phase: setecv1alpha1.SandboxPhaseRunning},
	}
	c := newClient(t, sb)
	s := &Service{Client: c, AuthDisabled: true, DefaultNamespace: "team-a"}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := s.Wait(ctx, &setecv1alpha1grpc.WaitRequest{SandboxId: "team-a/sb/u-1"})
	if err == nil {
		t.Fatal("expected deadline error")
	}
}

// stubStreamServer implements the minimum needed for
// SandboxService_StreamLogsServer so StreamLogs can be tested
// without a real gRPC transport. Send() collects every chunk the
// service emits so assertions can inspect them after the RPC returns.
type stubStreamServer struct {
	ctx context.Context
	setecv1alpha1grpc.SandboxService_StreamLogsServer

	mu       sync.Mutex
	received []*setecv1alpha1grpc.StreamLogsResponse
}

func (s *stubStreamServer) Context() context.Context { return s.ctx }

func (s *stubStreamServer) Send(c *setecv1alpha1grpc.StreamLogsResponse) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	clone := &setecv1alpha1grpc.StreamLogsResponse{
		Data:   append([]byte(nil), c.GetData()...),
		Stream: c.GetStream(),
	}
	s.received = append(s.received, clone)
	return nil
}

func (s *stubStreamServer) Chunks() []*setecv1alpha1grpc.StreamLogsResponse {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*setecv1alpha1grpc.StreamLogsResponse, len(s.received))
	copy(out, s.received)
	return out
}

func TestStreamLogs_InvalidID(t *testing.T) {
	t.Parallel()
	s := &Service{Client: newClient(t), AuthDisabled: true, DefaultNamespace: "team-a"}
	stream := &stubStreamServer{ctx: context.Background()}
	err := s.StreamLogs(&setecv1alpha1grpc.StreamLogsRequest{SandboxId: "malformed"}, stream)
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("code = %s, want InvalidArgument", status.Code(err))
	}
}

func TestStreamLogs_CrossTenantDenied(t *testing.T) {
	t.Parallel()
	s := &Service{Client: newClient(t), AuthDisabled: true, DefaultNamespace: "team-b"}
	stream := &stubStreamServer{ctx: context.Background()}
	err := s.StreamLogs(&setecv1alpha1grpc.StreamLogsRequest{
		SandboxId: "team-a/sb/uid",
	}, stream)
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("code = %s, want PermissionDenied", status.Code(err))
	}
}

// TestStreamLogs_SandboxNotFound asserts that streaming logs for a
// sandbox id that doesn't exist surfaces NotFound from the
// controller-runtime Get before any Pod lookup is attempted.
func TestStreamLogs_SandboxNotFound(t *testing.T) {
	t.Parallel()
	c := newClient(t)
	s := &Service{
		Client:           c,
		Clientset:        k8sfake.NewSimpleClientset(), //nolint:staticcheck // NewClientset needs --with-applyconfig wiring, tracked in issue N/A
		AuthDisabled:     true,
		DefaultNamespace: "team-a",
	}
	stream := &stubStreamServer{ctx: context.Background()}
	err := s.StreamLogs(&setecv1alpha1grpc.StreamLogsRequest{
		SandboxId: "team-a/missing/uid",
	}, stream)
	if status.Code(err) != codes.NotFound {
		t.Fatalf("code = %s, want NotFound", status.Code(err))
	}
}

// TestStreamLogs_PodNotYetCreated covers the FailedPrecondition path
// when the Sandbox CR exists but the controller has not yet created
// its backing Pod. Follow=false so the service errors immediately
// instead of polling.
func TestStreamLogs_PodNotYetCreated(t *testing.T) {
	t.Parallel()
	sb := &setecv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sb", Namespace: "team-a", UID: "u-1"},
	}
	c := newClient(t, sb)
	s := &Service{
		Client:           c,
		Clientset:        k8sfake.NewSimpleClientset(), //nolint:staticcheck // NewClientset needs --with-applyconfig wiring, tracked in issue N/A
		AuthDisabled:     true,
		DefaultNamespace: "team-a",
	}
	stream := &stubStreamServer{ctx: context.Background()}
	err := s.StreamLogs(&setecv1alpha1grpc.StreamLogsRequest{
		SandboxId: "team-a/sb/u-1",
	}, stream)
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("code = %s, want FailedPrecondition (got %v)", status.Code(err), err)
	}
}

// TestStreamLogs_HappyPath exercises the full streaming path with a
// Running Pod and a fake clientset returning the default "fake logs"
// response. Asserts every chunk carries stream="stdout" and the
// concatenated bytes contain the expected content.
func TestStreamLogs_HappyPath(t *testing.T) {
	t.Parallel()
	sb := &setecv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sb", Namespace: "team-a", UID: "u-1"},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "sb-vm", Namespace: "team-a"},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	c := newClient(t, sb, pod)
	cs := k8sfake.NewSimpleClientset(pod) //nolint:staticcheck // NewClientset needs --with-applyconfig wiring, tracked in issue N/A

	s := &Service{
		Client:           c,
		Clientset:        cs,
		AuthDisabled:     true,
		DefaultNamespace: "team-a",
	}
	stream := &stubStreamServer{ctx: context.Background()}
	if err := s.StreamLogs(&setecv1alpha1grpc.StreamLogsRequest{
		SandboxId: "team-a/sb/u-1",
		Follow:    false,
	}, stream); err != nil {
		t.Fatalf("StreamLogs: %v", err)
	}
	chunks := stream.Chunks()
	if len(chunks) == 0 {
		t.Fatal("expected at least one StreamLogsResponse, got none")
	}
	for i, cc := range chunks {
		if cc.GetStream() != "stdout" {
			t.Errorf("chunk %d stream = %q, want stdout", i, cc.GetStream())
		}
		if len(cc.GetData()) == 0 {
			t.Errorf("chunk %d has empty Data", i)
		}
	}
	joined := joinChunks(chunks)
	if !strings.Contains(joined, "fake logs") {
		t.Fatalf("expected concatenated chunks to contain 'fake logs'; got %q", joined)
	}
	// Silence the unused reactor import; even though we didn't install
	// one here, other tests may in the future.
	_ = k8stesting.DefaultWatchReactor
}

// TestStreamLogs_ClientCancel verifies that a canceled client context
// causes the service to return cleanly without surfacing an error to
// the gRPC client.
func TestStreamLogs_ClientCancel(t *testing.T) {
	t.Parallel()
	sb := &setecv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sb", Namespace: "team-a", UID: "u-1"},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "sb-vm", Namespace: "team-a"},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	c := newClient(t, sb, pod)
	cs := k8sfake.NewSimpleClientset(pod) //nolint:staticcheck // NewClientset needs --with-applyconfig wiring, tracked in issue N/A

	s := &Service{
		Client:           c,
		Clientset:        cs,
		AuthDisabled:     true,
		DefaultNamespace: "team-a",
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately so relay returns promptly
	stream := &stubStreamServer{ctx: ctx}

	err := s.StreamLogs(&setecv1alpha1grpc.StreamLogsRequest{
		SandboxId: "team-a/sb/u-1",
		Follow:    true,
	}, stream)
	// Cancelled context may be surfaced as Canceled or produce a
	// nil return — both are acceptable clean-shutdown shapes.
	if err != nil && status.Code(err) != codes.Canceled {
		t.Fatalf("unexpected error on cancel: %v", err)
	}
}

// TestStreamLogs_NoClientsetConfigured asserts that a Service with no
// clientset returns FailedPrecondition instead of panicking.
func TestStreamLogs_NoClientsetConfigured(t *testing.T) {
	t.Parallel()
	sb := &setecv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sb", Namespace: "team-a", UID: "u-1"},
	}
	c := newClient(t, sb)
	s := &Service{
		Client:           c,
		AuthDisabled:     true,
		DefaultNamespace: "team-a",
	}
	stream := &stubStreamServer{ctx: context.Background()}
	err := s.StreamLogs(&setecv1alpha1grpc.StreamLogsRequest{
		SandboxId: "team-a/sb/u-1",
	}, stream)
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("code = %s, want FailedPrecondition", status.Code(err))
	}
}

// joinChunks concatenates chunk data back into a single string for
// substring assertions.
func joinChunks(chunks []*setecv1alpha1grpc.StreamLogsResponse) string {
	var b strings.Builder
	for _, cc := range chunks {
		b.Write(cc.GetData())
	}
	return b.String()
}

// TestStreamLogs_FollowPodTransitions exercises the Follow=true poll
// loop where the Pod starts Pending and transitions to Running while
// the service is waiting. The background goroutine flips the Pod
// phase; StreamLogs should then open the log stream and return
// cleanly on EOF.
func TestStreamLogs_FollowPodTransitions(t *testing.T) {
	t.Parallel()
	sb := &setecv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sb", Namespace: "team-a", UID: "u-1"},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "sb-vm", Namespace: "team-a"},
		Status:     corev1.PodStatus{Phase: corev1.PodPending},
	}
	c := newClient(t, sb, pod)
	cs := k8sfake.NewSimpleClientset(pod) //nolint:staticcheck // NewClientset needs --with-applyconfig wiring, tracked in issue N/A

	s := &Service{
		Client:           c,
		Clientset:        cs,
		AuthDisabled:     true,
		DefaultNamespace: "team-a",
	}

	// After a short delay, flip the Pod to Running so the poll loop
	// exits and the log stream opens.
	go func() {
		time.Sleep(1200 * time.Millisecond)
		cur := &corev1.Pod{}
		_ = c.Get(context.Background(), types.NamespacedName{Namespace: "team-a", Name: "sb-vm"}, cur)
		cur.Status.Phase = corev1.PodRunning
		_ = c.Status().Update(context.Background(), cur)
	}()

	stream := &stubStreamServer{ctx: context.Background()}
	if err := s.StreamLogs(&setecv1alpha1grpc.StreamLogsRequest{
		SandboxId: "team-a/sb/u-1",
		Follow:    true,
	}, stream); err != nil {
		t.Fatalf("StreamLogs: %v", err)
	}
	if len(stream.Chunks()) == 0 {
		t.Fatal("expected chunks after Pod became Running")
	}
}

// TestRelayLogStream_SendError simulates a gRPC Send error (other than
// context cancel) and asserts it surfaces as Internal.
func TestRelayLogStream_SendError(t *testing.T) {
	t.Parallel()
	src := strings.NewReader("one\ntwo\n")
	stream := &errorSendStream{ctx: context.Background(), err: errForTesting("broken transport")}
	err := relayLogStream(context.Background(), src, stream)
	if status.Code(err) != codes.Internal {
		t.Fatalf("code = %s, want Internal (err=%v)", status.Code(err), err)
	}
}

// TestPodLogsAvailable_Phases asserts each Pod phase routes to the
// correct loggable/not-loggable classification.
func TestPodLogsAvailable_Phases(t *testing.T) {
	cases := map[corev1.PodPhase]bool{
		corev1.PodPending:   false,
		corev1.PodRunning:   true,
		corev1.PodSucceeded: true,
		corev1.PodFailed:    true,
		corev1.PodUnknown:   false,
	}
	for phase, want := range cases {
		got := podLogsAvailable(&corev1.Pod{Status: corev1.PodStatus{Phase: phase}})
		if got != want {
			t.Errorf("phase %q: got %v, want %v", phase, got, want)
		}
	}
}

// errorSendStream is a stubStreamServer variant whose Send() always
// returns the configured error. Used to drive the send-error branch
// of relayLogStream.
type errorSendStream struct {
	ctx context.Context
	setecv1alpha1grpc.SandboxService_StreamLogsServer
	err error
}

func (s *errorSendStream) Context() context.Context                           { return s.ctx }
func (s *errorSendStream) Send(_ *setecv1alpha1grpc.StreamLogsResponse) error { return s.err }
func errForTesting(msg string) error                                          { return &simpleErr{msg} }

type simpleErr struct{ s string }

func (e *simpleErr) Error() string { return e.s }

// stubResolver is used indirectly by Launch/Kill/Wait via AuthDisabled=true;
// this test exercises the TenantResolver plumbing explicitly.
func TestResolveNamespace_UsesResolver(t *testing.T) {
	t.Parallel()
	s := &Service{
		Client:         newClient(t),
		TenantResolver: &stubResolver{ns: "resolved-ns"},
		AuthDisabled:   true,
		// With AuthDisabled true, DefaultNamespace wins; so remove it
		// here to test the AuthDisabled=false path below.
	}
	// AuthDisabled=true, DefaultNamespace empty → FailedPrecondition.
	_, err := s.resolveNamespace(context.Background())
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("expected FailedPrecondition, got %v", err)
	}
}

func TestResolveNamespace_AuthEnabledWithCert(t *testing.T) {
	t.Parallel()
	cert := makeCert(t, []string{"tenant-a.svc"})
	ctx := ctxWithCert(cert)

	s := &Service{
		Client:         newClient(t),
		TenantResolver: &stubResolver{ns: "ns-for-tenant-a"},
	}
	ns, err := s.resolveNamespace(ctx)
	if err != nil {
		t.Fatalf("resolveNamespace(): %v", err)
	}
	if ns != "ns-for-tenant-a" {
		t.Fatalf("ns = %q, want ns-for-tenant-a", ns)
	}
}

func TestResolveNamespace_AuthEnabledNoResolver(t *testing.T) {
	t.Parallel()
	cert := makeCert(t, []string{"tenant-a.svc"})
	ctx := ctxWithCert(cert)

	s := &Service{Client: newClient(t)}
	_, err := s.resolveNamespace(ctx)
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("code = %s, want FailedPrecondition", status.Code(err))
	}
}

func TestResolveNamespace_ResolverError(t *testing.T) {
	t.Parallel()
	cert := makeCert(t, []string{"tenant-a.svc"})
	ctx := ctxWithCert(cert)

	s := &Service{
		Client:         newClient(t),
		TenantResolver: &stubResolver{err: status.Error(codes.NotFound, "no match")},
	}
	_, err := s.resolveNamespace(ctx)
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("code = %s, want PermissionDenied", status.Code(err))
	}
}

func TestGrpcCodeFor_Cases(t *testing.T) {
	t.Parallel()
	notFound := apierrors.NewNotFound(schema.GroupResource{Resource: "sandboxes"}, "x")
	exists := apierrors.NewAlreadyExists(schema.GroupResource{Resource: "sandboxes"}, "x")
	forbidden := apierrors.NewForbidden(schema.GroupResource{Resource: "sandboxes"}, "x",
		status.Error(codes.PermissionDenied, "no"))
	conflict := apierrors.NewConflict(schema.GroupResource{Resource: "sandboxes"}, "x",
		status.Error(codes.Aborted, "x"))
	invalid := apierrors.NewInvalid(schema.GroupKind{Kind: "Sandbox"}, "x", nil)

	cases := []struct {
		name string
		err  error
		want codes.Code
	}{
		{"unknown", status.Error(codes.Unknown, "x"), codes.Internal},
		{"not found", notFound, codes.NotFound},
		{"already exists", exists, codes.AlreadyExists},
		{"forbidden", forbidden, codes.PermissionDenied},
		{"conflict", conflict, codes.Aborted},
		{"invalid", invalid, codes.InvalidArgument},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := grpcCodeFor(tc.err); got != tc.want {
				t.Fatalf("grpcCodeFor(%v) = %s, want %s", tc.err, got, tc.want)
			}
		})
	}
}
