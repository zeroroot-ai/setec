// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

// Package secretscan implements the "no secrets in a Snapshot" invariant
// (docs/design/threat-model.md). A Snapshot is shared across every warm-pool claim of a
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
	// nameGroup, when set, is the capture group of a name that must hold a
	// secret keyword as a whole segment (keywordSegment). 0 checks nothing.
	nameGroup int
	// valueGroup, when set, is the capture group of a value that must not
	// have the shape of a string table (stringTableValue). 0 checks nothing.
	valueGroup int
}

// formatString is a value that starts with a Go or C format verb, as the
// string table of a binary holds a format string ("%v\[0-9a-f]{1,6}").
var formatString = regexp.MustCompile(`^%[-+# 0]*\d*(?:\.\d+)?[A-Za-z]`)

// formatSign is a second sign of a format string or a pattern in a string
// table: a backslash, a bracket, a brace, or another verb. A password that
// starts with "%" and a letter has none of them (setec#287).
var formatSign = regexp.MustCompile(`[\\\[\]{}]|.%[-+# 0]*\d*(?:\.\d+)?[A-Za-z]`)

// labelList is a value made only of "name:" labels, as the string table of
// a binary holds field and option names side by side
// ("Names:Roles:Nodes:", "public-key:").
var labelList = regexp.MustCompile(`^(?:[A-Za-z][A-Za-z0-9_-]*:)+$`)

// stringTableValue reports whether a value has the shape of a string table
// and not of a secret: a format string, or a list of labels. A format string
// starts with a verb and has a second sign (formatSign), so a password that
// starts with "%" and a letter still counts. Any other run of characters is
// a value, so a password with punctuation still counts.
func stringTableValue(v string) bool {
	return formatString.MatchString(v) && formatSign.MatchString(v) || labelList.MatchString(v)
}

// secretKeywords are the name segments that read as a secret (D90).
var secretKeywords = map[string]bool{
	"secret": true, "secrets": true, "password": true, "passwords": true, "passwd": true,
	"token": true, "tokens": true, "credential": true, "credentials": true,
	"apikey": true, "privatekey": true,
	// A bare key segment or a name that ends with "key" (SSH_KEY,
	// SIGNING_KEY, encryptionKey) reads as a secret too (D90, amended). A
	// false match such as "monkey" fails the scan closed.
	"key": true, "keys": true,
}

// keywordSegment reports whether name holds a secret keyword as a whole
// segment (D90). Underscores, dashes and dots split a name, and so does a
// case change: "apiToken" is "api" and "token", "DBPassword" is "db" and
// "password". "secrettls" is one segment, and it is no keyword.
func keywordSegment(name string) bool {
	if keywordAtEnd(name) {
		return true
	}
	segs := nameSegments(name)
	for _, seg := range segs {
		if secretKeywords[seg] {
			return true
		}
	}
	return false
}

// keywordAtEnd reports whether name ends with a secret keyword, in any case
// and with or without separators (D90, amended): "MYSECRET", "DBPASSWORD",
// "APIKEY" and "GITHUBTOKEN" are secret names that the segment rule alone
// misses. "secrettls" does not end with a keyword. A false match fails the
// scan closed, and a missed secret does not, so the rule takes the false
// match.
func keywordAtEnd(name string) bool {
	flat := strings.ToLower(strings.NewReplacer("_", "", "-", "", ".", "").Replace(name))
	for kw := range secretKeywords {
		if strings.HasSuffix(flat, kw) {
			return true
		}
	}
	return false
}

// nameSegments splits name at underscores, dashes, dots and case changes,
// and returns the segments in lower case.
func nameSegments(name string) []string {
	var segs []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			segs = append(segs, strings.ToLower(string(cur)))
			cur = cur[:0]
		}
	}
	rs := []rune(name)
	for i, r := range rs {
		switch {
		case r == '_' || r == '-' || r == '.':
			flush()
			continue
		case isUpper(r) && i > 0 && (isLower(rs[i-1]) || isDigit(rs[i-1])):
			// "apiToken": a capital after a lower-case letter starts a segment.
			flush()
		case isUpper(r) && i > 0 && isUpper(rs[i-1]) && i+1 < len(rs) && isLower(rs[i+1]):
			// "DBPassword": the last capital of a run starts a segment.
			flush()
		}
		cur = append(cur, r)
	}
	flush()
	return segs
}

func isUpper(r rune) bool { return r >= 'A' && r <= 'Z' }
func isLower(r rune) bool { return r >= 'a' && r <= 'z' }
func isDigit(r rune) bool { return r >= '0' && r <= '9' }

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
		re:        regexp.MustCompile(`-{5}BEGIN (?:RSA |EC |DSA |OPENSSH |PGP )?PRIVATE KEY-{5}`),
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
		// An assignment whose name reads as a secret and whose value is a
		// non-trivial token: an environment variable ("DB_PASSWORD=..."), a
		// field of a struct or a JSON document ("apiToken=...",
		// "token: ..."). This is the shape secrets take when they are
		// (wrongly) baked into a snapshot rather than injected after the
		// restore.
		//
		// A memory image holds the string tables of the binaries in the
		// guest, where words of unrelated text sit side by side with no
		// separator ("read traffic secret" next to "tls: ..."). So a secret
		// keyword counts only as a whole segment of the name
		// (keywordSegment, D90): underscores, dashes, dots and case changes
		// split a name into segments. "SECRET_KEY", "DB_PASSWORD",
		// "apiToken" and "token" match; "secrettls" does not. The value is
		// any run of characters up to white space, a quote or a NUL, so a
		// password with punctuation still counts. Only the two shapes of a
		// string table are not a value (stringTableValue).
		name: "secret-shaped-env-assignment",
		re: regexp.MustCompile(`\b([A-Za-z0-9_.-]*(?i:secret|password|passwd|key|token|credential)[A-Za-z0-9_.-]*)` +
			`\s*[:=]\s*["']?([^\s"'\x00]{8,})`),
		maskGroup:  2,
		nameGroup:  1,
		valueGroup: 2,
	},
	{
		// A line of a config file (YAML, .env, INI, TOML) whose key reads
		// as a secret, in any case: "client_secret: ...", "db_password=...",
		// "Password=...". The page cache of a guest holds such files.
		//
		// The rule is case insensitive, so three conditions keep it off the
		// string tables of binaries: the key starts a line (after a newline,
		// not a NUL), the key ends with the secret word ("Password=" but not
		// "PasswordAuthentication="), and the value is token characters that
		// end at white space, a quote, a NUL or the line end ("%v\[0-9" is
		// not a value).
		name: "secret-shaped-config-assignment",
		re: regexp.MustCompile(`(?im)^[ \t]*(?:export[ \t]+)?` +
			`([a-z0-9_.-]*(?:secret|password|passwd|api[_-]?key|token|private[_-]?key|credentials?))` +
			`[ \t]*[:=][ \t]*["']?([a-z0-9_\-+/=.~]{8,})(?:["'\s\x00]|$)`),
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
				if g := rl.nameGroup; g > 0 && (m[2*g] < 0 || !keywordSegment(string(window[m[2*g]:m[2*g+1]]))) {
					continue
				}
				if g := rl.valueGroup; g > 0 && m[2*g] >= 0 && stringTableValue(string(window[m[2*g]:m[2*g+1]])) {
					continue
				}
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
			window := make([]byte, 0, len(carry)+n)
			window = append(window, carry...)
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
