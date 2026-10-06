// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

// Package errwrap adds the step that failed to an error from another
// package, so a log line or a status names where a failure started.
package errwrap

import "fmt"

// Wrap returns err with op in front, or nil when err is nil. A caller can
// wrap the result of a call that may succeed: a nil error stays nil.
func Wrap(err error, op string) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", op, err)
}
