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

// Package secretscan implements the "no secrets in a Snapshot" invariant
// (ADR-0052). A Snapshot is shared across every warm-pool claim of a
// SandboxClass, so any secret baked into snapshot state would leak to every
// future tenant that restores it. The architectural rule is therefore:
// secrets are injected per-lease POST-restore over the control plane, NEVER
// present at snapshot time.
//
// This package is the enforcement half of that rule. It scans a byte stream
// (a snapshot artifact, or any candidate payload the snapshot builder is
// about to persist) for secret-shaped material and reports every match. The
// scanner is pure and stream-oriented: it holds at most a bounded window in
// memory regardless of input size, so it is safe to run against multi-GiB
// memory snapshots.
//
// The scanner is deliberately conservative about false positives: it matches
// high-signal token shapes (provider key prefixes, PEM private-key headers,
// JWTs, declared env-var assignments with secret-shaped names) rather than
// generic high-entropy heuristics, which would flag the random bytes that
// legitimately fill a memory image. The goal is a CI gate that fails loudly
// on a real leak without crying wolf on every snapshot.
package secretscan

import (
	"bufio"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
)

// Finding describes a single secret-shaped match in the scanned stream.
type Finding struct {
	// Rule is the stable identifier of the detector that fired (e.g.
	// "pem-private-key", "aws-access-key-id"). Stable so CI output and
	// allowlists can key on it.
	Rule string

	// Offset is the byte offset within the stream at which the match
	// begins. Approximate to the start of the containing window for
	// multi-line rules; exact for single-line rules.
	Offset int64

	// Excerpt is a short, redacted snippet around the match suitable for
	// surfacing in CI logs without re-leaking the secret. The matched
	// secret body is masked; only enough context to locate the finding
	// is retained.
	Excerpt string
}

// String renders a Finding as a single CI-friendly line.
func (f Finding) String() string {
	return fmt.Sprintf("%s at offset %d: %s", f.Rule, f.Offset, f.Excerpt)
}

// rule is one secret detector: a compiled pattern plus the index of the
// capture group holding the sensitive body to mask (0 == mask the whole
// match).
type rule struct {
	name      string
	re        *regexp.Regexp
	maskGroup int
}

// builtinRules is the default detector set. Each pattern targets a
// high-signal, low-false-positive secret shape. The set is intentionally
// small and auditable; extend it deliberately rather than reaching for a
// generic entropy heuristic.
//
// Patterns operate on a single logical line. Multi-line constructs (PEM
// blocks) are detected by their unambiguous header line, which is sufficient
// for a leak gate — the header alone never appears in legitimate random
// memory fill.
var builtinRules = []rule{
	{
		name:      "pem-private-key",
		re:        regexp.MustCompile(`-----BEGIN (?:RSA |EC |DSA |OPENSSH |PGP )?PRIVATE KEY-----`),
		maskGroup: 0,
	},
	{
		name:      "aws-access-key-id",
		re:        regexp.MustCompile(`\b((?:AKIA|ASIA|AIDA|AROA)[0-9A-Z]{16})\b`),
		maskGroup: 1,
	},
	{
		name:      "aws-secret-access-key",
		re:        regexp.MustCompile(`(?i)aws_?secret_?access_?key["'\s:=]{1,4}([0-9A-Za-z/+]{40})`),
		maskGroup: 1,
	},
	{
		name:      "gcp-service-account-key",
		re:        regexp.MustCompile(`"private_key_id"\s*:\s*"[0-9a-f]{40}"`),
		maskGroup: 0,
	},
	{
		name:      "github-token",
		re:        regexp.MustCompile(`\b((?:ghp|gho|ghu|ghs|ghr|github_pat)_[0-9A-Za-z_]{20,})\b`),
		maskGroup: 1,
	},
	{
		name:      "slack-token",
		re:        regexp.MustCompile(`\b(xox[baprs]-[0-9A-Za-z-]{10,})\b`),
		maskGroup: 1,
	},
	{
		name:      "stripe-secret-key",
		re:        regexp.MustCompile(`\b((?:sk|rk)_(?:live|test)_[0-9A-Za-z]{16,})\b`),
		maskGroup: 1,
	},
	{
		name:      "openai-api-key",
		re:        regexp.MustCompile(`\b(sk-[0-9A-Za-z_-]{20,})\b`),
		maskGroup: 1,
	},
	{
		name:      "jwt",
		re:        regexp.MustCompile(`\beyJ[0-9A-Za-z_-]{8,}\.eyJ[0-9A-Za-z_-]{8,}\.[0-9A-Za-z_-]{8,}\b`),
		maskGroup: 0,
	},
	{
		// A declared environment-variable assignment whose name reads as a
		// secret and whose value is a non-trivial token. This is the shape
		// secrets take when they are (wrongly) baked into a snapshot via the
		// container env rather than injected post-restore.
		name:      "secret-shaped-env-assignment",
		re:        regexp.MustCompile(`(?i)\b([A-Z0-9_]*(?:SECRET|PASSWORD|PASSWD|API[_-]?KEY|TOKEN|PRIVATE[_-]?KEY|CREDENTIAL)[A-Z0-9_]*)\s*[:=]\s*["']?([^\s"']{8,})`),
		maskGroup: 2,
	},
}

// Scanner detects secret-shaped material in a byte stream. The zero value is
// not usable; construct one with New.
type Scanner struct {
	rules []rule
	// maxExcerpt bounds the length of redacted context retained per finding.
	maxExcerpt int
}

// New returns a Scanner using the builtin detector set.
func New() *Scanner {
	return &Scanner{rules: builtinRules, maxExcerpt: 80}
}

// maxLineBytes bounds the per-line buffer the scanner is willing to hold.
// Snapshot memory images are mostly binary with no newlines, so a single
// "line" could otherwise be gigabytes. We cap the scan window and advance in
// fixed chunks past the cap; secret tokens are far shorter than this bound,
// so capping never splits a real token in a way that hides it (we overlap
// windows to be safe).
const maxLineBytes = 1 << 20 // 1 MiB

// windowOverlap is carried between binary chunks so a token straddling a
// chunk boundary is still matched. It must exceed the longest token any rule
// can match; 512 bytes comfortably covers JWTs and PEM headers.
const windowOverlap = 512

// Scan reads r to completion and returns every secret-shaped finding. It is
// stream-oriented and memory-bounded: it never buffers more than
// maxLineBytes + windowOverlap at once. Findings are de-duplicated by
// (rule, excerpt) and returned in offset order.
func (s *Scanner) Scan(r io.Reader) ([]Finding, error) {
	br := bufio.NewReaderSize(r, 64*1024)

	var (
		findings []Finding
		seen     = map[string]struct{}{}
		offset   int64
		carry    []byte
	)

	emit := func(window []byte, base int64) {
		for _, rl := range s.rules {
			for _, m := range rl.re.FindAllSubmatchIndex(window, -1) {
				start := int64(m[0]) + base
				f := Finding{
					Rule:    rl.name,
					Offset:  start,
					Excerpt: redact(window, m, rl.maskGroup, s.maxExcerpt),
				}
				key := f.Rule + "\x00" + f.Excerpt
				if _, dup := seen[key]; dup {
					continue
				}
				seen[key] = struct{}{}
				findings = append(findings, f)
			}
		}
	}

	buf := make([]byte, maxLineBytes)
	for {
		n, err := io.ReadFull(br, buf)
		if n > 0 {
			window := append(carry[:0:0], carry...)
			window = append(window, buf[:n]...)
			// base is the stream offset of window[0].
			base := offset - int64(len(carry))
			emit(window, base)

			offset += int64(n)

			// Carry the tail so a token spanning this chunk and the next is
			// still seen. Overlap is bounded and far smaller than the chunk.
			if len(window) > windowOverlap {
				carry = append(carry[:0], window[len(window)-windowOverlap:]...)
			} else {
				carry = append(carry[:0], window...)
			}
		}
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("secretscan: read: %w", err)
		}
	}

	sort.SliceStable(findings, func(i, j int) bool {
		if findings[i].Offset != findings[j].Offset {
			return findings[i].Offset < findings[j].Offset
		}
		return findings[i].Rule < findings[j].Rule
	})
	return findings, nil
}

// redact builds a short, safe excerpt around match m, masking the sensitive
// capture group so CI logs never re-leak the secret they are reporting.
func redact(window []byte, m []int, maskGroup, maxExcerpt int) string {
	matchStart, matchEnd := m[0], m[1]

	// Resolve the byte span to mask. maskGroup 0 masks the whole match;
	// otherwise mask the named capture group when present.
	maskStart, maskEnd := matchStart, matchEnd
	if maskGroup > 0 && len(m) > 2*maskGroup+1 && m[2*maskGroup] >= 0 {
		maskStart, maskEnd = m[2*maskGroup], m[2*maskGroup+1]
	}

	// Take a bit of context on each side of the match, clamped to the window.
	ctx := 12
	lo := max(matchStart-ctx, 0)
	hi := min(matchEnd+ctx, len(window))

	var b strings.Builder
	for i := lo; i < hi; i++ {
		if i >= maskStart && i < maskEnd {
			if i == maskStart {
				b.WriteString("***REDACTED***")
			}
			continue
		}
		c := window[i]
		if c < 0x20 || c > 0x7e {
			b.WriteByte('.')
		} else {
			b.WriteByte(c)
		}
	}
	out := strings.TrimSpace(b.String())
	if len(out) > maxExcerpt {
		out = out[:maxExcerpt] + "…"
	}
	return out
}
