// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package guestagent

import (
	"math"
	"testing"
)

// TestChownID proves an id above the limit is refused. With the limit of
// a 32-bit build, uid 2147483648 does not fit in an int, so ClaimWorkspace
// must not pass it to os.Chown.
func TestChownID(t *testing.T) {
	cases := []struct {
		id    uint32
		limit uint64
		want  int
		ok    bool
	}{
		{1000, math.MaxInt32, 1000, true},
		{math.MaxInt32, math.MaxInt32, math.MaxInt32, true},
		{1 << 31, math.MaxInt32, 0, false},
		{math.MaxUint32, math.MaxInt32, 0, false},
	}
	for _, c := range cases {
		got, ok := chownID(c.id, c.limit)
		if ok != c.ok || got != c.want {
			t.Errorf("chownID(%d, %d) = %d, %v, want %d, %v", c.id, c.limit, got, ok, c.want, c.ok)
		}
	}
}
