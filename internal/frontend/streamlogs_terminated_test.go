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
	"io"
	"strings"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	setecv1grpc "github.com/zeroroot-ai/setec/api/grpc/v1"
	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// terminatedWorkloadPod builds a Pod whose workload container has
// already exited — the shape a fast one-shot Sandbox is in by the time
// a caller attaches its log stream (setec#263).
func terminatedWorkloadPod(exitCode int32) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "sb-vm", Namespace: "team-a"},
		Status: corev1.PodStatus{
			Phase: corev1.PodSucceeded,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: workloadContainerName,
				State: corev1.ContainerState{
					Terminated: &corev1.ContainerStateTerminated{ExitCode: exitCode},
				},
			}},
		},
	}
}

// recordingOpener is a podLogOpener stub that records every
// PodLogOptions it was asked for and answers each call from a
// scripted list of results.
type recordingOpener struct {
	mu      sync.Mutex
	calls   []corev1.PodLogOptions
	results []openResult
}

type openResult struct {
	rc  io.ReadCloser
	err error
}

func (o *recordingOpener) open(_ context.Context, _, _ string, opts *corev1.PodLogOptions) (io.ReadCloser, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.calls = append(o.calls, *opts)
	if len(o.results) == 0 {
		return nil, errForTesting("recordingOpener: no scripted result left")
	}
	res := o.results[0]
	o.results = o.results[1:]
	return res.rc, res.err
}

func (o *recordingOpener) Calls() []corev1.PodLogOptions {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]corev1.PodLogOptions(nil), o.calls...)
}

// breakingReader yields the configured bytes and then fails, modelling
// a follow stream the kubelet tears down when the container it was
// following terminates mid-flight.
type breakingReader struct {
	rest string
	err  error
}

func (r *breakingReader) Read(p []byte) (int, error) {
	if r.rest == "" {
		return 0, r.err
	}
	n := copy(p, r.rest)
	r.rest = r.rest[n:]
	return n, nil
}

func (r *breakingReader) Close() error { return nil }

func nopReadCloser(s string) io.ReadCloser { return io.NopCloser(strings.NewReader(s)) }

// TestStreamLogs_TerminatedContainerDropsFollow asserts that a
// Follow=true request against a workload container that has already
// terminated is served as a completed-log read: there is nothing left
// to follow, and asking the kubelet to follow a dead container is what
// loses a fast Sandbox's output (setec#263).
func TestStreamLogs_TerminatedContainerDropsFollow(t *testing.T) {
	t.Parallel()
	sb := &setecv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sb", Namespace: "team-a", UID: "u-1"},
	}
	pod := terminatedWorkloadPod(0)
	c := newClient(t, sb, pod)
	cs := k8sfake.NewSimpleClientset(pod) //nolint:staticcheck // NewClientset needs --with-applyconfig wiring, tracked in issue N/A

	var (
		mu       sync.Mutex
		seen     []*corev1.PodLogOptions
		recorder = func(action k8stesting.Action) (bool, runtime.Object, error) {
			if action.GetSubresource() != "log" {
				return false, nil, nil
			}
			generic, ok := action.(k8stesting.GenericAction)
			if !ok {
				return false, nil, nil
			}
			opts, ok := generic.GetValue().(*corev1.PodLogOptions)
			if !ok {
				return false, nil, nil
			}
			mu.Lock()
			seen = append(seen, opts)
			mu.Unlock()
			return false, nil, nil
		}
	)
	cs.PrependReactor("get", "pods", recorder)

	s := &Service{
		Client:           c,
		Clientset:        cs,
		AuthDisabled:     true,
		DefaultNamespace: "team-a",
	}
	stream := &stubStreamServer{ctx: context.Background()}
	if err := s.StreamLogs(&setecv1grpc.StreamLogsRequest{
		SandboxId: "team-a/sb/u-1",
		Follow:    true,
	}, stream); err != nil {
		t.Fatalf("StreamLogs: %v", err)
	}
	if joined := joinChunks(stream.Chunks()); !strings.Contains(joined, "fake logs") {
		t.Fatalf("expected the terminated container's captured log, got %q", joined)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seen) == 0 {
		t.Fatal("expected at least one GetLogs call")
	}
	for i, opts := range seen {
		if opts.Follow {
			t.Errorf("GetLogs call %d used Follow=true against an already-terminated container", i)
		}
		if opts.Container != workloadContainerName {
			t.Errorf("GetLogs call %d container = %q, want %q", i, opts.Container, workloadContainerName)
		}
	}
}

// TestStreamLogs_FollowAttachRaceFallsBack reproduces the race in
// setec#263: the Pod still reads as Running when the service checks,
// the workload exits before the attach lands, and the kubelet refuses
// the follow attach. The output must still reach the caller through a
// completed-log read rather than surfacing as Internal.
func TestStreamLogs_FollowAttachRaceFallsBack(t *testing.T) {
	t.Parallel()
	sb := &setecv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sb", Namespace: "team-a", UID: "u-1"},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "sb-vm", Namespace: "team-a"},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	opener := &recordingOpener{results: []openResult{
		{err: errForTesting(`container "workload" in pod "sb-vm" is terminated`)},
		{rc: nopReadCloser("===GIBSON_TOOL_OUTPUT===\nscan done\n")},
	}}

	s := &Service{
		Client:           newClient(t, sb, pod),
		Clientset:        k8sfake.NewSimpleClientset(pod), //nolint:staticcheck // NewClientset needs --with-applyconfig wiring, tracked in issue N/A
		AuthDisabled:     true,
		DefaultNamespace: "team-a",
		logOpener:        opener.open,
	}
	stream := &stubStreamServer{ctx: context.Background()}
	if err := s.StreamLogs(&setecv1grpc.StreamLogsRequest{
		SandboxId: "team-a/sb/u-1",
		Follow:    true,
	}, stream); err != nil {
		t.Fatalf("StreamLogs: %v", err)
	}
	want := "===GIBSON_TOOL_OUTPUT===\nscan done\n"
	if got := joinChunks(stream.Chunks()); got != want {
		t.Fatalf("log output = %q, want %q", got, want)
	}
	calls := opener.Calls()
	if len(calls) != 2 {
		t.Fatalf("GetLogs calls = %d, want 2 (follow attach then completed-log read)", len(calls))
	}
	if !calls[0].Follow {
		t.Error("first call should have honoured the caller's Follow=true")
	}
	if calls[1].Follow {
		t.Error("fallback call must not use Follow")
	}
}

// TestStreamLogs_MidStreamTerminationYieldsRemainder covers a workload
// that exits while its follow stream is open: the stream breaks after
// partial output, and the caller must still receive the lines produced
// after the break — exactly once, and without an error status.
func TestStreamLogs_MidStreamTerminationYieldsRemainder(t *testing.T) {
	t.Parallel()
	sb := &setecv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sb", Namespace: "team-a", UID: "u-1"},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "sb-vm", Namespace: "team-a"},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	opener := &recordingOpener{results: []openResult{
		{rc: &breakingReader{
			rest: "starting\nprobing\n",
			err:  errForTesting("unexpected EOF: container terminated"),
		}},
		{rc: nopReadCloser("starting\nprobing\n===GIBSON_TOOL_OUTPUT===\ndone\n")},
	}}

	s := &Service{
		Client:           newClient(t, sb, pod),
		Clientset:        k8sfake.NewSimpleClientset(pod), //nolint:staticcheck // NewClientset needs --with-applyconfig wiring, tracked in issue N/A
		AuthDisabled:     true,
		DefaultNamespace: "team-a",
		logOpener:        opener.open,
	}
	stream := &stubStreamServer{ctx: context.Background()}
	if err := s.StreamLogs(&setecv1grpc.StreamLogsRequest{
		SandboxId: "team-a/sb/u-1",
		Follow:    true,
	}, stream); err != nil {
		t.Fatalf("StreamLogs: %v", err)
	}
	want := "starting\nprobing\n===GIBSON_TOOL_OUTPUT===\ndone\n"
	if got := joinChunks(stream.Chunks()); got != want {
		t.Fatalf("log output = %q, want %q (no duplicated or dropped lines)", got, want)
	}
}

// TestStreamLogs_FallbackFailureStillReportsInternal asserts the
// fallback does not paper over a genuinely broken log backend: when the
// completed-log read fails too, the caller gets Internal.
func TestStreamLogs_FallbackFailureStillReportsInternal(t *testing.T) {
	t.Parallel()
	sb := &setecv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sb", Namespace: "team-a", UID: "u-1"},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "sb-vm", Namespace: "team-a"},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	opener := &recordingOpener{results: []openResult{
		{err: errForTesting("kubelet unreachable")},
		{err: errForTesting("kubelet unreachable")},
	}}

	s := &Service{
		Client:           newClient(t, sb, pod),
		Clientset:        k8sfake.NewSimpleClientset(pod), //nolint:staticcheck // NewClientset needs --with-applyconfig wiring, tracked in issue N/A
		AuthDisabled:     true,
		DefaultNamespace: "team-a",
		logOpener:        opener.open,
	}
	stream := &stubStreamServer{ctx: context.Background()}
	err := s.StreamLogs(&setecv1grpc.StreamLogsRequest{
		SandboxId: "team-a/sb/u-1",
		Follow:    true,
	}, stream)
	if status.Code(err) != codes.Internal {
		t.Fatalf("code = %s, want Internal (err=%v)", status.Code(err), err)
	}
	if !strings.Contains(err.Error(), "kubelet unreachable") {
		t.Errorf("error should carry the original attach failure, got %v", err)
	}
}

// TestWorkloadContainerTerminated_Shapes pins the classification the
// follow-drop decision rests on.
func TestWorkloadContainerTerminated_Shapes(t *testing.T) {
	t.Parallel()
	running := &corev1.Pod{Status: corev1.PodStatus{
		Phase: corev1.PodRunning,
		ContainerStatuses: []corev1.ContainerStatus{{
			Name:  workloadContainerName,
			State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
		}},
	}}
	// A Pod can still report Running while the single workload
	// container has already exited; that window is the race.
	exitedInRunningPod := &corev1.Pod{Status: corev1.PodStatus{
		Phase: corev1.PodRunning,
		ContainerStatuses: []corev1.ContainerStatus{{
			Name:  workloadContainerName,
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}},
		}},
	}}
	// Sidecars terminating must not be mistaken for the workload.
	sidecarOnly := &corev1.Pod{Status: corev1.PodStatus{
		Phase: corev1.PodRunning,
		ContainerStatuses: []corev1.ContainerStatus{{
			Name:  "sidecar",
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{}},
		}},
	}}
	succeededNoStatus := &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodSucceeded}}

	cases := []struct {
		name string
		pod  *corev1.Pod
		want bool
	}{
		{"nil", nil, false},
		{"running", running, false},
		{"workload exited in a Running pod", exitedInRunningPod, true},
		{"only a sidecar exited", sidecarOnly, false},
		{"succeeded without container statuses", succeededNoStatus, true},
		{"terminated workload", terminatedWorkloadPod(1), true},
	}
	for _, tc := range cases {
		if got := workloadContainerTerminated(tc.pod); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}
