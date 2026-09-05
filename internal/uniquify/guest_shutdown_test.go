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

package uniquify

import (
	"context"
	"net"
	"path/filepath"
	"testing"
	"time"
)

// blockingIdentity parks the handler inside ApplyMachineID until released, so a
// handler can be kept demonstrably in-flight while Serve is asked to stop.
type blockingIdentity struct {
	entered  chan struct{}
	release  chan struct{}
	finished chan struct{}
}

func (b *blockingIdentity) ApplyMachineID(string) error {
	b.entered <- struct{}{}
	<-b.release
	close(b.finished)
	return nil
}
func (b *blockingIdentity) ApplyBootID(string) error   { return nil }
func (b *blockingIdentity) ApplyHostname(string) error { return nil }
func (b *blockingIdentity) Read() (string, string, string, error) {
	return "machine", "boot", "host", nil
}

// TestServe_WaitsForInFlightHandlers is the uniquify half of setec#319 — the
// same unsupervised-goroutine shape as internal/entropy.
//
// Serve's return must mean every handler has finished, because that is what
// every caller assumes. cmd/setec-guest-agent exits on the first errCh value,
// so without this a SIGTERM could cut a uniquify directive off mid-apply.
func TestServe_WaitsForInFlightHandlers(t *testing.T) {
	dir := t.TempDir()
	ln, err := net.Listen("unix", filepath.Join(dir, "uniquify.sock"))
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	id := &blockingIdentity{
		entered:  make(chan struct{}),
		release:  make(chan struct{}),
		finished: make(chan struct{}),
	}
	h := &GuestHandler{Identity: id, Network: &fakeNetwork{}, CID: fakeCID{cid: 5}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	served := make(chan struct{})
	go func() {
		defer close(served)
		_ = h.Serve(ctx, ln)
	}()

	conn, err := net.Dial("unix", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := WriteSpec(conn, Spec{MachineID: "abc"}); err != nil {
		t.Fatalf("WriteSpec: %v", err)
	}

	select {
	case <-id.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("handler never reached ApplyMachineID")
	}

	cancel()

	select {
	case <-served:
		t.Fatal("Serve returned while a handler was still in-flight; its return " +
			"must mean every handler has finished (setec#319)")
	case <-time.After(250 * time.Millisecond):
	}

	close(id.release)

	select {
	case <-id.finished:
	case <-time.After(5 * time.Second):
		t.Fatal("handler never finished after release")
	}
	select {
	case <-served:
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after its last handler finished")
	}
}
