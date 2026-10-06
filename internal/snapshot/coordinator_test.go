// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package snapshot

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
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

// verifiedRestoreRes returns a RestoreSandboxResponse carrying every
// docs/design/isolation.md per-restore verification signal, so the invariant gate
// admits the restore. Tests that exercise a specific missing signal
// build their own response.
func verifiedRestoreRes() *setecgrpcv1.RestoreSandboxResponse {
	return &setecgrpcv1.RestoreSandboxResponse{
		Success:         true,
		EntropyReseeded: true,
		Uniquified:      true,
		EncryptedAtRest: true,
	}
}

// fakeNodeAgentClient records the most recent request and returns
// the configured response/error. Individual test cases swap the
// response or error via the constructor.
type fakeNodeAgentClient struct {
	createResp *setecgrpcv1.CreateSnapshotResponse
	createErr  error
	// onCreate, when set, is called from inside CreateSnapshot before the
	// response is returned.
	onCreate   func()
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
	// onCreate runs while the storage write is nominally in flight. It is
	// what lets a test observe a transient phase instead of only the
	// terminal one.
	if f.onCreate != nil {
		f.onCreate()
	}
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
		Namespace: "t-a", Name: "s",
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
		Namespace: sb.Namespace,
		Name:      sb.Status.PodName,
		UID:       "pod-uid-123",
		Spec:      corev1.PodSpec{NodeName: node},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
		},
	}
}

// newCoord assembles a Coordinator fed by the given fake client and
// node-agent dialer. Metrics are always enabled (isolated registry)
// so recording is exercised alongside the other behavior.
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
	// The node-agent resolves the Firecracker socket itself. The operator
	// names the Pod by UID (setec#19).
	if na.lastCreate.GetSourcePodUid() != string(pod.UID) {
		t.Fatalf("source_pod_uid = %q, want the Pod UID %q", na.lastCreate.GetSourcePodUid(), pod.UID)
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
		Namespace: "t-a", Name: "snap-1",
		Spec: setecv1alpha1.SnapshotSpec{
			SandboxClass: "standard", ImageRef: "x", StorageBackend: "local-disk",
			StorageRef: "x", Node: "node-a",
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

// TestCreateSnapshot_RPCError_ReportsFailed is the Failed half of
// setec#129. The CR is KEPT and reports Failed with a reason.
//
// This test used to assert the opposite — "No Snapshot CR should exist" —
// because the CR was created after the write, so a failed write left no
// record anywhere and SnapshotPhaseFailed had no assignment in the whole
// repo. An operator could not tell a snapshot that failed from one that
// was never asked for. The expectation was inverted deliberately.
func TestCreateSnapshot_RPCError_ReportsFailed(t *testing.T) {
	sb := newSandboxForCoord()
	pod := newPodForSandbox(sb, "node-a")
	c := newFakeClient(t, sb, pod)
	na := &fakeNodeAgentClient{createErr: errors.New("fc: pause failed")}
	coord := newCoord(c, &fakeDialer{client: na})

	err := coord.CreateSnapshot(context.Background(), sb)
	if err == nil {
		t.Fatalf("expected error on RPC failure")
	}

	got := &setecv1alpha1.Snapshot{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "t-a", Name: "snap-1"}, got); err != nil {
		t.Fatalf("Snapshot CR should be retained for observability: %v", err)
	}
	if got.Status.Phase != setecv1alpha1.SnapshotPhaseFailed {
		t.Fatalf("status.phase = %q, want Failed", got.Status.Phase)
	}
	if got.Status.Reason == "" {
		t.Fatalf("status.reason is empty; a Failed phase with no reason tells an operator nothing")
	}
	// A Failed snapshot must not look restorable: no storage reference was
	// ever returned, and Validate has to refuse it.
	if got.Spec.StorageRef != "" {
		t.Fatalf("spec.storageRef = %q, want empty on a failed write", got.Spec.StorageRef)
	}
	if v := Validate(sb, got, nil); len(v) == 0 {
		t.Fatalf("Validate accepted a Failed snapshot as a restore source")
	}
}

// TestCreateSnapshot_ReportsCreatingDuringWrite is the Creating half of
// setec#129. The phase is observed FROM INSIDE the storage write, which
// is the only way to tell a transient phase apart from one that is never
// written: asserting after CreateSnapshot returns would only ever see the
// terminal value.
func TestCreateSnapshot_ReportsCreatingDuringWrite(t *testing.T) {
	sb := newSandboxForCoord()
	pod := newPodForSandbox(sb, "node-a")
	c := newFakeClient(t, sb, pod)

	var duringPhase setecv1alpha1.SnapshotPhase
	var duringStorageRef string
	var duringErr error
	na := &fakeNodeAgentClient{
		createResp: &setecgrpcv1.CreateSnapshotResponse{
			StorageRef: "t-a-snap-1", SizeBytes: 4096, Sha256: "deadbeef",
		},
		onCreate: func() {
			mid := &setecv1alpha1.Snapshot{}
			duringErr = c.Get(context.Background(),
				types.NamespacedName{Namespace: "t-a", Name: "snap-1"}, mid)
			duringPhase = mid.Status.Phase
			duringStorageRef = mid.Spec.StorageRef
		},
	}
	coord := newCoord(c, &fakeDialer{client: na})

	if err := coord.CreateSnapshot(context.Background(), sb); err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	if duringErr != nil {
		t.Fatalf("Snapshot CR did not exist while the write was in flight: %v", duringErr)
	}
	if duringPhase != setecv1alpha1.SnapshotPhaseCreating {
		t.Fatalf("phase during write = %q, want Creating", duringPhase)
	}
	if duringStorageRef != "" {
		t.Fatalf("spec.storageRef = %q during the write; the backend has not returned one yet", duringStorageRef)
	}

	got := &setecv1alpha1.Snapshot{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "t-a", Name: "snap-1"}, got); err != nil {
		t.Fatalf("get Snapshot: %v", err)
	}
	if got.Status.Phase != setecv1alpha1.SnapshotPhaseReady {
		t.Fatalf("final phase = %q, want Ready", got.Status.Phase)
	}
	if got.Spec.StorageRef != "t-a-snap-1" || got.Spec.Size != 4096 || got.Spec.SHA256 != "deadbeef" {
		t.Fatalf("backend result not recorded on the CR: %#v", got.Spec)
	}
}

// TestCreateSnapshot_DialErrorReportsFailed covers the other way the
// write can never start. The node-agent is unreachable, so no RPC is
// issued at all, and the phase must still say so.
func TestCreateSnapshot_DialErrorReportsFailed(t *testing.T) {
	sb := newSandboxForCoord()
	pod := newPodForSandbox(sb, "node-a")
	c := newFakeClient(t, sb, pod)
	coord := newCoord(c, &fakeDialer{dialErr: errors.New("connection refused")})

	if err := coord.CreateSnapshot(context.Background(), sb); err == nil {
		t.Fatalf("expected error when the node-agent cannot be dialed")
	}
	got := &setecv1alpha1.Snapshot{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "t-a", Name: "snap-1"}, got); err != nil {
		t.Fatalf("Snapshot CR should be retained: %v", err)
	}
	if got.Status.Phase != setecv1alpha1.SnapshotPhaseFailed {
		t.Fatalf("status.phase = %q, want Failed", got.Status.Phase)
	}
	if got.Status.Reason != "NodeAgentUnreachable" {
		t.Fatalf("status.reason = %q, want NodeAgentUnreachable", got.Status.Reason)
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
		Namespace: "t-a", Name: "snap-1",
		Spec: setecv1alpha1.SnapshotSpec{
			SourceSandbox: "s",
			SandboxClass:  "standard", ImageRef: "ghcr.io/org/app:v1",
			Node: "node-a", StorageBackend: "local-disk", StorageRef: "t-a-snap-1",
		},
	}
	c := newFakeClient(t, sb, pod, snap)
	na := &fakeNodeAgentClient{
		restoreRes: verifiedRestoreRes(),
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
		Namespace: "t-a", Name: "snap-1",
		Spec: setecv1alpha1.SnapshotSpec{Node: "node-b"},
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
		Namespace: "t-a", Name: "snap-1",
		Spec: setecv1alpha1.SnapshotSpec{SourceSandbox: "s", Node: "node-a"},
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

// TestRestoreSandbox_EmitsEntropyReseededEvent asserts the Coordinator
// surfaces the node-agent's entropy_reseeded confirmation as a Normal
// event on the Sandbox (setec#72 observability requirement).
func TestRestoreSandbox_EmitsEntropyReseededEvent(t *testing.T) {
	sb := newSandboxForCoord()
	pod := newPodForSandbox(sb, "node-a")
	snap := &setecv1alpha1.Snapshot{
		Namespace: "t-a", Name: "snap-1",
		Spec: setecv1alpha1.SnapshotSpec{
			SourceSandbox: "s",
			SandboxClass:  "standard", Node: "node-a",
			StorageBackend: "local-disk", StorageRef: "t-a-snap-1",
		},
	}
	c := newFakeClient(t, sb, pod, snap)
	na := &fakeNodeAgentClient{
		restoreRes: verifiedRestoreRes(),
	}
	rec := testutil.NewFakeEventsRecorder(32)
	coord := &Coordinator{
		Client:   c,
		Dialer:   &fakeDialer{client: na},
		Recorder: rec,
		Metrics:  metrics.NewCollectorsWith(prometheus.NewRegistry()),
	}

	if err := coord.RestoreSandbox(context.Background(), sb, snap); err != nil {
		t.Fatalf("RestoreSandbox: %v", err)
	}

	var sawReseeded bool
	for {
		select {
		case ev := <-rec.Events:
			if strings.Contains(ev, EventReasonEntropyReseeded) {
				sawReseeded = true
			}
			continue
		default:
		}
		break
	}
	if !sawReseeded {
		t.Fatal("expected an EntropyReseeded event after a reseeded restore")
	}
}

// TestRestoreSandbox_NoReseedEventWithoutConfirmation pins that a
// restore whose reseed the node-agent did NOT confirm (e.g.
// --entropy-reseed=off) is refused by the docs/design/isolation.md invariant gate:
// no EntropyReseeded event, a typed InvariantGateViolation instead,
// and the restore surfaces the terminal gate error.
func TestRestoreSandbox_NoReseedEventWithoutConfirmation(t *testing.T) {
	sb := newSandboxForCoord()
	pod := newPodForSandbox(sb, "node-a")
	snap := &setecv1alpha1.Snapshot{
		Namespace: "t-a", Name: "snap-1",
		Spec: setecv1alpha1.SnapshotSpec{SourceSandbox: "s", Node: "node-a"},
	}
	c := newFakeClient(t, sb, pod, snap)
	res := verifiedRestoreRes()
	res.EntropyReseeded = false
	na := &fakeNodeAgentClient{
		restoreRes: res,
		pauseRes:   &setecgrpcv1.PauseSandboxResponse{Success: true},
	}
	rec := testutil.NewFakeEventsRecorder(32)
	coord := &Coordinator{
		Client:   c,
		Dialer:   &fakeDialer{client: na},
		Recorder: rec,
		Metrics:  metrics.NewCollectorsWith(prometheus.NewRegistry()),
	}
	err := coord.RestoreSandbox(context.Background(), sb, snap)
	if !errors.Is(err, ErrInvariantGateViolation) {
		t.Fatalf("err = %v, want ErrInvariantGateViolation", err)
	}
	// The unverified VM must have been paused before hand-back.
	if na.lastPause == nil {
		t.Fatal("expected the unverified VM to be paused")
	}
	sawViolation := false
	for {
		select {
		case ev := <-rec.Events:
			if strings.Contains(ev, EventReasonEntropyReseeded) {
				t.Fatalf("EntropyReseeded event emitted without confirmation: %s", ev)
			}
			if strings.Contains(ev, EventReasonInvariantGateViolation) {
				sawViolation = true
			}
			continue
		default:
		}
		break
	}
	if !sawViolation {
		t.Fatal("expected an InvariantGateViolation event on an unconfirmed reseed")
	}
}
