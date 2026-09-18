// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package webhook

import (
	"context"
	"strings"
	"testing"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
)

// TestValidateCreate_CommandRequiredUnlessSession is the admission fixture
// for setec#7: only a session may omit spec.command.
func TestValidateCreate_CommandRequiredUnlessSession(t *testing.T) {
	t.Parallel()
	v := lifecycleValidator(t)
	base := mkSandbox("standard", 1, "1Gi", "")

	withoutCommand := func(sb *setecv1alpha1.Sandbox) *setecv1alpha1.Sandbox {
		out := sb.DeepCopy()
		out.Spec.Command = nil
		return out
	}

	cases := []struct {
		name    string
		sb      *setecv1alpha1.Sandbox
		wantErr bool
	}{
		{"implicit ephemeral without command denied", withoutCommand(base), true},
		{"explicit ephemeral without command denied", withoutCommand(withMode(base, setecv1alpha1.LifecycleModeEphemeral)), true},
		{"session without command allowed", withoutCommand(withMode(base, setecv1alpha1.LifecycleModeSession)), false},
		{"session with command allowed", withMode(base, setecv1alpha1.LifecycleModeSession), false},
		{"ephemeral with command allowed", base, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := v.ValidateCreate(context.Background(), tc.sb)
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), "spec.command must have at least one entry") {
					t.Fatalf("err = %v, want the spec.command rejection", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
		})
	}
}
