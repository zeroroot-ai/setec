//go:build linux

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

package probe

import (
	"context"
	"strings"
	"testing"
)

// TestRuncProbe exercises the runc probe with injected LookPath functions.
func TestRuncProbe(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		lookPathFound bool
		wantAvailable bool
		wantReason    string // substring required when !wantAvailable
	}{
		{
			name:          "runc found in PATH",
			lookPathFound: true,
			wantAvailable: true,
		},
		{
			name:          "runc not in PATH",
			lookPathFound: false,
			wantAvailable: false,
			wantReason:    "runc binary not found in PATH",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			p := newRuncProbe(Config{
				LookPath: fakeLookPath(tc.lookPathFound),
			})

			got := p.Check(context.Background())

			if got.Available != tc.wantAvailable {
				t.Errorf("Available = %v, want %v (Reason=%q)", got.Available, tc.wantAvailable, got.Reason)
			}
			if !tc.wantAvailable && tc.wantReason != "" {
				if !strings.Contains(got.Reason, tc.wantReason) {
					t.Errorf("Reason = %q, want substring %q", got.Reason, tc.wantReason)
				}
			}
			if tc.wantAvailable {
				if _, ok := got.Details["runc"]; !ok {
					t.Errorf("Details missing key %q; got %v", "runc", got.Details)
				}
			}
			if p.Name() != "runc" {
				t.Errorf("Name() = %q, want %q", p.Name(), "runc")
			}
		})
	}
}
