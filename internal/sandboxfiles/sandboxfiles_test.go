// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package sandboxfiles

import (
	"strconv"
	"strings"
	"testing"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
)

func file(path, content string, mode *int32) setecv1alpha1.SandboxFile {
	return setecv1alpha1.SandboxFile{Path: path, Content: []byte(content), Mode: mode}
}

func TestValidate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		files   []setecv1alpha1.SandboxFile
		wantErr string
	}{
		{name: "no files", files: nil},
		{name: "a nested file", files: []setecv1alpha1.SandboxFile{file("skill/scripts/run.sh", "x", new(int32(0o555)))}},
		{name: "mode 0444", files: []setecv1alpha1.SandboxFile{file("a", "x", new(int32(0o444)))}},
		{name: "empty path", files: []setecv1alpha1.SandboxFile{file("", "x", nil)}, wantErr: "empty"},
		{name: "absolute path", files: []setecv1alpha1.SandboxFile{file("/etc/passwd", "x", nil)}, wantErr: "relative"},
		{name: "dot dot", files: []setecv1alpha1.SandboxFile{file("a/../../b", "x", nil)}, wantErr: "clean"},
		{name: "leading dot dot", files: []setecv1alpha1.SandboxFile{file("../b", "x", nil)}, wantErr: "clean"},
		{name: "dot element", files: []setecv1alpha1.SandboxFile{file("a/./b", "x", nil)}, wantErr: "clean"},
		{name: "double slash", files: []setecv1alpha1.SandboxFile{file("a//b", "x", nil)}, wantErr: "clean"},
		{name: "trailing slash", files: []setecv1alpha1.SandboxFile{file("a/", "x", nil)}, wantErr: "clean"},
		{name: "NUL byte", files: []setecv1alpha1.SandboxFile{file("a\x00b", "x", nil)}, wantErr: "NUL"},
		{name: "backslash", files: []setecv1alpha1.SandboxFile{file(`a\b`, "x", nil)}, wantErr: "backslash"},
		{name: "long path", files: []setecv1alpha1.SandboxFile{file(strings.Repeat("a", 513), "x", nil)}, wantErr: "longer"},
		{name: "same path twice", files: []setecv1alpha1.SandboxFile{file("a", "x", nil), file("a", "y", nil)}, wantErr: "twice"},
		{name: "file and directory", files: []setecv1alpha1.SandboxFile{file("a", "x", nil), file("a/b", "y", nil)}, wantErr: "directory"},
		{name: "write bit", files: []setecv1alpha1.SandboxFile{file("a", "x", new(int32(0o644)))}, wantErr: "write"},
		{name: "setuid bit", files: []setecv1alpha1.SandboxFile{file("a", "x", new(int32(0o4555)))}, wantErr: "setuid"},
		{name: "negative mode", files: []setecv1alpha1.SandboxFile{file("a", "x", new(int32(-1)))}, wantErr: "mode"},
		{name: "total at the bound", files: []setecv1alpha1.SandboxFile{
			file("a", strings.Repeat("x", MaxTotalBytes-len("a")-PerFileOverhead), nil),
		}},
		{name: "total above the bound", files: []setecv1alpha1.SandboxFile{
			file("a", strings.Repeat("x", MaxTotalBytes-len("a")-PerFileOverhead+1), nil),
		}, wantErr: "total"},
		{name: "many long paths above the bound", files: manyFiles(2000, 200), wantErr: "total"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := Validate(tc.files)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("Validate: %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("Validate = %v, want an error with %q", err, tc.wantErr)
			}
		})
	}
}

func manyFiles(n, pathLen int) []setecv1alpha1.SandboxFile {
	files := make([]setecv1alpha1.SandboxFile, 0, n)
	for i := range n {
		p := strconv.Itoa(i)
		files = append(files, file(strings.Repeat("d", pathLen-len(p)-1)+"/"+p, "", nil))
	}
	return files
}

func TestManyFilesAreNotLimitedByCount(t *testing.T) {
	t.Parallel()
	files := make([]setecv1alpha1.SandboxFile, 0, 2000)
	for i := range 2000 {
		files = append(files, file("d/"+strconv.Itoa(i)+"/f", "x", nil))
	}
	if err := Validate(files); err != nil {
		t.Fatalf("2000 small files: %v", err)
	}
}

func TestModeOfAndMachinePath(t *testing.T) {
	t.Parallel()
	if got := ModeOf(file("a", "x", nil)); got != DefaultMode {
		t.Fatalf("default mode = %o", got)
	}
	if got := ModeOf(file("a", "x", new(int32(0o555)))); got != 0o555 {
		t.Fatalf("mode = %o", got)
	}
	if got := MachinePath("s/run.sh"); got != "/setec/files/s/run.sh" {
		t.Fatalf("path = %s", got)
	}
}
