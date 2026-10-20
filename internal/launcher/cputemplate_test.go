// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package launcher

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCheckCPUTemplate(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for _, n := range []string{"m8i.json", "c8i.json"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := CheckCPUTemplate(filepath.Join(dir, "m8i.json")); err != nil {
		t.Fatalf("known template: %v", err)
	}
	err := CheckCPUTemplate(filepath.Join(dir, "z9x.json"))
	if err == nil || !strings.Contains(err.Error(), `"z9x"`) || !strings.Contains(err.Error(), "c8i, m8i") {
		t.Fatalf("unknown template error = %v", err)
	}
}

func TestRun_UnknownCPUTemplateIsABadSpec(t *testing.T) {
	t.Parallel()
	s := testSpec(t)
	s.CPUTemplate = filepath.Join(t.TempDir(), "z9x.json")
	l := &Launcher{Spec: s, Net: &fakeNet{}, VMM: &fakeVMM{}, Console: io.Discard, Grace: time.Second,
		Format: noFormat, CheckCPUTemplate: CheckCPUTemplate}
	code, err := l.Run(t.Context())
	var le *Error
	if code != LaunchFailedExit || !errors.As(err, &le) || le.Reason != ReasonBadSpec {
		t.Fatalf("code = %d, err = %v", code, err)
	}
}

func TestCheckCPUTemplate_UnreadableDirIsReported(t *testing.T) {
	t.Parallel()
	err := CheckCPUTemplate(filepath.Join(t.TempDir(), "no-dir", "m8i.json"))
	if err == nil || !strings.Contains(err.Error(), "cannot be read") {
		t.Fatalf("error = %v", err)
	}
}
