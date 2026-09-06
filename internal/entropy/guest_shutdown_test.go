// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package entropy

import (
	"bytes"
	"context"
	"net"
	"path/filepath"
	"testing"
	"time"
)

// blockingPool holds AddEntropy inside the handler until the test releases it,
// so a handler can be kept demonstrably in-flight while Serve is asked to stop.
type blockingPool struct {
	entered  chan struct{} // closed-ish signal: handler has reached AddEntropy
	release  chan struct{} // test closes this to let the handler finish
	finished chan struct{} // handler closes this after AddEntropy returns
}

func (p *blockingPool) AddEntropy(_ []byte) error {
	p.entered <- struct{}{}
	<-p.release
	close(p.finished)
	return nil
}

// TestServe_WaitsForInFlightHandlers is the regression test for setec#319.
//
// Serve used to spawn unsupervised goroutines and return as soon as Accept
// failed after cancellation, so its return meant only "the accept loop
// stopped" — a handler could still be running. Every caller treats the return
// as "all work is done"; in the guest agent that means SIGTERM could cut a
// reseed injection off mid-flight, which for entropy is an ADR-0005 concern
// (the reseed is what stands between a restored clone and a duplicated RNG
// stream).
//
// The test pins the contract directly rather than through the symptom (a
// "Log in goroutine after test completed" panic, which only reproduces on a
// loaded machine): with a handler parked inside AddEntropy, Serve must NOT
// return, and must return once the handler is released.
func TestServe_WaitsForInFlightHandlers(t *testing.T) {
	dir := t.TempDir()
	ln, err := net.Listen("unix", filepath.Join(dir, "guest.sock"))
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	pool := &blockingPool{
		entered:  make(chan struct{}),
		release:  make(chan struct{}),
		finished: make(chan struct{}),
	}
	h := &GuestHandler{Pool: pool}

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
	if err := WriteRequest(conn, bytes.Repeat([]byte{7}, 32)); err != nil {
		t.Fatalf("WriteRequest: %v", err)
	}

	// Handler is now parked inside AddEntropy.
	select {
	case <-pool.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("handler never reached AddEntropy")
	}

	// Stop the accept loop while the handler is still in-flight.
	cancel()

	// THE ASSERTION: Serve must not return while a handler is live. Before the
	// fix it returned here, which is what let the handler outlive its caller.
	select {
	case <-served:
		t.Fatal("Serve returned while a handler was still in-flight; its return " +
			"must mean every handler has finished (setec#319)")
	case <-time.After(250 * time.Millisecond):
		// Still running, as required.
	}

	// Release the handler; now Serve may return.
	close(pool.release)

	select {
	case <-pool.finished:
	case <-time.After(5 * time.Second):
		t.Fatal("handler never finished after release")
	}
	select {
	case <-served:
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after its last handler finished")
	}
}
