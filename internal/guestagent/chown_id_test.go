// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package guestagent

import (
	"math"
	"testing"
)

// TestChownID proves an id above math.MaxInt32 is refused, so
// ClaimWorkspace never passes an id to os.Chown that does not fit in an
// int on a 32-bit build.
func TestChownID(t *testing.T) {
	cases := []struct {
		id   uint32
		want int
		ok   bool
	}{
		{0, 0, true},
		{1000, 1000, true},
		{math.MaxInt32, math.MaxInt32, true},
		{1 << 31, 0, false},
		{math.MaxUint32, 0, false},
	}
	for _, c := range cases {
		got, ok := chownID(c.id)
		if ok != c.ok || got != c.want {
			t.Errorf("chownID(%d) = %d, %v, want %d, %v", c.id, got, ok, c.want, c.ok)
		}
	}
}
