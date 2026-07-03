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

package main

import (
	"bytes"
	"context"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/zeroroot-ai/setec/internal/entropy"
)

func TestParseFlags_Defaults(t *testing.T) {
	o, err := parseFlags(nil)
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if o.Port != entropy.DefaultVsockPort {
		t.Fatalf("port = %d, want %d", o.Port, entropy.DefaultVsockPort)
	}
	if o.RandomDevice != "/dev/urandom" {
		t.Fatalf("random device = %q", o.RandomDevice)
	}
}

func TestParseFlags_RejectsZeroPort(t *testing.T) {
	if _, err := parseFlags([]string{"--vsock-port", "0"}); err == nil {
		t.Fatal("port 0 must be rejected")
	}
}

// capturePool records injected entropy for the run-loop test.
type capturePool struct {
	mu       sync.Mutex
	payloads [][]byte
}

func (c *capturePool) AddEntropy(p []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.payloads = append(c.payloads, append([]byte(nil), p...))
	return nil
}

// TestRun_ServesReseedRequests drives the agent's run loop end-to-end
// over a Unix socket standing in for the vsock listener.
func TestRun_ServesReseedRequests(t *testing.T) {
	ln, err := net.Listen("unix", filepath.Join(t.TempDir(), "vsock-standin.sock"))
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	pool := &capturePool{}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- run(ctx, ln, pool, t.Logf) }()

	conn, err := net.Dial("unix", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	payload := bytes.Repeat([]byte{0x5A}, 128)
	if err := entropy.WriteRequest(conn, payload); err != nil {
		t.Fatalf("WriteRequest: %v", err)
	}
	ack, err := entropy.ReadAck(conn)
	if err != nil {
		t.Fatalf("ReadAck: %v", err)
	}
	if ack.Status != entropy.StatusOK {
		t.Fatalf("status = %d", ack.Status)
	}
	_ = conn.Close()

	pool.mu.Lock()
	got := len(pool.payloads)
	pool.mu.Unlock()
	if got != 1 {
		t.Fatalf("pool injections = %d, want 1", got)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("run did not stop on ctx cancel")
	}
}
