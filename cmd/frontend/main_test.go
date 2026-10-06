// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package main

import (
	"strings"
	"testing"
)

const daemonID = "spiffe://example.org/ns/gibson/sa/gibson-daemon"

// TestRepeatedString_CollectsEveryOccurrence pins the allow-list flag
// being repeatable. Keeping only the last occurrence would silently
// narrow the allow-list to one caller.
func TestRepeatedString_CollectsEveryOccurrence(t *testing.T) {
	t.Parallel()
	var ids repeatedString
	for _, id := range []string{daemonID, "spiffe://example.org/ns/gibson/sa/gibson-executor"} {
		if err := ids.Set(id); err != nil {
			t.Fatalf("Set(%q): %v", id, err)
		}
	}
	if len(ids) != 2 {
		t.Fatalf("collected %d IDs (%v), want 2", len(ids), ids)
	}
	if got := ids.String(); !strings.Contains(got, "gibson-daemon") ||
		!strings.Contains(got, "gibson-executor") {
		t.Fatalf("String() = %q, want both IDs", got)
	}
}

// TestParseGrants pins the grant flag: the Pod-write grant of the operator
// is required, and a malformed entry is refused.
func TestParseGrants(t *testing.T) {
	t.Parallel()
	got, err := parseGrants([]string{"setec-sandbox-namespace=setec-system/setec"})
	if err != nil || len(got) != 1 || got[0].ClusterRole != "setec-sandbox-namespace" ||
		got[0].ServiceAccount.Namespace != "setec-system" || got[0].ServiceAccount.Name != "setec" {
		t.Fatalf("parseGrants = %+v, %v", got, err)
	}
	for _, bad := range [][]string{nil, {"role"}, {"role=sa"}, {"=ns/sa"}, {"role=/sa"}} {
		if _, err := parseGrants(bad); err == nil {
			t.Fatalf("parseGrants(%q) accepted the entries", bad)
		}
	}
}
