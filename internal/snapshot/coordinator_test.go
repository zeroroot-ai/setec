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

package snapshot

import (
	"context"
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	setecgrpcv1 "github.com/zeroroot-ai/setec/api/grpc/v1"
	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
	"github.com/zeroroot-ai/setec/internal/controller/testutil"
	"github.com/zeroroot-ai/setec/internal/metrics"
	"github.com/zeroroot-ai/setec/internal/snapshot/storage"
)

// --- test doubles --------------------------------------------------

// fakeNodeAgentClient records the most recent request and returns
// the configured response/error. Individual test cases swap the
// response or error via the constructor.
type fakeNodeAgentClient struct {
	createResp *setecgrpcv1.CreateSnapshotResponse
	createErr  error
	restoreRes *setecgrpcv1.RestoreSandboxResponse
	restoreErr error
	pauseRes   *setecgrpcv1.PauseSandboxResponse
	pauseErr   error
	resumeRes  *setecgrpcv1.ResumeSandboxResponse
	resumeErr  error
	deleteRes  *setecgrpcv1.DeleteSnapshotResponse
	deleteErr  error

	// last request captures for assertions.
	lastCreate  *setecgrpcv1.CreateSnapshotRequest
	lastRestore *setecgrpcv1.RestoreSandboxRequest
	lastPause   *setecgrpcv1.PauseSandboxRequest
	lastResume  *setecgrpcv1.ResumeSandboxRequest
}

func (f *fakeNodeAgentClient) CreateSnapshot(_ context.Context, in *setecgrpcv1.CreateSnapshotRequest) (*setecgrpcv1.CreateSnapshotResponse, error) {
	f.lastCreate = in
	return f.createResp, f.createErr
}
func (f *fakeNodeAgentClient) RestoreSandbox(_ context.Context, in *setecgrpcv1.RestoreSandboxRequest) (*setecgrpcv1.RestoreSandboxResponse, error) {
	f.lastRestore = in
	return f.restoreRes, f.restoreErr
}
func (f *fakeNodeAgentClient) PauseSandbox(_ context.Context, in *setecgrpcv1.PauseSandboxRequest) (*setecgrpcv1.PauseSandboxResponse, error) {
	f.lastPause = in
	return f.pauseRes, f.pauseErr
}
func (f *fakeNodeAgentClient) ResumeSandbox(_ context.Context, in *setecgrpcv1.ResumeSandboxRequest) (*setecgrpcv1.ResumeSandboxResponse, error) {
	f.lastResume = in
	return f.resumeRes, f.resumeErr
}
func (f *fakeNodeAgentClient) QueryPool(_ context.Context, _ *setecgrpcv1.QueryPoolRequest) (*setecgrpcv1.QueryPoolResponse, error) {
	return nil, nil
}
func (f *fakeNodeAgentClient) DeleteSnapshot(_ context.Context, _ *setecgrpcv1.DeleteSnapshotRequest) (*setecgrpcv1.DeleteSnapshotResponse, error) {
	return f.deleteRes, f.deleteErr
}

// fakeDialer returns the configured client; if dialErr is non-nil it
// is returned verbatim so we can exercise the NodeAgentUnreachable
// path.
type fakeDialer struct {
	client  NodeAgentClient
	dialErr error
}

func (d *fakeDialer) Dial(_ context.Context, _ string) (NodeAgentClient, error) {
	return d.client, d.dialErr
}

// newScheme builds a runtime.Scheme with the core + setec v1alpha1
// types needed by tests.
func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := scheme.AddToScheme(s); err != nil {
		t.Fatalf("core scheme: %v", err)
	}
	if err := setecv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("setec scheme: %v", err)
	}
	return s
}

func newFakeClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(newScheme(t)).
		WithObjects(objs...).
		WithStatusSubresource(&setecv1alpha1.Snapshot{}, &setecv1alpha1.Sandbox{}).
		Build()
}

// newSandboxForCoord returns a Sandbox with a snapshot-create intent
// plus a backing Pod that's scheduled to node-a.
func newSandboxForCoord() *setecv1alpha1.Sandbox {
	return &setecv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Namespace: "t-a", Name: "s"},
		Spec: setecv1alpha1.SandboxSpec{
			SandboxClassName: "standard",
			Image:            "ghcr.io/org/app:v1",
			Snapshot: &setecv1alpha1.SandboxSnapshotSpec{
				Create:      true,
				Name:        "snap-1",
				AfterCreate: setecv1alpha1.SandboxSnapshotAfterCreateRunning,
			},
		},
		Status: setecv1alpha1.SandboxStatus{
			PodName: "s-vm",
			Phase:   setecv1alpha1.SandboxPhaseRunning,
		},
	}
}

func newPodForSandbox(sb *setecv1alpha1.Sandbox, node string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: sb.Namespace,
			Name:      sb.Status.PodName,
			UID:       "pod-uid-123",
		},
		Spec: corev1.PodSpec{NodeName: node},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
		},
	}
}

// newCoord assembles a Coordinator fed by the given fake client and
// node-agent dialer. Metrics are always enabled (isolated registry)
// so recording is exercised alongside the other behaviour.
func newCoord(c client.Client, dialer NodeAgentDialer) *Coordinator {
	rec := testutil.NewFakeEventsRecorder(32)
	return &Coordinator{
		Client:   c,
		Storage:  nil, // operator-side Coordinator doesn't call Save/Open
		Dialer:   dialer,
		Recorder: rec,
		Metrics:  metrics.NewCollectorsWith(prometheus.NewRegistry()),
	}
}

// --- actual tests --------------------------------------------------

func TestCreateSnapshot_Happy(t *testing.T) {
	sb := newSandboxForCoord()
	pod := newPodForSandbox(sb, "node-a")
	c := newFakeClient(t, sb, pod)
	na := &fakeNodeAgentClient{
		createResp: &setecgrpcv1.CreateSnapshotResponse{
			StorageRef: "t-a-snap-1", SizeBytes: 1024, Sha256: "cafe",
		},
	}
	coord := newCoord(c, &fakeDialer{client: na})

	if err := coord.CreateSnapshot(context.Background(), sb); err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}

	// RPC was issued with the expected fields.
	if na.lastCreate == nil {
		t.Fatalf("CreateSnapshot RPC not invoked")
	}
	if na.lastCreate.SandboxId != "t-a/s" {
		t.Fatalf("sandbox_id = %q", na.lastCreate.SandboxId)
	}
	if na.lastCreate.SourceKataSocket == "" {
		t.Fatalf("expected kata socket path to be populated")
	}

	// Snapshot CR was created with the node-agent's response.
	got := &setecv1alpha1.Snapshot{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "t-a", Name: "snap-1"}, got); err != nil {
		t.Fatalf("get Snapshot: %v", err)
	}
	if got.Spec.StorageRef != "t-a-snap-1" || got.Spec.Size != 1024 || got.Spec.SHA256 != "cafe" {
		t.Fatalf("snapshot fields wrong: %#v", got.Spec)
	}
	if got.Spec.Node != "node-a" {
		t.Fatalf("node = %q, want node-a", got.Spec.Node)
	}
	if got.Status.Phase != setecv1alpha1.SnapshotPhaseReady {
		t.Fatalf("status.phase = %q", got.Status.Phase)
	}
}

func TestCreateSnapshot_NameConflict(t *testing.T) {
	sb := newSandboxForCoord()
	pod := newPodForSandbox(sb, "node-a")
	existing := &setecv1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Namespace: "t-a", Name: "snap-1"},
		Spec: setecv1alpha1.SnapshotSpec{
			SandboxClass: "standard", ImageRef: "x", StorageBackend: "local-disk",
			StorageRef: "x", Node: "node-a", VMM: setecv1alpha1.VMMFirecracker,
		},
	}
	c := newFakeClient(t, sb, pod, existing)
	na := &fakeNodeAgentClient{}
	coord := newCoord(c, &fakeDialer{client: na})

	err := coord.CreateSnapshot(context.Background(), sb)
	if !errors.Is(err, ErrSnapshotNameConflict) {
		t.Fatalf("got %v, want ErrSnapshotNameConflict", err)
	}
	if na.lastCreate != nil {
		t.Fatalf("expected no RPC on name conflict")
	}
}

func TestCreateSnapshot_RPCError(t *testing.T) {
	sb := newSandboxForCoord()
	pod := newPodForSandbox(sb, "node-a")
	c := newFakeClient(t, sb, pod)
	na := &fakeNodeAgentClient{createErr: errors.New("fc: pause failed")}
	coord := newCoord(c, &fakeDialer{client: na})

	err := coord.CreateSnapshot(context.Background(), sb)
	if err == nil {
		t.Fatalf("expected error on RPC failure")
	}
	// No Snapshot CR should exist.
	got := &setecv1alpha1.Snapshot{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "t-a", Name: "snap-1"}, got); err == nil {
		t.Fatalf("Snapshot should not be created on RPC failure")
	}
}

func TestCreateSnapshot_InsufficientStorage(t *testing.T) {
	sb := newSandboxForCoord()
	pod := newPodForSandbox(sb, "node-a")
	c := newFakeClient(t, sb, pod)
	na := &fakeNodeAgentClient{createErr: errors.New("wrapped: " + storage.ErrInsufficientStorage.Error())}
	rec := testutil.NewFakeEventsRecorder(32)
	coord := &Coordinator{Client: c, Dialer: &fakeDialer{client: na}, Recorder: rec}

	if err := coord.CreateSnapshot(context.Background(), sb); err == nil {
		t.Fatalf("expected error")
	}
	found := false
	for len(rec.Events) > 0 {
		e := <-rec.Events
		if contains(e, EventReasonInsufficientStorage) {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected InsufficientStorage Event")
	}
}

func TestCreateSnapshot_DialFailureUnreachable(t *testing.T) {
	sb := newSandboxForCoord()
	pod := newPodForSandbox(sb, "node-a")
	c := newFakeClient(t, sb, pod)
	coord := newCoord(c, &fakeDialer{dialErr: errors.New("conn refused")})

	if err := coord.CreateSnapshot(context.Background(), sb); err == nil {
		t.Fatalf("expected error")
	}
}

func TestCreateSnapshot_PodNotScheduled(t *testing.T) {
	sb := newSandboxForCoord()
	pod := newPodForSandbox(sb, "") // no NodeName
	c := newFakeClient(t, sb, pod)
	coord := newCoord(c, &fakeDialer{client: &fakeNodeAgentClient{}})
	if err := coord.CreateSnapshot(context.Background(), sb); err == nil {
		t.Fatalf("expected error on unscheduled pod")
	}
}

func TestCreateSnapshot_MissingPod(t *testing.T) {
	sb := newSandboxForCoord()
	c := newFakeClient(t, sb)
	coord := newCoord(c, &fakeDialer{client: &fakeNodeAgentClient{}})
	if err := coord.CreateSnapshot(context.Background(), sb); err == nil {
		t.Fatalf("expected error on missing pod")
	}
}

func TestCreateSnapshot_RequiresSnapshotName(t *testing.T) {
	sb := newSandboxForCoord()
	sb.Spec.Snapshot.Name = ""
	c := newFakeClient(t, sb)
	coord := newCoord(c, &fakeDialer{client: &fakeNodeAgentClient{}})
	if err := coord.CreateSnapshot(context.Background(), sb); err == nil {
		t.Fatalf("expected error on empty name")
	}
}

func TestRestoreSandbox_Happy(t *testing.T) {
	sb := newSandboxForCoord()
	pod := newPodForSandbox(sb, "node-a")
	snap := &setecv1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Namespace: "t-a", Name: "snap-1"},
		Spec: setecv1alpha1.SnapshotSpec{
			SandboxClass: "standard", ImageRef: "ghcr.io/org/app:v1",
			Node: "node-a", StorageBackend: "local-disk", StorageRef: "t-a-snap-1",
			VMM: setecv1alpha1.VMMFirecracker,
		},
	}
	c := newFakeClient(t, sb, pod, snap)
	na := &fakeNodeAgentClient{
		restoreRes: &setecgrpcv1.RestoreSandboxResponse{Success: true},
	}
	coord := newCoord(c, &fakeDialer{client: na})

	if err := coord.RestoreSandbox(context.Background(), sb, snap); err != nil {
		t.Fatalf("RestoreSandbox: %v", err)
	}
	if na.lastRestore == nil {
		t.Fatalf("RestoreSandbox RPC not invoked")
	}
}

func TestRestoreSandbox_NodeMismatch(t *testing.T) {
	sb := newSandboxForCoord()
	pod := newPodForSandbox(sb, "node-a")
	snap := &setecv1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Namespace: "t-a", Name: "snap-1"},
		Spec:       setecv1alpha1.SnapshotSpec{Node: "node-b"},
	}
	c := newFakeClient(t, sb, pod, snap)
	coord := newCoord(c, &fakeDialer{client: &fakeNodeAgentClient{}})
	if err := coord.RestoreSandbox(context.Background(), sb, snap); err == nil {
		t.Fatalf("expected node mismatch error")
	}
}

func TestRestoreSandbox_RPCError(t *testing.T) {
	sb := newSandboxForCoord()
	pod := newPodForSandbox(sb, "node-a")
	snap := &setecv1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Namespace: "t-a", Name: "snap-1"},
		Spec:       setecv1alpha1.SnapshotSpec{Node: "node-a"},
	}
	c := newFakeClient(t, sb, pod, snap)
	na := &fakeNodeAgentClient{
		restoreRes: &setecgrpcv1.RestoreSandboxResponse{Success: false, Error: "kernel mismatch"},
	}
	coord := newCoord(c, &fakeDialer{client: na})
	if err := coord.RestoreSandbox(context.Background(), sb, snap); err == nil {
		t.Fatalf("expected error on restore failure")
	}
}

func TestRestoreSandbox_NilInputs(t *testing.T) {
	c := newFakeClient(t)
	coord := newCoord(c, &fakeDialer{client: &fakeNodeAgentClient{}})
	if err := coord.RestoreSandbox(context.Background(), nil, nil); err == nil {
		t.Fatalf("expected error on nil inputs")
	}
}

func TestPauseSandbox_Happy(t *testing.T) {
	sb := newSandboxForCoord()
	pod := newPodForSandbox(sb, "node-a")
	c := newFakeClient(t, sb, pod)
	na := &fakeNodeAgentClient{pauseRes: &setecgrpcv1.PauseSandboxResponse{Success: true}}
	coord := newCoord(c, &fakeDialer{client: na})
	if err := coord.Pause(context.Background(), sb); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if na.lastPause == nil {
		t.Fatalf("Pause RPC not invoked")
	}
}

func TestPauseSandbox_Failure(t *testing.T) {
	sb := newSandboxForCoord()
	pod := newPodForSandbox(sb, "node-a")
	c := newFakeClient(t, sb, pod)
	na := &fakeNodeAgentClient{pauseRes: &setecgrpcv1.PauseSandboxResponse{Success: false, Error: "vm creating"}}
	coord := newCoord(c, &fakeDialer{client: na})
	if err := coord.Pause(context.Background(), sb); err == nil {
		t.Fatalf("expected error")
	}
}

func TestResumeSandbox_Happy(t *testing.T) {
	sb := newSandboxForCoord()
	pod := newPodForSandbox(sb, "node-a")
	c := newFakeClient(t, sb, pod)
	na := &fakeNodeAgentClient{resumeRes: &setecgrpcv1.ResumeSandboxResponse{Success: true}}
	coord := newCoord(c, &fakeDialer{client: na})
	if err := coord.Resume(context.Background(), sb); err != nil {
		t.Fatalf("Resume: %v", err)
	}
}

func TestResumeSandbox_Failure(t *testing.T) {
	sb := newSandboxForCoord()
	pod := newPodForSandbox(sb, "node-a")
	c := newFakeClient(t, sb, pod)
	na := &fakeNodeAgentClient{resumeRes: &setecgrpcv1.ResumeSandboxResponse{Success: false, Error: "corrupt"}}
	coord := newCoord(c, &fakeDialer{client: na})
	if err := coord.Resume(context.Background(), sb); err == nil {
		t.Fatalf("expected error")
	}
}

func TestResumeSandbox_DialFailure(t *testing.T) {
	sb := newSandboxForCoord()
	pod := newPodForSandbox(sb, "node-a")
	c := newFakeClient(t, sb, pod)
	coord := newCoord(c, &fakeDialer{dialErr: errors.New("no route")})
	if err := coord.Resume(context.Background(), sb); err == nil {
		t.Fatalf("expected error")
	}
}

func TestPauseSandbox_DialFailure(t *testing.T) {
	sb := newSandboxForCoord()
	pod := newPodForSandbox(sb, "node-a")
	c := newFakeClient(t, sb, pod)
	coord := newCoord(c, &fakeDialer{dialErr: errors.New("no route")})
	if err := coord.Pause(context.Background(), sb); err == nil {
		t.Fatalf("expected error")
	}
}

func TestSocketForPod_Fallback(t *testing.T) {
	coord := &Coordinator{}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{UID: "abc"}}
	if got := coord.socketForPod(pod); got != "/run/kata-containers/abc/firecracker.socket" {
		t.Fatalf("socketForPod = %q", got)
	}
	// Empty UID returns empty string.
	if got := coord.socketForPod(&corev1.Pod{}); got != "" {
		t.Fatalf("empty UID should yield empty string, got %q", got)
	}
	// Custom pattern honoured.
	coord.KataSocketPattern = "/var/run/kata/%s/sock"
	if got := coord.socketForPod(pod); got != "/var/run/kata/abc/sock" {
		t.Fatalf("custom pattern: %q", got)
	}
}

func TestBackendNameDefault(t *testing.T) {
	if (&Coordinator{}).backendName() != "local-disk" {
		t.Fatalf("default backendName must be local-disk")
	}
	c := &Coordinator{StorageBackendName: "s3"}
	if c.backendName() != "s3" {
		t.Fatalf("custom backendName should be honored")
	}
}

// --- tiny helpers --------------------------------------------------

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (haystack == needle ||
		indexOf(haystack, needle) >= 0)
}

func indexOf(h, n string) int {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return i
		}
	}
	return -1
}
