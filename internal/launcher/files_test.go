// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package launcher

import (
	"os"
	"path/filepath"
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

func TestLoadFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	good := write("good.json", `[{"path":"/setec/files/a/b","source":"/etc/setec/files/f0","mode":365}]`)
	s := testSpec(t)
	s.FilesManifest = good
	if err := s.LoadFiles(); err != nil || len(s.Files) != 1 || s.Files[0].Path != "/setec/files/a/b" {
		t.Fatalf("LoadFiles = %v, %+v", err, s.Files)
	}
	for name, body := range map[string]string{
		"escape.json": `[{"path":"/etc/x","source":"/etc/setec/files/f0","mode":292}]`,
		"write.json":  `[{"path":"/setec/files/a","source":"/etc/setec/files/f0","mode":420}]`,
		"bad.json":    `not json`,
	} {
		s := testSpec(t)
		s.FilesManifest = write(name, body)
		if err := s.LoadFiles(); err == nil {
			t.Fatalf("%s: LoadFiles accepted the manifest", name)
		}
	}
	s = testSpec(t)
	s.FilesManifest = filepath.Join(dir, "missing.json")
	if err := s.LoadFiles(); err == nil {
		t.Fatal("LoadFiles accepted a missing manifest")
	}
}
