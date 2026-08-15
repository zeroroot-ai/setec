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

// TestKataQEMUProbe covers the kata-qemu probe across KVM, the TCG fallback,
// and the containerd runtime-handler requirement that gates all of them
// (setec#243).
func TestKataQEMUProbe(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		paths         []string // files/dirs to create under a temp FS root
		handlers      []string // containerd handlers to register; nil means {"kata-qemu"}
		noContainerd  bool     // write no containerd config at all
		allowTCG      bool
		wantAvailable bool
		wantMode      string // expected Details["mode"] when available
		wantReason    string // required substring in Reason
	}{
		{
			name:          "kvm + module + handler",
			paths:         []string{"dev/kvm", "sys/module/kvm_intel/"},
			wantAvailable: true,
			wantMode:      "kvm",
		},
		{
			name:          "no kvm, TCG allowed, handler registered",
			paths:         []string{},
			allowTCG:      true,
			wantAvailable: true,
			wantMode:      "tcg",
			wantReason:    "TCG fallback",
		},
		{
			name:          "no kvm, TCG disallowed",
			paths:         []string{},
			wantAvailable: false,
			wantReason:    "kata-qemu requires KVM",
		},
		{
			name:          "kvm device but no module, TCG disallowed",
			paths:         []string{"dev/kvm"},
			wantAvailable: false,
			wantReason:    "requires a KVM kernel module",
		},
		{
			// The staging failure: labels published on a node whose
			// containerd had never heard of kata.
			name:          "kvm + module but containerd registers no kata-qemu handler",
			paths:         []string{"dev/kvm", "sys/module/kvm_intel/"},
			handlers:      []string{"runc"},
			wantAvailable: false,
			wantReason:    `no runtime handler "kata-qemu" configured`,
		},
		{
			name:          "TCG fallback does not excuse a missing handler",
			paths:         []string{},
			handlers:      []string{"runc"},
			allowTCG:      true,
			wantAvailable: false,
			wantReason:    `no runtime handler "kata-qemu" configured`,
		},
		{
			name:          "kvm + module but no containerd config is readable",
			paths:         []string{"dev/kvm", "sys/module/kvm_intel/"},
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
					handlers = []string{"kata-qemu"}
				}
				writeStockContainerdConfig(t, root, handlers...)
			}
			p := newKataQEMUProbe(Config{FSRoot: root, AllowTCG: tc.allowTCG})

			got := p.Check(context.Background())

			if got.Available != tc.wantAvailable {
				t.Fatalf("Available = %v, want %v (Reason=%q)", got.Available, tc.wantAvailable, got.Reason)
			}
			if tc.wantReason != "" && !strings.Contains(got.Reason, tc.wantReason) {
				t.Errorf("Reason = %q, want substring %q", got.Reason, tc.wantReason)
			}
			if tc.wantMode != "" && got.Details["mode"] != tc.wantMode {
				t.Errorf("Details[mode] = %q, want %q", got.Details["mode"], tc.wantMode)
			}
			if tc.wantAvailable && got.Details["containerd_handler"] != handlerConfigured {
				t.Errorf("Details[containerd_handler] = %q, want %q",
					got.Details["containerd_handler"], handlerConfigured)
			}
			if p.Name() != "kata-qemu" {
				t.Errorf("Name() = %q, want %q", p.Name(), "kata-qemu")
			}
		})
	}
}
