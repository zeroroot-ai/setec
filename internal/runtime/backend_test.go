// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package runtime

import (
	"strings"
	"testing"
)

// TestValidateBackend_OneBackend pins the cutover of setec#198: the
// launcher and an empty name pass, a removed backend is refused with the
// reason, and an unknown name is refused.
func TestValidateBackend_OneBackend(t *testing.T) {
	t.Parallel()
	for _, ok := range []string{"", BackendLauncher} {
		if err := ValidateBackend(ok); err != nil {
			t.Errorf("ValidateBackend(%q) = %v", ok, err)
		}
	}
	for _, removed := range RemovedBackends {
		err := ValidateBackend(removed)
		if err == nil || !strings.Contains(err.Error(), "was removed") {
			t.Errorf("ValidateBackend(%q) = %v, want the removal reason", removed, err)
		}
	}
	if err := ValidateBackend("qemu"); err == nil {
		t.Error("an unknown backend was accepted")
	}
}
