// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package entropy

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"
)

// Pool is the guest-side sink fresh entropy is written into. The
// production implementation (KernelPool, pool_linux.go) issues the
// RNDADDENTROPY ioctl so the payload is both mixed into the kernel
// CRNG input pool and credited; tests inject fakes.
type Pool interface {
	// AddEntropy mixes p into the entropy pool and credits it. It
	// must return a non-nil error when the injection did not happen —
	// the handler acks StatusError in that case so the host fails
	// closed rather than assuming a reseed that never occurred.
	AddEntropy(p []byte) error
}

// guestConnBudget bounds a single reseed exchange so a stalled peer
// cannot wedge the accept loop's goroutine forever.
const guestConnBudget = 10 * time.Second

// GuestHandler serves reseed requests inside the guest. One request is
// served per connection; the connection is closed after the ack.
type GuestHandler struct {
	// Pool receives the injected entropy. Required.
	Pool Pool

	// Logf, when non-nil, receives one line per served request.
	Logf func(format string, args ...any)
}

// Serve accepts connections from ln until ctx is cancelled, serving
// each with ServeConn on its own goroutine.
//
// Serve does not return until every handler it started has finished. The
// handlers used to be unsupervised, so a return meant only "the accept loop
// stopped" — callers that treat it as "all work is done" (every caller does)
// could observe a handler still running afterwards. In the guest agent that
// means SIGTERM can cut a reseed injection or a uniquify directive off
// mid-flight, and for entropy that is an ADR-0005 concern: the reseed is what
// stands between a restored clone and a duplicated RNG stream, so "mostly
// completes before shutdown" is not the guarantee the invariant needs
// (setec#319).
func (h *GuestHandler) Serve(ctx context.Context, ln net.Listener) error {
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	// Every handler is tracked, and Serve waits for all of them on the way
	// out — via defer, so BOTH return paths below (clean shutdown and accept
	// error) wait rather than only the happy one.
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil //nolint:nilerr // Accept fails because we closed ln on ctx cancel: clean shutdown, not an error.
			}
			return fmt.Errorf("entropy: accept: %w", err)
		}
		wg.Go(func() {
			if serveErr := h.ServeConn(conn); serveErr != nil {
				h.logf("entropy reseed request failed: %v", serveErr)
			}
		})
	}
}

// ServeConn reads one reseed request from conn, injects the payload
// into the Pool, and writes the ack. The ack digest is always the
// SHA-256 of the received payload; the status reflects whether the
// injection succeeded. conn is closed on return.
func (h *GuestHandler) ServeConn(conn net.Conn) error {
	defer func() { _ = conn.Close() }()
	if h.Pool == nil {
		return errors.New("entropy: GuestHandler has no Pool")
	}
	_ = conn.SetDeadline(time.Now().Add(guestConnBudget))

	payload, err := ReadRequest(conn)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(payload)

	status := StatusOK
	if injectErr := h.Pool.AddEntropy(payload); injectErr != nil {
		status = StatusError
		h.logf("entropy injection failed: %v", injectErr)
	}
	if ackErr := WriteAck(conn, status, digest); ackErr != nil {
		return ackErr
	}
	h.logf("reseed: injected %d bytes (status=%d)", len(payload), status)
	if status != StatusOK {
		return errors.New("entropy: injection failed; acked StatusError")
	}
	return nil
}

func (h *GuestHandler) logf(format string, args ...any) {
	if h.Logf != nil {
		h.Logf(format, args...)
	}
}
