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

package webhook

import (
	"context"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
	setecruntime "github.com/zeroroot-ai/setec/internal/runtime"
)

// TestSandboxClassWebhook_ValidateMaxPauseDuration covers the
// maxPauseDuration coherence rule (setec#202): the cap, when set, must
// be positive — omission is the way to leave pauses unbounded.
func TestSandboxClassWebhook_ValidateMaxPauseDuration(t *testing.T) {
	t.Parallel()

	mk := func(max *metav1.Duration) *setecv1alpha1.SandboxClass {
		cls := mkSandboxClass("mpd", "", mkRuntime(setecruntime.BackendKataFC))
		cls.Spec.MaxPauseDuration = max
		return cls
	}

	tests := []struct {
		name    string
		class   *setecv1alpha1.SandboxClass
		wantErr bool
		wantMsg string
	}{
		{
			name:  "unset → accept (pauses unbounded)",
			class: mk(nil),
		},
		{
			name:  "positive → accept",
			class: mk(&metav1.Duration{Duration: 30 * time.Minute}),
		},
		{
			name:    "zero → reject",
			class:   mk(&metav1.Duration{}),
			wantErr: true,
			wantMsg: "must be a positive duration",
		},
		{
			name:    "negative → reject",
			class:   mk(&metav1.Duration{Duration: -time.Minute}),
			wantErr: true,
			wantMsg: "must be a positive duration",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := webhookWith(fakeClientWithNS(t, gateNamespaceUnlabelled()), baseConfig())
			_, err := w.ValidateCreate(context.Background(), tc.class)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected rejection, got admit")
				}
				if !strings.Contains(err.Error(), tc.wantMsg) {
					t.Fatalf("error %q does not contain %q", err.Error(), tc.wantMsg)
				}
				return
			}
			if err != nil {
				t.Fatalf("expected admit, got %v", err)
			}
		})
	}
}
