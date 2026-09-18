// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package chartname

import (
	"strings"
	"testing"
)

// TestFullname pins the derivation to what charts/setec/templates/
// _helpers.tpl renders. The e2e suite reads the operator Deployment and
// the DaemonSets back by these names; a release name without "setec" in
// it (the chain-6 job's chain6-exit) once made the install gate wait ten
// minutes for a Deployment the chart never rendered under that name.
func TestFullname(t *testing.T) {
	t.Parallel()
	long := "setec-" + strings.Repeat("x", 56) + "-tail"
	tests := []struct {
		release string
		want    string
	}{
		{"setec-e2e-suites", "setec-e2e-suites"},
		{"setec-e2e-session", "setec-e2e-session"},
		{"setec-e2e-20260918-120000", "setec-e2e-20260918-120000"},
		{"chain6-exit", "chain6-exit-setec"},
		{"chain6-exit-fixture", "chain6-exit-fixture-setec"},
		{"setec", "setec"},
		// 67 characters, cut to 63, which ends on the "-" before "tail".
		{long, "setec-" + strings.Repeat("x", 56)},
	}
	for _, tc := range tests {
		if got := Fullname(tc.release); got != tc.want {
			t.Errorf("Fullname(%q) = %q, want %q", tc.release, got, tc.want)
		}
		if len(Fullname(tc.release)) > 63 {
			t.Errorf("Fullname(%q) is longer than 63 characters", tc.release)
		}
	}
}
