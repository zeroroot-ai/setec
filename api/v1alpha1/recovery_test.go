// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package v1alpha1

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestRecordRecovery pins the record of setec#237: a resume keeps the time
// of its state, a restart from the workspace has none, each recovery raises
// the count, and a new checkpoint keeps the record.
func TestRecordRecovery(t *testing.T) {
	t.Parallel()
	taken := metav1.NewTime(time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC))
	at := metav1.NewTime(taken.Add(time.Minute))

	ck := &SandboxCheckpointStatus{}
	ck.RecordRecovery(SessionRecoveryResumedFromCheckpoint, at, &taken)
	if ck.LastRecovery != SessionRecoveryResumedFromCheckpoint || ck.Recoveries != 1 ||
		ck.LastRecoveryAt == nil || !ck.LastRecoveryAt.Equal(&at) ||
		ck.LastRecoveryStateTakenAt == nil || !ck.LastRecoveryStateTakenAt.Equal(&taken) {
		t.Fatalf("after a resume: %+v", ck)
	}

	ck.RecordRecovery(SessionRecoveryRestartedFromWorkspace, at, &taken)
	if ck.Recoveries != 2 || ck.LastRecoveryStateTakenAt != nil {
		t.Fatalf("after a restart: count %d, state %v; want 2 and no state", ck.Recoveries, ck.LastRecoveryStateTakenAt)
	}

	next := &SandboxCheckpointStatus{Sequence: 3}
	next.CopyRecovery(ck)
	if next.LastRecovery != ck.LastRecovery || next.Recoveries != 2 || !next.LastRecoveryAt.Equal(ck.LastRecoveryAt) {
		t.Fatalf("a new checkpoint lost the record: %+v", next)
	}
	next.CopyRecovery(nil)
	if next.Recoveries != 2 {
		t.Fatal("CopyRecovery(nil) changed the record")
	}
}
