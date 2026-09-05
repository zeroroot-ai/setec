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

package firecracker

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// handler describes a single expected HTTP exchange. The test server
// matches each incoming request against the head of the queue,
// asserts method/path, optionally inspects the body, and replies with
// the configured status + body.
type handler struct {
	method     string
	path       string
	status     int
	body       string
	assertBody func(t *testing.T, raw []byte)
}

// startUnixServer spins up an http.Server on a Unix socket in
// t.TempDir and dispatches through the given handler queue. The
// returned socket path is what the caller hands to
// NewClientFromSocket.
func startUnixServer(t *testing.T, handlers []handler) string {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "fc.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	i := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if i >= len(handlers) {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusTeapot)
			return
		}
		h := handlers[i]
		i++
		if r.Method != h.method || r.URL.Path != h.path {
			t.Errorf("req %d: got %s %s, want %s %s", i-1, r.Method, r.URL.Path, h.method, h.path)
		}
		if h.assertBody != nil {
			raw, _ := io.ReadAll(r.Body)
			h.assertBody(t, raw)
		}
		w.WriteHeader(h.status)
		if h.body != "" {
			_, _ = w.Write([]byte(h.body))
		}
	})

	srv := &http.Server{
		Handler:     mux,
		ReadTimeout: 2 * time.Second,
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		_ = srv.Close()
		_ = ln.Close()
	})
	return sock
}

func TestPauseSuccess(t *testing.T) {
	sock := startUnixServer(t, []handler{{
		method: http.MethodPatch, path: "/vm", status: http.StatusNoContent,
		assertBody: func(t *testing.T, raw []byte) {
			var m map[string]string
			if err := json.Unmarshal(raw, &m); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if m["state"] != "Paused" {
				t.Fatalf("state = %q, want Paused", m["state"])
			}
		},
	}})
	c := NewClientFromSocket(sock)
	if err := c.Pause(context.Background()); err != nil {
		t.Fatalf("Pause: %v", err)
	}
}

func TestResumeSuccess(t *testing.T) {
	sock := startUnixServer(t, []handler{{
		method: http.MethodPatch, path: "/vm", status: http.StatusNoContent,
		assertBody: func(t *testing.T, raw []byte) {
			var m map[string]string
			_ = json.Unmarshal(raw, &m)
			if m["state"] != "Resumed" {
				t.Fatalf("state = %q, want Resumed", m["state"])
			}
		},
	}})
	c := NewClientFromSocket(sock)
	if err := c.Resume(context.Background()); err != nil {
		t.Fatalf("Resume: %v", err)
	}
}

func TestCreateSnapshotSuccess(t *testing.T) {
	sock := startUnixServer(t, []handler{{
		method: http.MethodPut, path: "/snapshot/create", status: http.StatusNoContent,
		assertBody: func(t *testing.T, raw []byte) {
			var m map[string]any
			_ = json.Unmarshal(raw, &m)
			if m["snapshot_type"] != "Full" {
				t.Fatalf("snapshot_type = %v", m["snapshot_type"])
			}
			if m["snapshot_path"] != "/tmp/s.bin" {
				t.Fatalf("snapshot_path = %v", m["snapshot_path"])
			}
			if m["mem_file_path"] != "/tmp/m.bin" {
				t.Fatalf("mem_file_path = %v", m["mem_file_path"])
			}
			if m["version"] != "1.0.0" {
				t.Fatalf("version = %v", m["version"])
			}
		},
	}})
	c := NewClientFromSocket(sock)
	if err := c.CreateSnapshot(context.Background(), "/tmp/s.bin", "/tmp/m.bin"); err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
}

func TestLoadSnapshotSuccess(t *testing.T) {
	sock := startUnixServer(t, []handler{{
		method: http.MethodPut, path: "/snapshot/load", status: http.StatusNoContent,
		assertBody: func(t *testing.T, raw []byte) {
			var m map[string]any
			_ = json.Unmarshal(raw, &m)
			if m["enable_diff_snapshots"] != false {
				t.Fatalf("enable_diff_snapshots = %v", m["enable_diff_snapshots"])
			}
			if m["resume_vm"] != true {
				t.Fatalf("resume_vm = %v", m["resume_vm"])
			}
		},
	}})
	c := NewClientFromSocket(sock)
	if err := c.LoadSnapshot(context.Background(), "/tmp/s.bin", "/tmp/m.bin"); err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
}

func TestPauseErrorBodySurfaced(t *testing.T) {
	sock := startUnixServer(t, []handler{{
		method: http.MethodPatch, path: "/vm", status: http.StatusBadRequest,
		body: `{"fault_message":"invalid state transition"}`,
	}})
	c := NewClientFromSocket(sock)
	err := c.Pause(context.Background())
	if err == nil || !strings.Contains(err.Error(), "invalid state transition") {
		t.Fatalf("expected fault_message in error, got %v", err)
	}
}

func TestCreateSnapshotServerError(t *testing.T) {
	sock := startUnixServer(t, []handler{{
		method: http.MethodPut, path: "/snapshot/create", status: http.StatusInternalServerError,
		body: `{"fault_message":"out of memory"}`,
	}})
	c := NewClientFromSocket(sock)
	err := c.CreateSnapshot(context.Background(), "/s", "/m")
	if err == nil || !strings.Contains(err.Error(), "out of memory") {
		t.Fatalf("expected out of memory in error, got %v", err)
	}
}

func TestErrorWithoutFaultMessage(t *testing.T) {
	sock := startUnixServer(t, []handler{{
		method: http.MethodPatch, path: "/vm", status: http.StatusBadRequest,
		body: "plain-text-not-json",
	}})
	c := NewClientFromSocket(sock)
	err := c.Pause(context.Background())
	if err == nil || !strings.Contains(err.Error(), "plain-text-not-json") {
		t.Fatalf("expected raw body in error, got %v", err)
	}
}

func TestContextCancelledBeforeRequest(t *testing.T) {
	sock := startUnixServer(t, nil)
	c := NewClientFromSocket(sock)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.Pause(ctx); err == nil {
		t.Fatalf("expected context error, got nil")
	}
}

func TestDialFailureSurfaced(t *testing.T) {
	// No server on this socket.
	c := NewClientFromSocket(filepath.Join(t.TempDir(), "missing.sock"))
	if err := c.Pause(context.Background()); err == nil {
		t.Fatalf("expected dial error")
	}
}
