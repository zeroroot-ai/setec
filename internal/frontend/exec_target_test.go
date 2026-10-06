// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package frontend

import (
	"reflect"
	"testing"
)

// TestExecTarget pins the one exec path: the command runs in the machine
// of the Sandbox through the relay of the launcher container.
func TestExecTarget(t *testing.T) {
	t.Parallel()
	cmd := []string{"nmap", "-h"}
	c, got := execTarget(cmd)
	want := append(append([]string{}, LauncherExecCommand...), cmd...)
	if c != "launcher" || !reflect.DeepEqual(got, want) {
		t.Fatalf("execTarget = %s %v", c, got)
	}
}
