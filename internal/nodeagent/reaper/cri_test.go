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

package reaper

import (
	"context"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
)

// fakeRuntimeService is a minimal CRI RuntimeService implementing only the
// methods the reaper uses. The embedded Unimplemented server supplies the rest.
type fakeRuntimeService struct {
	runtimeapi.UnimplementedRuntimeServiceServer

	mu       sync.Mutex
	items    []*runtimeapi.PodSandbox
	wantFilt runtimeapi.PodSandboxState
	stopped  []string
	removed  []string
}

func (f *fakeRuntimeService) ListPodSandbox(_ context.Context, req *runtimeapi.ListPodSandboxRequest) (*runtimeapi.ListPodSandboxResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	// Record the requested filter so the test can assert NOT_READY scoping.
	if req.GetFilter() != nil && req.GetFilter().GetState() != nil {
		f.wantFilt = req.GetFilter().GetState().GetState()
	}
	return &runtimeapi.ListPodSandboxResponse{Items: f.items}, nil
}

func (f *fakeRuntimeService) StopPodSandbox(_ context.Context, req *runtimeapi.StopPodSandboxRequest) (*runtimeapi.StopPodSandboxResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped = append(f.stopped, req.GetPodSandboxId())
	return &runtimeapi.StopPodSandboxResponse{}, nil
}

func (f *fakeRuntimeService) RemovePodSandbox(_ context.Context, req *runtimeapi.RemovePodSandboxRequest) (*runtimeapi.RemovePodSandboxResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = append(f.removed, req.GetPodSandboxId())
	return &runtimeapi.RemovePodSandboxResponse{}, nil
}

// startFakeCRI serves the fake RuntimeService on a unix socket in a temp dir and
// returns the socket path. The server is stopped via t.Cleanup.
func startFakeCRI(t *testing.T, svc *fakeRuntimeService) string {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "cri.sock")
	lis, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer()
	runtimeapi.RegisterRuntimeServiceServer(srv, svc)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return sock
}

func TestCRIClient_ListNotReadySandboxes_MapsFields(t *testing.T) {
	created := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)
	svc := &fakeRuntimeService{items: []*runtimeapi.PodSandbox{
		{
			Id:             "abc123",
			RuntimeHandler: "kata-fc",
			CreatedAt:      created.UnixNano(),
			Metadata:       &runtimeapi.PodSandboxMetadata{Name: "mypod", Namespace: "myns"},
		},
		{
			Id:             "no-ts",
			RuntimeHandler: "kata-qemu",
			CreatedAt:      0, // unreported
		},
	}}
	sock := startFakeCRI(t, svc)

	c, err := NewCRIClient(sock)
	if err != nil {
		t.Fatalf("NewCRIClient: %v", err)
	}
	defer func() { _ = c.Close() }()

	got, err := c.ListNotReadySandboxes(context.Background())
	if err != nil {
		t.Fatalf("ListNotReadySandboxes: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d sandboxes, want 2", len(got))
	}
	if got[0].ID != "abc123" || got[0].Name != "mypod" || got[0].Namespace != "myns" ||
		got[0].RuntimeHandler != "kata-fc" || !got[0].CreatedAt.Equal(created) {
		t.Fatalf("field mapping wrong: %+v", got[0])
	}
	if !got[1].CreatedAt.IsZero() {
		t.Fatalf("unreported CreatedAt should map to zero time, got %v", got[1].CreatedAt)
	}
	// The reaper must scope the list to NOT_READY sandboxes.
	svc.mu.Lock()
	filt := svc.wantFilt
	svc.mu.Unlock()
	if filt != runtimeapi.PodSandboxState_SANDBOX_NOTREADY {
		t.Fatalf("ListPodSandbox filter = %v, want SANDBOX_NOTREADY", filt)
	}
}

func TestCRIClient_ForceRemove_StopsThenRemoves(t *testing.T) {
	svc := &fakeRuntimeService{}
	sock := startFakeCRI(t, svc)
	c, err := NewCRIClient(sock)
	if err != nil {
		t.Fatalf("NewCRIClient: %v", err)
	}
	defer func() { _ = c.Close() }()

	if err := c.ForceRemove(context.Background(), "sb-1"); err != nil {
		t.Fatalf("ForceRemove: %v", err)
	}
	svc.mu.Lock()
	defer svc.mu.Unlock()
	if len(svc.stopped) != 1 || svc.stopped[0] != "sb-1" {
		t.Fatalf("stopped = %v, want [sb-1]", svc.stopped)
	}
	if len(svc.removed) != 1 || svc.removed[0] != "sb-1" {
		t.Fatalf("removed = %v, want [sb-1]", svc.removed)
	}
}

func TestNewCRIClient_BadSocketDialsLazily(t *testing.T) {
	// grpc.NewClient does not connect eagerly, so construction succeeds even
	// for a nonexistent socket; Close must still be safe.
	c, err := NewCRIClient("/nonexistent/cri.sock")
	if err != nil {
		t.Fatalf("NewCRIClient: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}
