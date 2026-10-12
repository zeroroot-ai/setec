// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

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

// TestScan_SecretKeywordIsAWholeSegment is D90: a secret keyword counts
// only as a whole segment of the name. Underscores, dashes, dots and case
// changes split a name. The names of the decision match in each case; a
// keyword inside a longer word does not.
func TestScan_SecretKeywordIsAWholeSegment(t *testing.T) {
	s := New()
	match := []string{
		"SECRET_KEY=Zx9fQ2mPaa71",
		"DB_PASSWORD=Zx9fQ2mPaa71",
		"apiToken=Zx9fQ2mPaa71",
		"\x00token: Zx9fQ2mPaa71\x00",
		"DBPassword=Zx9fQ2mPaa71",
		"client.secret=Zx9fQ2mPaa71",
		"x-api-key: Zx9fQ2mPaa71",
		"MY_PRIVATE_KEY=Zx9fQ2mPaa71",
		"aws.credentials: Zx9fQ2mPaa71",
		// "Password" is a whole segment of this name, so it matches (D90).
		"PasswordAuthentication=nofailed",
		// A password with punctuation is a value.
		"DB_PASSWORD=P@ssw0rd!2024",
		"SECRET_KEY=a#b$c%d^e&f*",
		"apiToken=P@ss:w0rd(2024)",
		"\x00token: abc:def:123!\x00",
		// A name that ends with a keyword (D90, amended).
		"MYSECRET=Zx9fQ2mPaa71",
		"DBPASSWORD=Zx9fQ2mPaa71",
		"APIKEY=Zx9fQ2mPaa71",
		"GITHUBTOKEN=Zx9fQ2mPaa71",
		"\x00apikey: Zx9fQ2mPaa71\x00",
		// A bare key with a secret-shaped value (D90, amended).
		"SSH_KEY=AAAAB3NzaC1yc2EAAAADAQABAAAB",
		"STRIPE_KEY=sk_live_abc",
		"SIGNING_KEY=9f8e7d6c5b4a39281706f5e4d3c2b1a0",
		"\x00encryptionKey: ghp_x1y2z3\x00",
		"DEPLOY_KEY=ssh-ed25519 AAAAC3NzaC1lZDI1NTE5",
		// A generated password that starts with "%" and a letter (setec#287).
		"DB_PASSWORD=%Kx9!q2Lm",
	}
	for _, body := range match {
		fs, err := s.Scan(strings.NewReader(body))
		if err != nil {
			t.Fatalf("scan: %v", err)
		}
		if !ruleSet(fs)["secret-shaped-env-assignment"] {
			t.Errorf("%q: want secret-shaped-env-assignment, got %v", body, fs)
		}
	}
	noMatch := []string{
		"failed to read traffic secrettls: received unexpected error",
		"\x00secrettls: Zx9fQ2mPaa71\x00",
		"tokenizer=Zx9fQ2mPaa71",
		"passwordless=Zx9fQ2mPaa71",
		"MYSECRETVALUE=Zx9fQ2mPaa71",
		"secrettls: Zx9fQ2mPaa71",
		// A bare key with a value that is not secret-shaped: the kernel
		// keyring text of the guest memory (setec#295 run 38229444253).
		"\x00trusted_key: instantiate\x00",
		"\x00encrypted key: decrypting\x00",
		"KEY_ID=12",
		"SSH_KEY=Zx9fQ2mPaa71",
		"\x003trusted_key: unsealing key: instantiate\x00",
		"keyspace: Zx9fQ2mPaa71",
		"apikeyring=Zx9fQ2mPaa71",
	}
	for _, body := range noMatch {
		fs, err := s.Scan(strings.NewReader(body))
		if err != nil {
			t.Fatalf("scan: %v", err)
		}
		if ruleSet(fs)["secret-shaped-env-assignment"] {
			t.Errorf("%q: a keyword inside a longer word matched: %v", body, fs)
		}
	}
}

// TestNameSegments pins the split of a name (D90).
func TestStringTableValue(t *testing.T) {
	for v, want := range map[string]bool{
		`%v\[0-9a-f]{1,6}`:    true,
		`%-08.3f%s`:           true,
		"%Kx9!q2Lm":           false,
		"%sAbcdefgh":          false,
		"Names:Roles:Nodes:":  true,
		"public-key:":         true,
		"P@ssw0rd!2024":       false,
		"a#b$c%d^e&f*":        false,
		"abc:def:123!":        false,
		"Zx9fQ2mPaa71":        false,
		"user:pass@host:5432": false,
		"100%secret":          false,
	} {
		if got := stringTableValue(v); got != want {
			t.Errorf("stringTableValue(%q) = %v, want %v", v, got, want)
		}
	}
}

// TestSecretShapedValue pins the value check of a bare key name (D90,
// amended): a known secret prefix, or 16 or more token characters with a
// letter and a digit.
func TestSecretShapedValue(t *testing.T) {
	for v, want := range map[string]bool{
		"sk_live_abc":                      true,
		"rk_x":                             true,
		"AKIAABC":                          true,
		"-----BEGIN":                       true,
		"ssh-rsa AAAAB3":                   true,
		"9f8e7d6c5b4a39281706f5e4d3c2b1a0": true,
		"Zx9fQ2mPaa71Zx9f":                 true,
		"Zx9fQ2mPaa71Zx9":                  false, // 15 characters
		"instantiateinstantiate":           false, // no digit
		"1234567890123456":                 false, // no letter
		"decrypting":                       false,
		"Zx9fQ2mP aa71Zx9fQ2":              false, // white space ends the value
	} {
		value, _, _ := strings.Cut(v, " ")
		if got := secretShapedValue([]byte(v), 0, value); got != want {
			t.Errorf("secretShapedValue(%q) = %v, want %v", v, got, want)
		}
	}
}

func TestNameSegments(t *testing.T) {
	for name, want := range map[string]string{
		"SECRET_KEY":             "secret key",
		"apiToken":               "api token",
		"DBPassword":             "db password",
		"client.secret-value":    "client secret value",
		"secrettls":              "secrettls",
		"PasswordAuthentication": "password authentication",
		"aws2Token":              "aws2 token",
	} {
		if got := strings.Join(nameSegments(name), " "); got != want {
			t.Errorf("nameSegments(%q) = %q, want %q", name, got, want)
		}
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

// TestScan_BinaryStringTablesAreNotFlagged guards the false positive that
// refused every base of the warm pool in the nightly launcher run: a memory
// image holds the string tables of the binaries in the guest. Lower-case text
// in those tables is not an environment variable assignment.
func TestScan_BinaryStringTablesAreNotFlagged(t *testing.T) {
	s := New()
	tables := []string{
		// The excerpt of the finding in the nightly run (setec#253).
		"\x00\x01failed to read traffic secrettls: received unexpected error: did not\x00",
		// The shapes of the string tables of common binaries.
		"\x00private-key: public-key:\x00",
		"\x00token: %v\\[0-9a-f]{1,6}\x00",
		"\ntoken: %v\\[0-9a-f]{1,6}\n",
		"Token:Names:Roles:Nodes:Force:Tasks:SecretConfigVolumeSince:Attrs:Close:",
	}
	for _, table := range tables {
		fs, err := s.Scan(strings.NewReader(table))
		if err != nil {
			t.Fatalf("scan: %v", err)
		}
		if len(fs) != 0 {
			t.Errorf("string table %q produced findings: %v", table, fs)
		}
	}
}

// TestScan_ConfigAssignmentInAnyCase proves that a secret line of a config
// file is found in lower and mixed case, while the string tables of
// TestScan_BinaryStringTablesAreNotFlagged stay clean.
func TestScan_ConfigAssignmentInAnyCase(t *testing.T) {
	s := New()
	lines := []string{
		"client_secret: Zx9fQ2mPaa71",     // YAML
		"db_password=Zx9fQ2mPaa71",        // a lower-case .env entry
		"api_key = Zx9fQ2mPaa71",          // INI or TOML
		"Password=Zx9fQ2mPaa71",           // mixed case
		"  oauth_token: 'Zx9fQ2mPaa71'\n", // indented, quoted
	}
	for _, line := range lines {
		body := "# config\n" + line + "\nnext: value\n"
		fs, err := s.Scan(strings.NewReader(body))
		if err != nil {
			t.Fatalf("scan: %v", err)
		}
		if !ruleSet(fs)["secret-shaped-config-assignment"] {
			t.Errorf("expected secret-shaped-config-assignment on %q; got %v", line, fs)
		}
		for _, f := range fs {
			if strings.Contains(f.Excerpt, "Zx9fQ2mPaa71") {
				t.Errorf("excerpt re-leaked the secret: %q", f.Excerpt)
			}
		}
	}
}

// TestScan_EnvAssignmentInEnvironBlock proves that the case-sensitive rule
// still finds a secret in the environment block of a process, where each
// entry ends with a NUL byte.
func TestScan_EnvAssignmentInEnvironBlock(t *testing.T) {
	s := New()
	environ := "PATH=/usr/bin\x00HOME=/root\x00GITHUB_TOKEN=abcdef0123456789\x00TERM=xterm\x00"
	fs, err := s.Scan(strings.NewReader(environ))
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if !ruleSet(fs)["secret-shaped-env-assignment"] {
		t.Fatalf("expected secret-shaped-env-assignment on an environ block; got %v", fs)
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
