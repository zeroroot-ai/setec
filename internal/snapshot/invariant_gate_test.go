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
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	setecgrpcv1 "github.com/zeroroot-ai/setec/api/grpc/v1"
	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
	"github.com/zeroroot-ai/setec/internal/controller/testutil"
	"github.com/zeroroot-ai/setec/internal/metrics"
	"github.com/zeroroot-ai/setec/internal/snapshot/gate"
)

// The tests in this file pin the ADR-0005 invariant-gate behaviour at
// the coordinator — the single decision point every pool warm-start
// and snapshot restore/resume passes through (setec#191).

// gateBreakage enumerates ways to strip one verification signal from
// an otherwise fully-verified claim response.
var gateBreakage = []struct {
	name   string
	mutate func(*setecgrpcv1.ClaimPoolEntryResponse)
}{
	{"reseed-unverified", func(r *setecgrpcv1.ClaimPoolEntryResponse) { r.EntropyReseeded = false }},
	{"identity-unverified", func(r *setecgrpcv1.ClaimPoolEntryResponse) { r.Uniquified = false }},
	{"provenance-unverified", func(r *setecgrpcv1.ClaimPoolEntryResponse) { r.ProvenanceVerified = false }},
	{"atrest-unverified", func(r *setecgrpcv1.ClaimPoolEntryResponse) { r.EncryptedAtRest = false }},
	// Invariant 1 has its own signal (the recorded secret-scan
	// verdict, setec#206): losing it must reject the restore even
	// when the provenance attestation (invariant 4) still holds —
	// the gate never infers clean-base from provenance.
	{"clean-base-unverified", func(r *setecgrpcv1.ClaimPoolEntryResponse) { r.CleanBaseVerified = false }},
}

// TestWarmStartFromPool_GateRejectsUnverifiedRestore asserts that a
// node-side "successful" restore missing ANY per-restore verification
// is rejected: outcome WarmStartRejected (the caller destroys the
// sandbox — the VM already holds the unverified state, cold boot is
// not a safe fallback), the VM is paused best-effort, and a typed
// InvariantGateViolation event is emitted.
func TestWarmStartFromPool_GateRejectsUnverifiedRestore(t *testing.T) {
	for _, tc := range gateBreakage {
		t.Run(tc.name, func(t *testing.T) {
			res := verifiedClaimRes("entry-1")
			tc.mutate(res)
			na := &fakeNodeAgentClient{
				claimRes: res,
				pauseRes: &setecgrpcv1.PauseSandboxResponse{Success: true},
			}
			coord, rec := newWarmStartCoord(t, na, nil)

			outcome, entryID := coord.WarmStartFromPool(context.Background(), newSandboxForCoord(), newPreWarmClass())
			if outcome != WarmStartRejected {
				t.Fatalf("outcome = %q, want %q", outcome, WarmStartRejected)
			}
			if entryID != "entry-1" {
				t.Fatalf("entryID = %q, want the consumed entry for the audit trail", entryID)
			}
			if na.lastPause == nil {
				t.Fatal("expected the unverified VM to be paused before hand-back")
			}
			evs := strings.Join(drainEvents(rec), "\n")
			if !strings.Contains(evs, EventReasonInvariantGateViolation) {
				t.Fatalf("expected %s event, got:\n%s", EventReasonInvariantGateViolation, evs)
			}
			if strings.Contains(evs, EventReasonWarmStartRestored) {
				t.Fatalf("a rejected restore must never emit %s:\n%s", EventReasonWarmStartRestored, evs)
			}
		})
	}
}

// TestWarmStartFromPool_DevOptOutServesLoudly asserts the dev-mode
// opt-out (class annotation + cluster dev label) serves the
// unverified restore but emits the loud UnverifiedRestoreAllowed
// warning.
func TestWarmStartFromPool_DevOptOutServesLoudly(t *testing.T) {
	res := verifiedClaimRes("entry-1")
	res.EntropyReseeded = false // one broken verification

	rec := testutil.NewFakeEventsRecorder(32)
	sb := newSandboxForCoord()
	pod := newPodForSandbox(sb, "node-a")
	cls := newPreWarmClass()
	cls.Annotations = map[string]string{gate.AllowUnverifiedRestoresAnnotation: "true"}
	devNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:   gate.DefaultGateNamespace,
		Labels: map[string]string{gate.DefaultAllowDevLabel: "true"},
	}}
	c := newFakeClient(t, sb, pod, cls, devNS)
	na := &fakeNodeAgentClient{claimRes: res}
	coord := &Coordinator{
		Client:   c,
		Dialer:   &fakeDialer{client: na},
		Recorder: rec,
		Metrics:  metrics.NewCollectorsWith(prometheus.NewRegistry()),
		Gate:     &gate.Gate{Reader: c},
	}

	outcome, _ := coord.WarmStartFromPool(context.Background(), sb, cls)
	if outcome != WarmStartRestored {
		t.Fatalf("outcome = %q, want %q under the dev opt-out", outcome, WarmStartRestored)
	}
	evs := strings.Join(drainEvents(rec), "\n")
	if !strings.Contains(evs, EventReasonUnverifiedRestoreAllowed) {
		t.Fatalf("dev opt-out must be loud (%s event), got:\n%s", EventReasonUnverifiedRestoreAllowed, evs)
	}
}

// TestWarmStartFromPool_AnnotationAloneStillRejected pins that the
// class annotation without the cluster-level dev label changes
// nothing.
func TestWarmStartFromPool_AnnotationAloneStillRejected(t *testing.T) {
	res := verifiedClaimRes("entry-1")
	res.Uniquified = false

	rec := testutil.NewFakeEventsRecorder(32)
	sb := newSandboxForCoord()
	pod := newPodForSandbox(sb, "node-a")
	cls := newPreWarmClass()
	cls.Annotations = map[string]string{gate.AllowUnverifiedRestoresAnnotation: "true"}
	unlabelled := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: gate.DefaultGateNamespace}}
	c := newFakeClient(t, sb, pod, cls, unlabelled)
	na := &fakeNodeAgentClient{
		claimRes: res,
		pauseRes: &setecgrpcv1.PauseSandboxResponse{Success: true},
	}
	coord := &Coordinator{
		Client:   c,
		Dialer:   &fakeDialer{client: na},
		Recorder: rec,
		Metrics:  metrics.NewCollectorsWith(prometheus.NewRegistry()),
		Gate:     &gate.Gate{Reader: c},
	}

	outcome, _ := coord.WarmStartFromPool(context.Background(), sb, cls)
	if outcome != WarmStartRejected {
		t.Fatalf("outcome = %q, want %q (annotation alone must not opt out)", outcome, WarmStartRejected)
	}
}

// TestRestoreSandbox_CrossSandboxRefusedBeforeRPC pins the pre-flight
// half of the gate: restoring one sandbox's snapshot into a DIFFERENT
// sandbox reuses session state across sessions (invariants 1/3/4), so
// outside dev-mode it is refused before any state touches the target
// VM.
func TestRestoreSandbox_CrossSandboxRefusedBeforeRPC(t *testing.T) {
	sb := newSandboxForCoord()
	pod := newPodForSandbox(sb, "node-a")
	snap := &setecv1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Namespace: "t-a", Name: "snap-1"},
		Spec: setecv1alpha1.SnapshotSpec{
			SourceSandbox: "some-other-sandbox",
			Node:          "node-a",
		},
	}
	c := newFakeClient(t, sb, pod, snap)
	na := &fakeNodeAgentClient{restoreRes: verifiedRestoreRes()}
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
	if na.lastRestore != nil {
		t.Fatal("cross-sandbox restore must be refused BEFORE the RestoreSandbox RPC")
	}
	evs := strings.Join(drainEvents(rec), "\n")
	if !strings.Contains(evs, EventReasonInvariantGateViolation) {
		t.Fatalf("expected %s event, got:\n%s", EventReasonInvariantGateViolation, evs)
	}
}

// TestRestoreSessionCheckpoint_ForeignRefRefusedBeforeRPC pins the
// gate on the setec#194 resume path: a checkpoint ref outside THIS
// sandbox's SessionCheckpointID namespace reuses another session's
// state (invariants 1/3/4), so it is refused before any state is
// loaded.
func TestRestoreSessionCheckpoint_ForeignRefRefusedBeforeRPC(t *testing.T) {
	sb := sessionSandbox()
	pod := newPodForSandbox(sb, "node-b")
	na := &fakeNodeAgentClient{restoreRes: verifiedRestoreRes()}
	coord := newCoord(newFakeClient(t, sb, pod), &fakeDialer{client: na})

	err := coord.RestoreSessionCheckpoint(context.Background(), sb,
		"t-a-other-session-ckpt-1", "s3", []byte("0123456789abcdef0123456789abcdef"))
	if !errors.Is(err, ErrInvariantGateViolation) {
		t.Fatalf("err = %v, want ErrInvariantGateViolation", err)
	}
	if na.lastRestore != nil {
		t.Fatal("foreign checkpoint resume must be refused BEFORE the RestoreSandbox RPC")
	}
}

// TestRestoreSessionCheckpoint_UnverifiedResumeRefused pins the gate
// on the resume path's post-RPC half: a node-side success missing a
// verification signal is terminal for the restored VM.
func TestRestoreSessionCheckpoint_UnverifiedResumeRefused(t *testing.T) {
	sb := sessionSandbox()
	pod := newPodForSandbox(sb, "node-b")
	res := verifiedRestoreRes()
	res.Uniquified = false
	na := &fakeNodeAgentClient{
		restoreRes: res,
		pauseRes:   &setecgrpcv1.PauseSandboxResponse{Success: true},
	}
	coord := newCoord(newFakeClient(t, sb, pod), &fakeDialer{client: na})

	err := coord.RestoreSessionCheckpoint(context.Background(), sb,
		"t-a-sess-ckpt-4", "s3", []byte("0123456789abcdef0123456789abcdef"))
	if !errors.Is(err, ErrInvariantGateViolation) {
		t.Fatalf("err = %v, want ErrInvariantGateViolation", err)
	}
	if na.lastPause == nil {
		t.Fatal("expected the unverified VM to be paused before hand-back")
	}
}

// TestRestoreSandbox_UnencryptedAtRestRefused pins invariant 5 on the
// snapshot-restore path: a node serving state from an unencrypted
// backend is refused.
func TestRestoreSandbox_UnencryptedAtRestRefused(t *testing.T) {
	sb := newSandboxForCoord()
	pod := newPodForSandbox(sb, "node-a")
	snap := &setecv1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Namespace: "t-a", Name: "snap-1"},
		Spec:       setecv1alpha1.SnapshotSpec{SourceSandbox: "s", Node: "node-a"},
	}
	c := newFakeClient(t, sb, pod, snap)
	res := verifiedRestoreRes()
	res.EncryptedAtRest = false
	na := &fakeNodeAgentClient{
		restoreRes: res,
		pauseRes:   &setecgrpcv1.PauseSandboxResponse{Success: true},
	}
	coord := newCoord(c, &fakeDialer{client: na})

	err := coord.RestoreSandbox(context.Background(), sb, snap)
	if !errors.Is(err, ErrInvariantGateViolation) {
		t.Fatalf("err = %v, want ErrInvariantGateViolation", err)
	}
	if na.lastPause == nil {
		t.Fatal("expected the unverified VM to be paused")
	}
}
