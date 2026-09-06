// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package snapshot

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	setecgrpcv1 "github.com/zeroroot-ai/setec/api/grpc/v1"
	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
	"github.com/zeroroot-ai/setec/internal/controller/testutil"
	"github.com/zeroroot-ai/setec/internal/metrics"
)

// newPreWarmClass returns a SandboxClass with an active pre-warm pool.
func newPreWarmClass() *setecv1alpha1.SandboxClass {
	return &setecv1alpha1.SandboxClass{
		ObjectMeta: metav1.ObjectMeta{Name: "standard"},
		Spec: setecv1alpha1.SandboxClassSpec{
			PreWarmPoolSize: 2,
			PreWarmImage:    "ghcr.io/org/app:v1",
		},
	}
}

// drainEvents collects every buffered event string from the fake
// recorder without blocking.
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

// newWarmStartCoord mirrors newCoord but exposes the recorder so
// warm-start tests can assert on the emitted events.
func newWarmStartCoord(t *testing.T, na *fakeNodeAgentClient, dialErr error) (*Coordinator, *testutil.FakeEventsRecorder) {
	t.Helper()
	rec := testutil.NewFakeEventsRecorder(32)
	sb := newSandboxForCoord()
	pod := newPodForSandbox(sb, "node-a")
	c := newFakeClient(t, sb, pod)
	return &Coordinator{
		Client:   c,
		Dialer:   &fakeDialer{client: na, dialErr: dialErr},
		Recorder: rec,
		Metrics:  metrics.NewCollectorsWith(prometheus.NewRegistry()),
	}, rec
}

func TestWarmStartFromPool_Restored(t *testing.T) {
	na := &fakeNodeAgentClient{
		claimRes: verifiedClaimRes(),
	}
	coord, rec := newWarmStartCoord(t, na, nil)
	sb := newSandboxForCoord()
	cls := newPreWarmClass()

	outcome, entryID := coord.WarmStartFromPool(context.Background(), sb, cls)
	if outcome != WarmStartRestored {
		t.Fatalf("outcome = %q, want %q", outcome, WarmStartRestored)
	}
	if entryID != "entry-1" {
		t.Fatalf("entryID = %q, want entry-1", entryID)
	}

	// The claim request must carry the class, its pre-warm image, and
	// the pod-derived Firecracker socket.
	if na.lastClaim == nil {
		t.Fatal("ClaimPoolEntry was not invoked")
	}
	if na.lastClaim.GetSandboxClass() != "standard" {
		t.Fatalf("claim class = %q", na.lastClaim.GetSandboxClass())
	}
	if na.lastClaim.GetImageRef() != "ghcr.io/org/app:v1" {
		t.Fatalf("claim image = %q", na.lastClaim.GetImageRef())
	}
	if !strings.Contains(na.lastClaim.GetKataSocketTarget(), "pod-uid-123") {
		t.Fatalf("claim socket = %q, want pod-UID-derived path", na.lastClaim.GetKataSocketTarget())
	}

	evs := strings.Join(drainEvents(rec), "\n")
	if !strings.Contains(evs, EventReasonWarmStartRestored) {
		t.Fatalf("expected %s event, got:\n%s", EventReasonWarmStartRestored, evs)
	}
	if !strings.Contains(evs, EventReasonEntropyReseeded) {
		t.Fatalf("expected %s event for a reseeded restore, got:\n%s", EventReasonEntropyReseeded, evs)
	}
}

func TestWarmStartFromPool_MissFallsBack(t *testing.T) {
	na := &fakeNodeAgentClient{
		claimRes: &setecgrpcv1.ClaimPoolEntryResponse{Claimed: false},
	}
	coord, rec := newWarmStartCoord(t, na, nil)

	outcome, entryID := coord.WarmStartFromPool(context.Background(), newSandboxForCoord(), newPreWarmClass())
	if outcome != WarmStartMiss {
		t.Fatalf("outcome = %q, want %q", outcome, WarmStartMiss)
	}
	if entryID != "" {
		t.Fatalf("entryID = %q, want empty on miss", entryID)
	}
	evs := strings.Join(drainEvents(rec), "\n")
	if !strings.Contains(evs, EventReasonWarmStartColdBoot) {
		t.Fatalf("expected %s event on miss, got:\n%s", EventReasonWarmStartColdBoot, evs)
	}
}

func TestWarmStartFromPool_RestoreFailureFallsBack(t *testing.T) {
	na := &fakeNodeAgentClient{
		claimRes: &setecgrpcv1.ClaimPoolEntryResponse{
			Claimed: true, Success: false, EntryId: "entry-9", Error: "loadSnapshot: boom",
		},
	}
	coord, rec := newWarmStartCoord(t, na, nil)

	outcome, _ := coord.WarmStartFromPool(context.Background(), newSandboxForCoord(), newPreWarmClass())
	if outcome != WarmStartError {
		t.Fatalf("outcome = %q, want %q", outcome, WarmStartError)
	}
	evs := strings.Join(drainEvents(rec), "\n")
	if !strings.Contains(evs, EventReasonWarmStartColdBoot) {
		t.Fatalf("expected %s event on restore failure, got:\n%s", EventReasonWarmStartColdBoot, evs)
	}
}

func TestWarmStartFromPool_RPCErrorFallsBack(t *testing.T) {
	na := &fakeNodeAgentClient{claimErr: errors.New("connection refused")}
	coord, _ := newWarmStartCoord(t, na, nil)

	outcome, _ := coord.WarmStartFromPool(context.Background(), newSandboxForCoord(), newPreWarmClass())
	if outcome != WarmStartError {
		t.Fatalf("outcome = %q, want %q", outcome, WarmStartError)
	}
}

func TestWarmStartFromPool_DialFailureFallsBack(t *testing.T) {
	coord, rec := newWarmStartCoord(t, &fakeNodeAgentClient{}, errors.New("no route to node"))

	outcome, _ := coord.WarmStartFromPool(context.Background(), newSandboxForCoord(), newPreWarmClass())
	if outcome != WarmStartError {
		t.Fatalf("outcome = %q, want %q", outcome, WarmStartError)
	}
	evs := strings.Join(drainEvents(rec), "\n")
	if !strings.Contains(evs, EventReasonWarmStartColdBoot) {
		t.Fatalf("expected %s event on dial failure, got:\n%s", EventReasonWarmStartColdBoot, evs)
	}
}

func TestWarmStartFromPool_MissingPodFallsBack(t *testing.T) {
	// Client without the backing Pod: getPod fails, warm start must
	// degrade to a cold-boot outcome rather than an error.
	rec := testutil.NewFakeEventsRecorder(32)
	sb := newSandboxForCoord()
	coord := &Coordinator{
		Client:   newFakeClient(t, sb),
		Dialer:   &fakeDialer{client: &fakeNodeAgentClient{}},
		Recorder: rec,
	}

	outcome, _ := coord.WarmStartFromPool(context.Background(), sb, newPreWarmClass())
	if outcome != WarmStartError {
		t.Fatalf("outcome = %q, want %q", outcome, WarmStartError)
	}
}
