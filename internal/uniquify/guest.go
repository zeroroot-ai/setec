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
	"errors"
	"fmt"
	"net"
	"sync"
	"time"
)

// IdentityApplier applies the per-restore machine identity inside the
// guest. The production implementation (apply_linux.go) writes
// /etc/machine-id, bind-mounts a fresh boot_id over
// /proc/sys/kernel/random/boot_id, and calls sethostname(2); tests
// inject fakes. Every method must return a non-nil error when the
// change did not take effect — the handler acks StatusError in that
// case so the host fails the restore closed.
type IdentityApplier interface {
	ApplyMachineID(id string) error
	ApplyBootID(id string) error
	ApplyHostname(name string) error
	// Read returns the identity currently observable in the guest.
	// The handler reports these values back so the host verifies
	// what IS, not what was requested.
	Read() (machineID, bootID, hostname string, err error)
}

// NetworkReconciler reconciles the guest's primary interface to the
// expected CNI-assigned address and reports the addresses observed
// afterwards. expectedIP may be empty, in which case the reconciler
// only reports.
type NetworkReconciler interface {
	Reconcile(expectedIP string) (observed []string, err error)
}

// CIDReporter returns the guest's local vsock context id.
type CIDReporter interface {
	LocalCID() (uint32, error)
}

// guestConnBudget bounds a single uniquify exchange so a stalled peer
// cannot wedge the accept loop's goroutine forever.
const guestConnBudget = 10 * time.Second

// GuestHandler serves uniquification directives inside the guest. One
// directive is served per connection; the connection is closed after
// the report.
type GuestHandler struct {
	// Identity applies machine-id / boot-id / hostname. Required.
	Identity IdentityApplier

	// Network reconciles the primary interface address. Required.
	Network NetworkReconciler

	// CID reports the local vsock context id. Required.
	CID CIDReporter

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
			return fmt.Errorf("uniquify: accept: %w", err)
		}
		wg.Go(func() {
			if serveErr := h.ServeConn(conn); serveErr != nil {
				h.logf("uniquify request failed: %v", serveErr)
			}
		})
	}
}

// ServeConn reads one Spec from conn, applies it, and writes the
// Report. The report digest is always the SHA-256 of the received
// spec bytes; the status reflects whether every directive applied.
// conn is closed on return.
func (h *GuestHandler) ServeConn(conn net.Conn) error {
	defer func() { _ = conn.Close() }()
	if h.Identity == nil || h.Network == nil || h.CID == nil {
		return errors.New("uniquify: GuestHandler is missing an applier")
	}
	_ = conn.SetDeadline(time.Now().Add(guestConnBudget))

	spec, raw, err := ReadSpec(conn)
	if err != nil {
		return err
	}
	report := h.apply(spec)
	report.Digest = DigestHex(raw)

	if ackErr := WriteReport(conn, report); ackErr != nil {
		return ackErr
	}
	h.logf("uniquify: applied machine-id/boot-id/hostname (status=%d, cid=%d, ips=%v)",
		report.Status, report.GuestCID, report.ObservedIPs)
	if report.Status != StatusOK {
		return fmt.Errorf("uniquify: apply failed; acked StatusError: %s", report.Error)
	}
	return nil
}

// apply runs every directive and assembles the report. It never
// aborts early on an individual failure: the host needs the full
// observed picture, and the StatusError ack fails the restore closed
// regardless.
func (h *GuestHandler) apply(spec Spec) Report {
	report := Report{Status: StatusOK}
	fail := func(err error) {
		report.Status = StatusError
		if report.Error == "" {
			report.Error = err.Error()
		} else {
			report.Error += "; " + err.Error()
		}
	}

	if err := h.Identity.ApplyMachineID(spec.MachineID); err != nil {
		fail(fmt.Errorf("machine-id: %w", err))
	}
	if err := h.Identity.ApplyBootID(spec.BootID); err != nil {
		fail(fmt.Errorf("boot-id: %w", err))
	}
	if err := h.Identity.ApplyHostname(spec.Hostname); err != nil {
		fail(fmt.Errorf("hostname: %w", err))
	}

	observed, err := h.Network.Reconcile(spec.PodIP)
	if err != nil {
		fail(fmt.Errorf("network: %w", err))
	}
	report.ObservedIPs = observed

	machineID, bootID, hostname, err := h.Identity.Read()
	if err != nil {
		fail(fmt.Errorf("read identity: %w", err))
	}
	report.MachineID = machineID
	report.BootID = bootID
	report.Hostname = hostname

	cid, err := h.CID.LocalCID()
	if err != nil {
		fail(fmt.Errorf("local cid: %w", err))
	}
	report.GuestCID = cid

	return report
}

func (h *GuestHandler) logf(format string, args ...any) {
	if h.Logf != nil {
		h.Logf(format, args...)
	}
}
