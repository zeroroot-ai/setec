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
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	setecv1grpc "github.com/zeroroot-ai/setec/api/grpc/v1"
	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
)

// longLivedSandbox builds a session Sandbox with a Running Pod: the
// shape a bank member is in after weeks of uptime.
func longLivedSandbox(t *testing.T, results ...openResult) (*Service, *recordingOpener) {
	t.Helper()
	sb := &setecv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sb", Namespace: "team-a", UID: "u-1"},
		Spec: setecv1alpha1.SandboxSpec{
			Lifecycle: &setecv1alpha1.Lifecycle{Mode: setecv1alpha1.LifecycleModeSession},
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "sb-vm", Namespace: "team-a"},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:  workloadContainerName,
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
			}},
		},
	}
	opener := &recordingOpener{results: results}
	s := &Service{
		Client:           newClient(t, sb, pod),
		Clientset:        k8sfake.NewSimpleClientset(pod), //nolint:staticcheck // NewClientset needs --with-applyconfig wiring, tracked in issue N/A
		AuthDisabled:     true,
		DefaultNamespace: "team-a",
		logOpener:        opener.open,
	}
	return s, opener
}

// TestStreamLogs_ReconnectGetsTailPlusNewLines proves check 4 of
// setec#372: a client that dropped its stream reconnects with
// tail_lines and receives that many trailing lines plus every new one.
// The read is bounded at the source — the kubelet is asked for the
// tail — so reattaching to a Sandbox that has run for weeks never
// replays the whole history.
func TestStreamLogs_ReconnectGetsTailPlusNewLines(t *testing.T) {
	t.Parallel()
	s, opener := longLivedSandbox(t, openResult{rc: nopReadCloser(
		stamped("2026-09-01T09:59:58.000Z", "turn 41 done") +
			stamped("2026-09-01T09:59:59.000Z", "idle") +
			stamped("2026-09-01T10:00:00.000Z", "turn 42 start") +
			stamped("2026-09-01T10:00:01.000Z", "turn 42 done"))})

	stream := &stubStreamServer{ctx: context.Background()}
	if err := s.StreamLogs(&setecv1grpc.StreamLogsRequest{
		SandboxId: "team-a/sb/u-1",
		Follow:    true,
		TailLines: 4,
	}, stream); err != nil {
		t.Fatalf("StreamLogs: %v", err)
	}

	want := "turn 41 done\nidle\nturn 42 start\nturn 42 done\n"
	if got := joinChunks(stream.Chunks()); got != want {
		t.Fatalf("log output = %q, want %q", got, want)
	}

	calls := opener.Calls()
	if len(calls) != 1 {
		t.Fatalf("GetLogs calls = %d, want 1", len(calls))
	}
	if calls[0].TailLines == nil || *calls[0].TailLines != 4 {
		t.Fatalf("TailLines = %v, want 4: the tail must be bounded at the kubelet, not in the frontend", calls[0].TailLines)
	}
	if !calls[0].Follow {
		t.Error("a reconnect with follow=true must keep following new lines")
	}
	if !calls[0].Timestamps {
		t.Error("every read asks for timestamps so a resumed read can position itself")
	}
}

// TestStreamLogs_NoTailLinesReadsWholeLog pins the default: a caller
// that names no tail gets the whole log the kubelet still holds.
func TestStreamLogs_NoTailLinesReadsWholeLog(t *testing.T) {
	t.Parallel()
	s, opener := longLivedSandbox(t, openResult{rc: nopReadCloser(
		stamped("2026-09-01T10:00:00.000Z", "first"))})

	stream := &stubStreamServer{ctx: context.Background()}
	if err := s.StreamLogs(&setecv1grpc.StreamLogsRequest{
		SandboxId: "team-a/sb/u-1",
	}, stream); err != nil {
		t.Fatalf("StreamLogs: %v", err)
	}
	if got := joinChunks(stream.Chunks()); got != "first\n" {
		t.Fatalf("log output = %q, want %q", got, "first\n")
	}
	if tail := opener.Calls()[0].TailLines; tail != nil {
		t.Fatalf("TailLines = %v, want nil", *tail)
	}
}

// TestStreamLogs_NegativeTailLinesRejected proves a negative tail is
// refused rather than silently read as "the whole log".
func TestStreamLogs_NegativeTailLinesRejected(t *testing.T) {
	t.Parallel()
	s, opener := longLivedSandbox(t)

	stream := &stubStreamServer{ctx: context.Background()}
	err := s.StreamLogs(&setecv1grpc.StreamLogsRequest{
		SandboxId: "team-a/sb/u-1",
		TailLines: -5,
	}, stream)
	if got := status.Code(err); got != codes.InvalidArgument {
		t.Fatalf("code = %s, want InvalidArgument", got)
	}
	if len(opener.Calls()) != 0 {
		t.Fatal("a rejected request must not open a log stream")
	}
}

// TestStreamLogs_LineBufferIsBounded proves the frontend's buffer does
// not grow with the workload's output. A single line past
// maxLogLineBytes is refused by the scanner rather than accumulated,
// and the failure is reported instead of hidden.
func TestStreamLogs_LineBufferIsBounded(t *testing.T) {
	t.Parallel()
	huge := "2026-09-01T10:00:00.000Z " + strings.Repeat("x", 2*maxLogLineBytes) + "\n"
	s, _ := longLivedSandbox(t,
		openResult{rc: nopReadCloser(huge)},
		openResult{err: errForTesting("kubelet unreachable")},
	)

	stream := &stubStreamServer{ctx: context.Background()}
	err := s.StreamLogs(&setecv1grpc.StreamLogsRequest{
		SandboxId: "team-a/sb/u-1",
	}, stream)
	if status.Code(err) != codes.Internal {
		t.Fatalf("code = %s, want Internal (err=%v)", status.Code(err), err)
	}
	for _, c := range stream.Chunks() {
		if len(c.GetData()) > maxLogLineBytes {
			t.Fatalf("chunk of %d bytes exceeds the %d byte cap", len(c.GetData()), maxLogLineBytes)
		}
	}
}
