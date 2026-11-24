// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package secretscan

import (
	"strings"
	"testing"
)

func scanText(t *testing.T, s *Scanner, text string) []Finding {
	t.Helper()
	got, err := s.Scan(strings.NewReader(text))
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// TestIdentifierTables: the field names of a program side by side in memory
// are not a secret, and a real assignment still is (setec#412).
func TestIdentifierTables(t *testing.T) {
	t.Parallel()
	for _, text := range []string{
		"\x00ConfigMapRef:SecretKeyRef:integerunsignedstring\x00",
		"\x00SecretName:%s>/etc/passwd\x00",
		"\x00TokenRequest:serviceaccounttokenprojection\x00",
	} {
		if got := scanText(t, New(), text); len(got) != 0 {
			t.Errorf("%q: findings %v; want none", text, got)
		}
	}
	for _, text := range []string{
		"DB_PASSWORD=hunter2hunter2",
		"client_secret:abcdefghijklmnop",
		"SecretKey: abcdefghijklmnop",
		"SecretKey=Abcdefgh1234567",
	} {
		if got := scanText(t, New(), text); len(got) == 0 {
			t.Errorf("%q: no finding; want one", text)
		}
	}
}

func TestAllowAndLimit(t *testing.T) {
	t.Parallel()
	text := "DB_PASSWORD=hunter2hunter2\n"
	a, err := ParseAllow("secret-shaped-env-assignment=hunter2")
	if err != nil {
		t.Fatal(err)
	}
	// Two rules match this line. The allow covers one of them.
	if got := scanText(t, New().WithAllow([]Allow{a}), text); len(got) != 1 || got[0].Rule != "secret-shaped-config-assignment" {
		t.Fatalf("an allow for one rule leaves %v; want only the other rule", got)
	}
	other, _ := ParseAllow("secret-shaped-config-assignment=hunter2")
	if got := scanText(t, New().WithAllow([]Allow{other}), text); len(got) != 1 || got[0].Rule != "secret-shaped-env-assignment" {
		t.Fatalf("an allow for the other rule leaves %v", got)
	}
	each, _ := ParseAllow("hunter2")
	if got := scanText(t, New().WithAllow([]Allow{each}), text); len(got) != 0 {
		t.Fatalf("an allow for each rule still finds %v", got)
	}
	many := strings.Repeat("API_TOKEN=abcdefgh1234567\nDB_PASSWORD=zyxwvuts7654321\nX_SECRET=qwertyui0987654\n", 1)
	if got := scanText(t, New().WithLimit(1), many); len(got) != 1 {
		t.Fatalf("limit 1 gives %d findings", len(got))
	}
	for _, bad := range []string{"", "secret-shaped-env-assignment=", "([", "nope=(", "secret-shaped-typo=x"} {
		if _, err := ParseAllow(bad); err == nil {
			t.Errorf("ParseAllow(%q) accepted", bad)
		}
	}
}
