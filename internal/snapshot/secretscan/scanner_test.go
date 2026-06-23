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

package secretscan

import (
	"bytes"
	"crypto/rand"
	"strings"
	"testing"
)

func ruleSet(fs []Finding) map[string]bool {
	out := map[string]bool{}
	for _, f := range fs {
		out[f.Rule] = true
	}
	return out
}

func TestScan_DetectsHighSignalSecrets(t *testing.T) {
	cases := []struct {
		name string
		body string
		rule string
	}{
		{"pem", "noise\n-----BEGIN RSA PRIVATE KEY-----\nMIIabc\n", "pem-private-key"},
		{"pem-openssh", "-----BEGIN OPENSSH PRIVATE KEY-----", "pem-private-key"},
		{"aws-id", "export AWS=AKIAIOSFODNN7EXAMPLE here", "aws-access-key-id"},
		{"aws-secret", `aws_secret_access_key = wJalrXUtnFEMIabc1234567890abcdEXAMPLEKEY12`, "aws-secret-access-key"},
		{"github", "token ghp_1234567890abcdefghijABCDEFGH found", "github-token"},
		{"slack", "xoxb-1234567890-abcdefghijklm token", "slack-token"},
		{"stripe", "key sk_live_abcdEFGH1234567890wxyz here", "stripe-secret-key"},
		{"jwt", "auth eyJhbGciOiJ.eyJzdWIiOiIx.SflKxwRJSMeKKF2QT4", "jwt"},
		{"env-secret", "DATABASE_PASSWORD=hunter2hunter2", "secret-shaped-env-assignment"},
		{"env-apikey", `MY_API_KEY: "sometoken12345"`, "secret-shaped-env-assignment"},
	}
	s := New()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs, err := s.Scan(strings.NewReader(tc.body))
			if err != nil {
				t.Fatalf("scan: %v", err)
			}
			if !ruleSet(fs)[tc.rule] {
				t.Fatalf("expected rule %q to fire on %q; got %v", tc.rule, tc.body, fs)
			}
		})
	}
}

func TestScan_RedactsSecretBody(t *testing.T) {
	s := New()
	fs, err := s.Scan(strings.NewReader("export TOKEN=supersecretvalue123"))
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(fs) == 0 {
		t.Fatal("expected a finding")
	}
	for _, f := range fs {
		if strings.Contains(f.Excerpt, "supersecretvalue123") {
			t.Fatalf("excerpt re-leaked the secret: %q", f.Excerpt)
		}
		if !strings.Contains(f.Excerpt, "REDACTED") {
			t.Fatalf("excerpt missing redaction marker: %q", f.Excerpt)
		}
	}
}

func TestScan_CleanInputHasNoFindings(t *testing.T) {
	s := New()
	clean := "the quick brown fox jumps over the lazy dog\nPORT=8080\nLOG_LEVEL=info\n"
	fs, err := s.Scan(strings.NewReader(clean))
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(fs) != 0 {
		t.Fatalf("expected no findings on clean input, got %v", fs)
	}
}

// TestScan_RandomMemoryFillIsNotFlagged guards the core false-positive
// concern: a memory snapshot is mostly random bytes. The conservative rule
// set must not fire on raw entropy.
func TestScan_RandomMemoryFillIsNotFlagged(t *testing.T) {
	s := New()
	buf := make([]byte, 4<<20) // 4 MiB of random fill
	if _, err := rand.Read(buf); err != nil {
		t.Fatalf("rand: %v", err)
	}
	fs, err := s.Scan(bytes.NewReader(buf))
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(fs) != 0 {
		t.Fatalf("random memory fill produced %d false-positive findings: %v", len(fs), fs)
	}
}

// TestScan_TokenSpanningChunkBoundary ensures a secret straddling the
// internal scan-window boundary is still detected (overlap carry works).
func TestScan_TokenSpanningChunkBoundary(t *testing.T) {
	s := New()
	// Place a PEM header so it begins a few bytes before a maxLineBytes
	// boundary, forcing it to span two internal windows.
	const secret = "-----BEGIN RSA PRIVATE KEY-----"
	pad := bytes.Repeat([]byte("a"), maxLineBytes-10)
	body := append(append([]byte{}, pad...), []byte(secret)...)
	fs, err := s.Scan(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if !ruleSet(fs)["pem-private-key"] {
		t.Fatalf("secret spanning chunk boundary was not detected: %v", fs)
	}
}

func TestScan_DeduplicatesIdenticalFindings(t *testing.T) {
	s := New()
	line := "-----BEGIN RSA PRIVATE KEY-----"
	body := strings.Repeat(line+"\n", 50)
	fs, err := s.Scan(strings.NewReader(body))
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	count := 0
	for _, f := range fs {
		if f.Rule == "pem-private-key" {
			count++
		}
	}
	// 50 identical lines must not produce 50 findings; dedup collapses
	// identical (rule, excerpt) pairs. Boundary windows yield a small
	// constant, never proportional to input size.
	if count == 0 || count > 5 {
		t.Fatalf("expected de-duplicated findings (1..5), got %d for 50 identical lines", count)
	}
}
