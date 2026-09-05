// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package controller

import "os"

// envtestOptOut reports whether a failed envtest start may be downgraded from
// a package failure to a skip, and why (setec#302).
//
// The invariant being protected: a lane that is SUPPOSED to run the envtest
// suites must fail if they cannot run. Reporting `ok` with zero tests executed
// is indistinguishable from a pass, which is how this package's entire
// behavioural coverage — Phase 2/3, session lifecycle, pause timeouts, the
// invariant gate, runtime selection — could silently evaporate while CI stayed
// green.
//
// KUBEBUILDER_ASSETS is the signal for which lane this is:
//
//   - Set: this is the envtest lane. `make test` resolves it to an absolute
//     path and asserts an executable etcd exists before invoking go test, so
//     reaching here with it set means envtest genuinely failed to start — the
//     setec#302 case (a relative or stale path). Fatal.
//   - Unset: not the envtest lane. The org-wide `fast` tier
//     (reusable-go-ci.yml) runs a generic `go test ./...` and never installs
//     envtest binaries; failing there would block every PR on a lane that was
//     never meant to run these tests.
//
// Deliberately NOT a fallback to "skip whenever anything is wrong": the whole
// defect was that a broken envtest lane looked identical to a healthy one.
func envtestOptOut() (skip bool, why string) {
	if os.Getenv("SETEC_SKIP_ENVTEST") == "1" {
		return true, "SETEC_SKIP_ENVTEST=1 is set"
	}
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		return true, "KUBEBUILDER_ASSETS is unset, so this is not the envtest lane (run `make test` for that)"
	}
	return false, ""
}
