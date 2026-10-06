// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package errwrap

import (
	"errors"
	"io/fs"
	"testing"
)

// TestWrap: nil stays nil, and a wrapped error keeps its chain and names
// the step.
func TestWrap(t *testing.T) {
	t.Parallel()
	if Wrap(nil, "read") != nil {
		t.Fatal("a nil error became an error")
	}
	err := Wrap(fs.ErrNotExist, "read the spec")
	if !errors.Is(err, fs.ErrNotExist) || err.Error() != "read the spec: file does not exist" {
		t.Fatalf("Wrap = %v", err)
	}
}
