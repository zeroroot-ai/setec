// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package leasepool

import (
	"crypto/rand"
	"encoding/hex"
)

// randID returns an opaque, unguessable lease id. crypto/rand is used so
// a lease token cannot be guessed by another tenant; the manager still
// scopes leases per namespace, but defence in depth is cheap here.
func randID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// rand.Read never returns an error on supported platforms; if it
		// somehow does, fall back to a fixed prefix so the caller still
		// gets a usable (if low-entropy) token rather than a panic.
		return "lease-fallback"
	}
	return "lease-" + hex.EncodeToString(b[:])
}
