// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package webhook

import (
	"context"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	setecruntime "github.com/zeroroot-ai/setec/internal/runtime"
)

// TestSandboxClassWebhook_ValidateFinishedSessionRetention covers the rule
// that finishedSessionRetention, when set, is positive.
func TestSandboxClassWebhook_ValidateFinishedSessionRetention(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		value   *metav1.Duration
		wantErr bool
	}{
		{name: "unset is accepted"},
		{name: "positive is accepted", value: &metav1.Duration{Duration: time.Hour}},
		{name: "zero is refused", value: &metav1.Duration{}, wantErr: true},
		{name: "negative is refused", value: &metav1.Duration{Duration: -time.Minute}, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cls := mkSandboxClass("fsr", mkRuntime(setecruntime.BackendLauncher))
			cls.Spec.FinishedSessionRetention = tc.value
			_, err := classWebhook(t).ValidateCreate(context.Background(), cls)
			if tc.wantErr != (err != nil) {
				t.Fatalf("wantErr=%v, got %v", tc.wantErr, err)
			}
			if tc.wantErr && !strings.Contains(err.Error(), "finishedSessionRetention") {
				t.Fatalf("the error must name the field, got %v", err)
			}
		})
	}
}
