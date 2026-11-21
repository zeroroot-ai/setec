// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package launcher

import (
	"strings"
	"testing"
)

func TestSpecValidate_Files(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		files   []File
		noLoad  bool
		wantErr string
	}{
		{name: "none"},
		{name: "one file", files: []File{{Path: "/setec/files/s/run.sh", Source: "/etc/setec/files/f0", Mode: 0o555}}},
		{name: "outside the directory", files: []File{{Path: "/etc/passwd", Source: "/etc/setec/files/f0", Mode: 0o444}}, wantErr: "below /setec/files"},
		{name: "dot dot", files: []File{{Path: "/setec/files/../etc/x", Source: "/etc/setec/files/f0", Mode: 0o444}}, wantErr: "below /setec/files"},
		{name: "the directory itself", files: []File{{Path: "/setec/files", Source: "/etc/setec/files/f0", Mode: 0o444}}, wantErr: "below /setec/files"},
		{name: "relative source", files: []File{{Path: "/setec/files/a", Source: "f0", Mode: 0o444}}, wantErr: "absolute"},
		{name: "write bit", files: []File{{Path: "/setec/files/a", Source: "/etc/setec/files/f0", Mode: 0o644}}, wantErr: "write"},
		{name: "no workload", files: []File{{Path: "/setec/files/a", Source: "/etc/setec/files/f0", Mode: 0o444}}, noLoad: true, wantErr: "workload"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := testSpec(t)
			s.Files = tc.files
			if tc.noLoad {
				s.Workload = nil
				s.Base = true
			}
			err := s.Validate()
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("Validate: %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("Validate = %v, want an error with %q", err, tc.wantErr)
			}
		})
	}
}
