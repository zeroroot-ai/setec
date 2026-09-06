// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

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
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mkFakeFS creates a temporary directory with the given relative paths.
// Paths ending in "/" are created as directories; all others as empty files.
func mkFakeFS(t *testing.T, paths ...string) string {
	t.Helper()
	root := t.TempDir()
	for _, p := range paths {
		full := filepath.Join(root, p)
		if strings.HasSuffix(p, "/") {
			if err := os.MkdirAll(full, 0o755); err != nil {
				t.Fatalf("mkFakeFS mkdir %q: %v", full, err)
			}
		} else {
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				t.Fatalf("mkFakeFS mkdir parent %q: %v", full, err)
			}
			if err := os.WriteFile(full, nil, 0o644); err != nil {
				t.Fatalf("mkFakeFS write %q: %v", full, err)
			}
		}
	}
	return root
}

// TestKVMAvailable verifies the KVMAvailable helper against a synthetic
// filesystem rooted at a temp directory.
func TestKVMAvailable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		paths     []string // files to create in the fake FS root
		wantOK    bool
		wantInMsg string // required substring in reason when !wantOK
	}{
		{
			name:   "dev/kvm present",
			paths:  []string{"dev/kvm"},
			wantOK: true,
		},
		{
			name:      "dev/kvm absent",
			paths:     []string{},
			wantOK:    false,
			wantInMsg: "/dev/kvm not found",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := mkFakeFS(t, tc.paths...)
			ok, reason := KVMAvailable(root)
			if ok != tc.wantOK {
				t.Errorf("KVMAvailable() ok = %v, want %v (reason=%q)", ok, tc.wantOK, reason)
			}
			if !tc.wantOK && tc.wantInMsg != "" && !strings.Contains(reason, tc.wantInMsg) {
				t.Errorf("KVMAvailable() reason = %q, want substring %q", reason, tc.wantInMsg)
			}
		})
	}
}

// TestModuleLoaded verifies the ModuleLoaded helper against a synthetic
// filesystem.
func TestModuleLoaded(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		paths      []string
		moduleName string
		want       bool
	}{
		{
			name:       "kvm_intel present",
			paths:      []string{"sys/module/kvm_intel/"},
			moduleName: "kvm_intel",
			want:       true,
		},
		{
			name:       "kvm_amd present",
			paths:      []string{"sys/module/kvm_amd/"},
			moduleName: "kvm_amd",
			want:       true,
		},
		{
			name:       "module absent",
			paths:      []string{},
			moduleName: "kvm_intel",
			want:       false,
		},
		{
			name:       "other module present but queried one absent",
			paths:      []string{"sys/module/kvm_amd/"},
			moduleName: "kvm_intel",
			want:       false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := mkFakeFS(t, tc.paths...)
			got := ModuleLoaded(root, tc.moduleName)
			if got != tc.want {
				t.Errorf("ModuleLoaded(%q) = %v, want %v", tc.moduleName, got, tc.want)
			}
		})
	}
}

// TestLoadedKVMModule verifies the preference order and the arm64 built-in
// fallback of the LoadedKVMModule helper.
func TestLoadedKVMModule(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		paths      []string
		wantModule string
		wantOK     bool
	}{
		{
			name:       "kvm_intel preferred over generic kvm",
			paths:      []string{"sys/module/kvm_intel/", "sys/module/kvm/"},
			wantModule: "kvm_intel",
			wantOK:     true,
		},
		{
			name:       "kvm_amd preferred over generic kvm",
			paths:      []string{"sys/module/kvm_amd/", "sys/module/kvm/"},
			wantModule: "kvm_amd",
			wantOK:     true,
		},
		{
			name:       "built-in kvm only (arm64)",
			paths:      []string{"sys/module/kvm/"},
			wantModule: "kvm",
			wantOK:     true,
		},
		{
			name:   "no KVM module",
			paths:  []string{"sys/module/loop/"},
			wantOK: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := mkFakeFS(t, tc.paths...)
			mod, ok := LoadedKVMModule(root)
			if ok != tc.wantOK {
				t.Errorf("LoadedKVMModule() ok = %v, want %v", ok, tc.wantOK)
			}
			if mod != tc.wantModule {
				t.Errorf("LoadedKVMModule() module = %q, want %q", mod, tc.wantModule)
			}
		})
	}
}
