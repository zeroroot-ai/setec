// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package credentials

import (
	"fmt"
	"io"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// reportInterval is the shortest time between two lines of the same kind in
// the default report of a Workload API failure. The go-spiffe client retries
// behind a backoff, so a failure that lasts would otherwise fill the log.
const reportInterval = 15 * time.Second

// noIdentityIssued says that the Workload API refused the workload because
// SPIRE has no registration entry for it yet. A Pod that starts before its
// entry exists sees this, and it ends when the entry exists.
func noIdentityIssued(err error) bool {
	return status.Code(err) == codes.PermissionDenied
}

// newWatchReporter returns the default report of a Workload API failure. It
// says in plain words that a refusal of an unregistered workload is a wait and
// not a fault, and it writes a line of each kind at most once for each
// reportInterval.
func newWatchReporter(w io.Writer, now func() time.Time) func(error) {
	var (
		mu   sync.Mutex
		last = map[bool]time.Time{}
	)
	return func(err error) {
		wait := noIdentityIssued(err)
		mu.Lock()
		at, seen := last[wait]
		t := now()
		quiet := seen && t.Sub(at) < reportInterval
		if !quiet {
			last[wait] = t
		}
		mu.Unlock()
		if quiet {
			return
		}
		if wait {
			fmt.Fprintf(w, "credentials: waiting for SPIRE to issue an identity to this workload "+
				"(no registration entry matches it yet). The wait ends when SPIRE registers the Pod: %v\n", err)
			return
		}
		fmt.Fprintf(w, "credentials: SPIFFE Workload API failure: %v\n", err)
	}
}
