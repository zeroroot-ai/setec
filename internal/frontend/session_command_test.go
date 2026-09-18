// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package frontend

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	setecv1grpc "github.com/zeroroot-ai/setec/api/grpc/v1"
)

// TestLaunch_SessionMayOmitCommand is the frontend fixture for setec#7: a
// session request with no command is accepted and lands as a Sandbox
// with an empty spec.command, which the operator boots as the keepalive.
// Every other lifecycle still requires one.
func TestLaunch_SessionMayOmitCommand(t *testing.T) {
	t.Parallel()

	sb := launchAndFetch(t, &setecv1grpc.LaunchRequest{
		Image:     "alpine:3.19",
		Resources: &setecv1grpc.Resources{Vcpu: 1, Memory: "256Mi"},
		Lifecycle: &setecv1grpc.Lifecycle{Mode: "session"},
	})
	if !sb.Spec.IsSession() {
		t.Fatalf("effective mode = %q, want session", sb.Spec.EffectiveLifecycleMode())
	}
	if len(sb.Spec.Command) != 0 {
		t.Fatalf("spec.command = %v, want empty so the operator boots the keepalive", sb.Spec.Command)
	}

	s := &Service{Client: newClient(t), AuthDisabled: true, DefaultNamespace: "team-a"}
	for name, req := range map[string]*setecv1grpc.LaunchRequest{
		"no lifecycle":       {Image: "x"},
		"empty lifecycle":    {Image: "x", Lifecycle: &setecv1grpc.Lifecycle{}},
		"explicit ephemeral": {Image: "x", Lifecycle: &setecv1grpc.Lifecycle{Mode: "ephemeral"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := s.Launch(context.Background(), req)
			if got := status.Code(err); got != codes.InvalidArgument {
				t.Fatalf("code = %s, want InvalidArgument (err=%v)", got, err)
			}
		})
	}
}
