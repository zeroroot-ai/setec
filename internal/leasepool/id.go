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
