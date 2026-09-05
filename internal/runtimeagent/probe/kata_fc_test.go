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
	"context"
	"strings"
	"testing"
)

// TestKataFCProbe exercises the kata-fc probe against fake filesystem layouts
// covering every combination of KVM device, CPU module, and containerd
// runtime-handler registration.
func TestKataFCProbe(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		paths         []string // files/dirs to create under a temp FS root
		handlers      []string // containerd handlers to register; nil means {"kata-fc"}
		noContainerd  bool     // write no containerd config at all
		wantAvailable bool
		wantReason    string // required substring in Reason when !wantAvailable
		wantModule    string // expected Details["kvm_module"] when available
	}{
		{
			name: "kvm + kvm_intel present",
			paths: []string{
				"dev/kvm",
				"sys/module/kvm_intel/",
			},
			wantAvailable: true,
			wantModule:    "kvm_intel",
		},
		{
			name: "kvm + kvm_amd present",
			paths: []string{
				"dev/kvm",
				"sys/module/kvm_amd/",
			},
			wantAvailable: true,
			wantModule:    "kvm_amd",
		},
		{
			name: "kvm + both intel and amd present (intel preferred)",
			paths: []string{
				"dev/kvm",
				"sys/module/kvm_intel/",
				"sys/module/kvm_amd/",
			},
			wantAvailable: true,
			wantModule:    "kvm_intel",
		},
		{
			name: "kvm + built-in kvm module only (arm64 Graviton)",
			paths: []string{
				"dev/kvm",
				"sys/module/kvm/",
			},
			wantAvailable: true,
			wantModule:    "kvm",
		},
		{
			name:          "kvm absent",
			paths:         []string{"sys/module/kvm_intel/"},
			wantAvailable: false,
			wantReason:    "KVM device /dev/kvm not found",
		},
		{
			name:          "kvm device present but no KVM module",
			paths:         []string{"dev/kvm"},
			wantAvailable: false,
			wantReason:    "none of kvm_intel, kvm_amd, or kvm",
		},
		{
			name:          "nothing present",
			paths:         []string{},
			wantAvailable: false,
			wantReason:    "KVM device /dev/kvm not found",
		},
		{
			// setec#243: KVM alone said "kata-capable" on a node where
			// kata was never installed, and every RunPodSandbox failed.
			name: "kvm + module but containerd registers no kata-fc handler",
			paths: []string{
				"dev/kvm",
				"sys/module/kvm_intel/",
			},
			handlers:      []string{"runc"},
			wantAvailable: false,
			wantReason:    `no runtime handler "kata-fc" configured`,
		},
		{
			name: "kvm + module but no containerd config is readable",
			paths: []string{
				"dev/kvm",
				"sys/module/kvm_intel/",
			},
			noContainerd:  true,
			wantAvailable: false,
			wantReason:    "no containerd configuration is readable",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			root := mkFakeFS(t, tc.paths...)
			if !tc.noContainerd {
				handlers := tc.handlers
				if handlers == nil {
					handlers = []string{"kata-fc"}
				}
				writeStockContainerdConfig(t, root, handlers...)
			}
			p := newKataFCProbe(Config{FSRoot: root})

			got := p.Check(context.Background())

			if got.Available != tc.wantAvailable {
				t.Errorf("Available = %v, want %v (Reason=%q)", got.Available, tc.wantAvailable, got.Reason)
			}
			if !tc.wantAvailable && tc.wantReason != "" {
				if !strings.Contains(got.Reason, tc.wantReason) {
					t.Errorf("Reason = %q, want substring %q", got.Reason, tc.wantReason)
				}
			}
			if tc.wantAvailable && tc.wantModule != "" {
				if got.Details["kvm_module"] != tc.wantModule {
					t.Errorf("Details[kvm_module] = %q, want %q", got.Details["kvm_module"], tc.wantModule)
				}
			}
			// Name must be constant.
			if p.Name() != "kata-fc" {
				t.Errorf("Name() = %q, want %q", p.Name(), "kata-fc")
			}
		})
	}
}
