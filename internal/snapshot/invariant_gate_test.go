// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package snapshot

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	setecgrpcv1 "github.com/zeroroot-ai/setec/api/grpc/v1"
	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
	"github.com/zeroroot-ai/setec/internal/controller/testutil"
	"github.com/zeroroot-ai/setec/internal/metrics"
)

// The tests in this file pin the docs/design/isolation.md invariant-gate behavior at
// the coordinator — the single decision point every pool warm-start
// and snapshot restore/resume passes through (setec#191).

// gateBreakage enumerates ways to strip one verification signal from
// an otherwise fully-verified claim response.
// VM.
func TestRestoreSandbox_CrossSandboxRefusedBeforeRPC(t *testing.T) {
	sb := newSandboxForCoord()
	pod := newPodForSandbox(sb, "node-a")
	snap := &setecv1alpha1.Snapshot{
		Namespace: "t-a", Name: "snap-1",
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
		"t-a-other-session-ckpt-1", "s3", []byte("0123456789abcdef0123456789abcdef"), time.Time{})
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
		"t-a-sess-ckpt-4", "s3", []byte("0123456789abcdef0123456789abcdef"), time.Time{})
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
		Namespace: "t-a", Name: "snap-1",
		Spec: setecv1alpha1.SnapshotSpec{SourceSandbox: "s", Node: "node-a"},
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

// drainEvents returns each event that the recorder holds now.
func drainEvents(rec *testutil.FakeEventsRecorder) []string {
	var out []string
	for {
		select {
		case ev := <-rec.Events:
			out = append(out, ev)
		default:
			return out
		}
	}
}
