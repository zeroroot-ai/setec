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

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRun_NoArgsIsUsageError(t *testing.T) {
	if got := run(nil, os.Stdout, os.Stderr); got != 2 {
		t.Fatalf("expected exit 2 on no args, got %d", got)
	}
}

func TestRun_CleanArtifactPasses(t *testing.T) {
	dir := t.TempDir()
	clean := filepath.Join(dir, "state.bin")
	if err := os.WriteFile(clean, []byte("PORT=8080\nLOG_LEVEL=info\nbenign memory\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := run([]string{clean}, os.Stdout, os.Stderr); got != 0 {
		t.Fatalf("expected exit 0 on clean artifact, got %d", got)
	}
}

func TestRun_SecretArtifactFails(t *testing.T) {
	dir := t.TempDir()
	dirty := filepath.Join(dir, "memory.bin")
	leak := []byte("AWS_SECRET_ACCESS_KEY=wJalrXUtnFEMIabc1234567890abcdEXAMPLEKEY12\n")
	if err := os.WriteFile(dirty, leak, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := run([]string{dirty}, os.Stdout, os.Stderr); got != 1 {
		t.Fatalf("expected exit 1 on secret artifact, got %d", got)
	}
}

func TestRun_ScansDirectoryRecursively(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "snap-a")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "memory.bin"),
		[]byte("-----BEGIN RSA PRIVATE KEY-----\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := run([]string{dir}, os.Stdout, os.Stderr); got != 1 {
		t.Fatalf("expected exit 1 scanning dir with a leaked key, got %d", got)
	}
}

func TestRun_MissingPathIsError(t *testing.T) {
	if got := run([]string{filepath.Join(t.TempDir(), "nope")}, os.Stdout, os.Stderr); got != 2 {
		t.Fatalf("expected exit 2 on missing path, got %d", got)
	}
}
