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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	setecgrpcv1 "github.com/zeroroot-ai/setec/api/grpc/v1"
	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
	"github.com/zeroroot-ai/setec/internal/controller/testutil"
	"github.com/zeroroot-ai/setec/internal/metrics"
)

// TestRestoreSandbox_PassesIdentityFieldsToNodeAgent pins the
// setec#189 request contract: the Coordinator forwards the identity a
// restored guest must adopt (sandbox id for CID ownership, the Pod's
// CNI-assigned IP, and the hostname).
func TestRestoreSandbox_PassesIdentityFieldsToNodeAgent(t *testing.T) {
	sb := newSandboxForCoord()
	pod := newPodForSandbox(sb, "node-a")
	pod.Status.PodIP = "10.7.8.9"
	snap := &setecv1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Namespace: "t-a", Name: "snap-1"},
		Spec:       setecv1alpha1.SnapshotSpec{SourceSandbox: "s", Node: "node-a", StorageRef: "t-a-snap-1"},
	}
	c := newFakeClient(t, sb, pod, snap)
	na := &fakeNodeAgentClient{
		restoreRes: verifiedRestoreRes(),
	}
	coord := newCoord(c, &fakeDialer{client: na})

	if err := coord.RestoreSandbox(context.Background(), sb, snap); err != nil {
		t.Fatalf("RestoreSandbox: %v", err)
	}
	if na.lastRestore.GetSandboxId() != "t-a/s" {
		t.Fatalf("sandbox_id = %q, want t-a/s", na.lastRestore.GetSandboxId())
	}
	if na.lastRestore.GetPodIp() != "10.7.8.9" {
		t.Fatalf("pod_ip = %q, want the Pod's CNI-assigned IP", na.lastRestore.GetPodIp())
	}
	if na.lastRestore.GetHostname() != "s" {
		t.Fatalf("hostname = %q, want the sandbox name", na.lastRestore.GetHostname())
	}
}

// TestRestoreSandbox_EmitsSandboxUniquifiedEvent asserts the
// Coordinator surfaces the node-agent's uniquified confirmation as a
// Normal event on a served restore — and that an UNCONFIRMED
// uniquification is refused outright by the ADR-0005 invariant gate
// (no event, terminal error) instead of being served quietly.
func TestRestoreSandbox_EmitsSandboxUniquifiedEvent(t *testing.T) {
	for _, confirmed := range []bool{true, false} {
		sb := newSandboxForCoord()
		pod := newPodForSandbox(sb, "node-a")
		snap := &setecv1alpha1.Snapshot{
			ObjectMeta: metav1.ObjectMeta{Namespace: "t-a", Name: "snap-1"},
			Spec:       setecv1alpha1.SnapshotSpec{SourceSandbox: "s", Node: "node-a"},
		}
		c := newFakeClient(t, sb, pod, snap)
		res := verifiedRestoreRes()
		res.Uniquified = confirmed
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
		if confirmed && err != nil {
			t.Fatalf("RestoreSandbox: %v", err)
		}
		if !confirmed && !errors.Is(err, ErrInvariantGateViolation) {
			t.Fatalf("err = %v, want ErrInvariantGateViolation for an unconfirmed uniquification", err)
		}
		saw := false
		for {
			select {
			case ev := <-rec.Events:
				if strings.Contains(ev, EventReasonSandboxUniquified) {
					saw = true
				}
				continue
			default:
			}
			break
		}
		if saw != confirmed {
			t.Fatalf("uniquified=%v but event seen=%v (the event must track explicit confirmation only)", confirmed, saw)
		}
	}
}

// TestWarmStart_PassesIdentityFieldsToClaim pins the same contract on
// the pool warm-start path.
func TestWarmStart_PassesIdentityFieldsToClaim(t *testing.T) {
	sb := newSandboxForCoord()
	pod := newPodForSandbox(sb, "node-a")
	pod.Status.PodIP = "10.7.8.10"
	cls := &setecv1alpha1.SandboxClass{
		ObjectMeta: metav1.ObjectMeta{Name: "standard"},
		Spec: setecv1alpha1.SandboxClassSpec{
			PreWarmPoolSize: 1,
			PreWarmImage:    "ghcr.io/org/app:v1",
		},
	}
	c := newFakeClient(t, sb, pod)
	na := &fakeNodeAgentClient{
		claimRes: verifiedClaimRes(),
	}
	rec := testutil.NewFakeEventsRecorder(32)
	coord := &Coordinator{
		Client:   c,
		Dialer:   &fakeDialer{client: na},
		Recorder: rec,
		Metrics:  metrics.NewCollectorsWith(prometheus.NewRegistry()),
	}
	outcome, _ := coord.WarmStartFromPool(context.Background(), sb, cls)
	if outcome != WarmStartRestored {
		t.Fatalf("outcome = %v, want restored", outcome)
	}
	if na.lastClaim.GetPodIp() != "10.7.8.10" || na.lastClaim.GetHostname() != "s" {
		t.Fatalf("claim did not carry identity fields: %+v", na.lastClaim)
	}
	saw := false
	for {
		select {
		case ev := <-rec.Events:
			if strings.Contains(ev, EventReasonSandboxUniquified) {
				saw = true
			}
			continue
		default:
		}
		break
	}
	if !saw {
		t.Fatal("expected a SandboxUniquified event after a uniquified warm start")
	}
}
