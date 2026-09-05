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

// Command setec-snapshot-scan is the "no secrets in a Snapshot" CI gate
// (ADR-0052). It scans a snapshot artifact (a file) or a directory of
// snapshot artifacts for secret-shaped material and exits non-zero if any is
// found. A Snapshot is shared across every warm-pool claim, so a secret baked
// into snapshot state would leak to every future tenant — this gate fails the
// build before such an artifact can ship.
//
// Usage:
//
//	setec-snapshot-scan <path> [<path>...]
//
// Exit codes:
//
//	0  no secret-shaped material found
//	1  secrets found (the leak is printed, redacted)
//	2  usage / I/O error
package main

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/zeroroot-ai/setec/internal/snapshot/secretscan"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// printf writes to w, deliberately ignoring the write error: the only sinks
// are stdout/stderr, where a failed write is unrecoverable and not worth
// threading an error through a CLI.
func printf(w io.Writer, format string, a ...any) {
	_, _ = fmt.Fprintf(w, format, a...)
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printf(stderr, "usage: setec-snapshot-scan <path> [<path>...]\n")
		printf(stderr, "scans snapshot artifacts for secret-shaped material; exits non-zero on any finding\n")
		return 2
	}

	var (
		allFindings []secretscan.PathFinding
		leaked      bool
	)
	for _, path := range args {
		findings, err := secretscan.ScanPath(path)
		switch {
		case errors.Is(err, secretscan.ErrSecretsFound):
			leaked = true
			allFindings = append(allFindings, findings...)
		case err != nil:
			printf(stderr, "setec-snapshot-scan: %v\n", err)
			return 2
		}
	}

	if leaked {
		printf(stderr, "FAIL: no-secrets-in-snapshot gate found %d secret-shaped finding(s):\n", len(allFindings))
		for _, f := range allFindings {
			printf(stderr, "  %s: %s\n", f.Path, f.Finding.String())
		}
		printf(stderr, "\n"+
			"A Snapshot is shared across every warm-pool claim. Secrets MUST be injected\n"+
			"per-lease POST-restore, never baked into snapshot state. Remove the secret\n"+
			"from the snapshot source and inject it after restore instead.\n")
		return 1
	}

	printf(stdout, "OK: no-secrets-in-snapshot gate scanned %d path(s); no secret-shaped material found\n", len(args))
	return 0
}
