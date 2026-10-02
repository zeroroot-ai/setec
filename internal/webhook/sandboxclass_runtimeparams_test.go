// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package webhook

import (
	"context"
	"strings"
	"testing"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
	setecruntime "github.com/zeroroot-ai/setec/internal/runtime"
)

// spec.runtime.params is documented in docs/crd-reference.md and
// docs/multitenancy.md, translated by internal/runtime's kataAnnotation table,
// and validated by ErrUnknownKataParam. It was also never delivered: the podspec
// builder called MutatePod(pod, nil) and no caller supplied params (#121).
//
// Now that they reach MutatePod, an unknown key fails pod creation. These cases
// pin the admission half, so that failure lands on whoever wrote the class rather
// than on every Sandbox that later uses it.

func TestSandboxClassWebhook_RuntimeParams(t *testing.T) {
	t.Parallel()

	withParams := func(backend string, params map[string]string) *setecv1alpha1.SandboxClass {
		cls := mkSandboxClass("rp", "", mkRuntime(backend))
		cls.Spec.Runtime.Params = params
		return cls
	}

	tests := []struct {
		name    string
		class   *setecv1alpha1.SandboxClass
		wantErr bool
		wantMsg []string
	}{
		{
			name:  "no params at all, accept",
			class: withParams(setecruntime.BackendKataQEMU, nil),
		},
		{
			name:  "both accepted keys on kata-qemu, accept",
			class: withParams(setecruntime.BackendKataQEMU, map[string]string{"vcpus": "4", "memory": "2048"}),
		},
		{
			name:    "an unknown key on a backend that accepts some, reject and list what is accepted",
			class:   withParams(setecruntime.BackendKataQEMU, map[string]string{"vcpus": "4", "hugepages": "on"}),
			wantErr: true,
			wantMsg: []string{"spec.runtime.params", "hugepages", "accepts only memory, vcpus"},
		},
		{
			// kata-fc, gvisor and runc all take `_ map[string]string`. Naming only
			// the key would read as a typo; the backend is the problem.
			name:    "params on a backend that consumes none, reject and say so",
			class:   withParams(setecruntime.BackendGVisor, map[string]string{"vcpus": "4"}),
			wantErr: true,
			wantMsg: []string{"consumes no runtime params", "declared and never applied"},
		},
		{
			name:    "params on kata-fc, reject",
			class:   withParams(setecruntime.BackendKataFC, map[string]string{"memory": "1024"}),
			wantErr: true,
			wantMsg: []string{"consumes no runtime params"},
		},
		{
			// Every bad key at once, so one mistake is one round trip.
			name:    "two unknown keys, both named",
			class:   withParams(setecruntime.BackendKataQEMU, map[string]string{"hugepages": "on", "numa": "1"}),
			wantErr: true,
			wantMsg: []string{"hugepages,numa"},
		},
		{
			// An empty map is not "params were set".
			name:  "an empty params map, accept",
			class: withParams(setecruntime.BackendKataQEMU, map[string]string{}),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := webhookWith(fakeClientWithNS(t, gateNamespaceUnlabelled()), baseConfig())
			_, err := w.ValidateCreate(context.Background(), tc.class)
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("unexpected validation error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("a class declaring params that cannot take effect was admitted")
			}
			for _, want := range tc.wantMsg {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err.Error(), want)
				}
			}
		})
	}
}

// TestAcceptedParamsIsDerivedNotRestated. The admission check and MutatePod must
// agree about which keys exist. If someone adds a kata annotation and forgets the
// allowlist, this fails rather than admitting a key MutatePod will reject.
func TestAcceptedParamsIsDerivedNotRestated(t *testing.T) {
	t.Parallel()

	accepted := setecruntime.AcceptedParams(setecruntime.BackendKataQEMU)
	if len(accepted) == 0 {
		t.Fatal("kata-qemu reports no accepted params, but its MutatePod translates some")
	}
	// Every accepted key must be one MutatePod actually applies, proven by
	// running it rather than by reading the table.
	for _, k := range accepted {
		cls := mkSandboxClass("derived", "", mkRuntime(setecruntime.BackendKataQEMU))
		cls.Spec.Runtime.Params = map[string]string{k: "1"}
		w := webhookWith(fakeClientWithNS(t, gateNamespaceUnlabelled()), baseConfig())
		if _, err := w.ValidateCreate(context.Background(), cls); err != nil {
			t.Errorf("AcceptedParams lists %q but admission rejects it: %v", k, err)
		}
	}
	for _, b := range []string{setecruntime.BackendKataFC, setecruntime.BackendGVisor} {
		if got := setecruntime.AcceptedParams(b); len(got) != 0 {
			t.Errorf("%s reports accepted params %v, but its MutatePod ignores them", b, got)
		}
	}
}
