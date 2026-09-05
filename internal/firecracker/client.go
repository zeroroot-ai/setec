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

// Package firecracker is a narrow hand-rolled HTTP-over-Unix-socket
// client for the subset of the Firecracker REST API Phase 3 needs:
// PATCH /vm (for pause/resume) and PUT /snapshot/{create,load}. A
// dedicated package (rather than pulling in an upstream Go SDK) keeps
// the dependency surface tight and the API contract legible.
//
// The API shape follows the upstream Firecracker OpenAPI schema
// published at firecracker-microvm.github.io. Responses are generally
// 204 No Content on success; 4xx/5xx errors carry a JSON body with a
// fault_message field which we surface verbatim so operators can
// diagnose misconfigured VMs.
package firecracker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// Client is the narrow surface Phase 3 uses. Implementations speak to
// a single Firecracker API socket; callers construct a Client per
// microVM via NewClientFromSocket.
type Client interface {
	// Pause transitions the VM to the Paused state (CPUs halted, memory
	// preserved). Safe to call on an already-paused VM (Firecracker
	// returns a clear error the caller can ignore).
	Pause(ctx context.Context) error

	// Resume transitions a Paused VM back to Running.
	Resume(ctx context.Context) error

	// CreateSnapshot writes a Full-type snapshot to the provided
	// host paths. The VM MUST be Paused first (Firecracker enforces);
	// the node-agent pauses before calling this.
	CreateSnapshot(ctx context.Context, statePath, memPath string) error

	// LoadSnapshot restores a VM from the provided host paths. Called
	// on a freshly-started Firecracker process whose API is ready
	// but which has not yet been configured via the usual
	// /boot-source + /drives sequence.
	LoadSnapshot(ctx context.Context, statePath, memPath string) error
}

// httpClient carries the HTTP machinery plus an optional override
// clock for tests. It satisfies the Client interface.
type httpClient struct {
	http *http.Client
	// base is the scheme+host portion of the URL. Unix sockets
	// render as "http://firecracker"; the hostname is cosmetic.
	base string
}

// NewClientFromSocket returns a Client that talks to the Firecracker
// REST API exposed on the given Unix socket path.
func NewClientFromSocket(socketPath string) Client {
	return &httpClient{
		http: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					d := net.Dialer{Timeout: 5 * time.Second}
					return d.DialContext(ctx, "unix", socketPath)
				},
				// Keep the pool small; one FC socket == one VM.
				MaxIdleConns:    2,
				IdleConnTimeout: 30 * time.Second,
			},
			// Per-request timeout is controlled via the passed ctx;
			// we still provide a global cap so a truly hung socket
			// does not leak the goroutine.
			Timeout: 30 * time.Second,
		},
		base: "http://firecracker",
	}
}

// fcError decodes the standard Firecracker JSON error body so we can
// surface the fault_message verbatim in the returned error.
type fcError struct {
	FaultMessage string `json:"fault_message"`
}

// do serializes body, issues the HTTP request against the configured
// Unix socket, and returns a typed error on any non-2xx response. A
// 204 No Content is the common success case.
func (c *httpClient) do(ctx context.Context, method, path string, body any) error {
	var reader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("firecracker: marshal body: %w", err)
		}
		reader = bytes.NewReader(buf)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		return fmt.Errorf("firecracker: new request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("firecracker: %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}

	// Non-2xx: read up to 16 KiB of body and surface it. The upstream
	// error bodies are small; the cap is defense against a broken
	// proxy sending runaway output.
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 16*1024))
	var parsed fcError
	_ = json.Unmarshal(raw, &parsed)
	if parsed.FaultMessage != "" {
		return fmt.Errorf("firecracker: %s %s: %d %s: %s",
			method, path, resp.StatusCode, http.StatusText(resp.StatusCode), parsed.FaultMessage)
	}
	return fmt.Errorf("firecracker: %s %s: %d %s: %s",
		method, path, resp.StatusCode, http.StatusText(resp.StatusCode), bytes.TrimSpace(raw))
}

// Pause issues PATCH /vm {"state": "Paused"}.
func (c *httpClient) Pause(ctx context.Context) error {
	return c.do(ctx, http.MethodPatch, "/vm", map[string]string{"state": "Paused"})
}

// Resume issues PATCH /vm {"state": "Resumed"}.
func (c *httpClient) Resume(ctx context.Context) error {
	return c.do(ctx, http.MethodPatch, "/vm", map[string]string{"state": "Resumed"})
}

// CreateSnapshot issues PUT /snapshot/create with a Full snapshot
// specification. version is pinned to "1.0.0" — callers who want a
// different on-disk version should extend the client rather than
// hard-coding in the coordinator layer.
func (c *httpClient) CreateSnapshot(ctx context.Context, statePath, memPath string) error {
	body := map[string]any{
		"snapshot_type": "Full",
		"snapshot_path": statePath,
		"mem_file_path": memPath,
		"version":       "1.0.0",
	}
	return c.do(ctx, http.MethodPut, "/snapshot/create", body)
}

// LoadSnapshot issues PUT /snapshot/load and asks Firecracker to
// resume the VM on success (enable_diff_snapshots=false; Phase 3 uses
// full snapshots only).
func (c *httpClient) LoadSnapshot(ctx context.Context, statePath, memPath string) error {
	body := map[string]any{
		"snapshot_path":         statePath,
		"mem_file_path":         memPath,
		"enable_diff_snapshots": false,
		"resume_vm":             true,
	}
	return c.do(ctx, http.MethodPut, "/snapshot/load", body)
}
