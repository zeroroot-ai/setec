// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package frontend

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	setecv1grpc "github.com/zeroroot-ai/setec/api/grpc/v1"
	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
	"github.com/zeroroot-ai/setec/internal/sandboxid"
)

type identityFixture struct {
	sb  *setecv1alpha1.Sandbox
	key ed25519.PrivateKey
}

func newIdentitySandbox(t *testing.T, name, uid string, gen int64) identityFixture {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sb := &setecv1alpha1.Sandbox{Name: name, Namespace: "team-a", UID: types.UID(uid)}
	sb.Status.Identity = &setecv1alpha1.SandboxIdentityStatus{
		PublicKey: base64.StdEncoding.EncodeToString(pub), Generation: gen}
	return identityFixture{sb: sb, key: priv}
}

func (f identityFixture) token(t *testing.T, sub string, gen int64) string {
	t.Helper()
	now := time.Now()
	tok, err := sandboxid.Sign(f.key, sandboxid.Claims{Issuer: sandboxid.Issuer, Subject: sub, Audience: "gibson",
		Tenant: "a", Generation: gen, IssuedAt: now.Unix(), Expires: now.Add(time.Minute).Unix(), ID: "j-" + sub})
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// TestVerifySandboxIdentity_TellsAForkFromItsSource pins setec#235 at the
// verifier: a token verifies as the sandbox whose key signed it, a fork
// that names its source fails, a token from before a snapshot of the
// source fails, and a token of a replaced sandbox fails.
func TestVerifySandboxIdentity_TellsAForkFromItsSource(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	scheme := runtime.NewScheme()
	utilruntime.Must(setecv1alpha1.AddToScheme(scheme))
	src := newIdentitySandbox(t, "src", "uid-src", 2)
	fork := newIdentitySandbox(t, "fork", "uid-fork", 1)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(src.sb, fork.sb).
		WithStatusSubresource(&setecv1alpha1.Sandbox{}).Build()
	s := &Service{Client: c, AuthDisabled: true, DefaultNamespace: "team-a"}
	verify := func(tok string) (*setecv1grpc.VerifySandboxIdentityResponse, error) {
		return s.VerifySandboxIdentity(ctx, &setecv1grpc.VerifySandboxIdentityRequest{Token: tok, Audience: "gibson"})
	}

	srcID, forkID := "team-a/src/uid-src", "team-a/fork/uid-fork"
	if resp, err := verify(src.token(t, srcID, 2)); err != nil || resp.GetSandboxId() != srcID {
		t.Fatalf("the source token = %v, %v", resp, err)
	}
	if resp, err := verify(fork.token(t, forkID, 1)); err != nil || resp.GetSandboxId() != forkID {
		t.Fatalf("the fork token = %v, %v", resp, err)
	}
	cases := map[string]string{
		// The fork signs with its own key: it cannot claim its source.
		"a fork that names its source": fork.token(t, srcID, 2),
		// A token that the fork found in its copy of the source memory.
		"a token from before the snapshot": src.token(t, srcID, 1),
		"another uid":                      src.token(t, "team-a/src/uid-old", 2),
		"no sandbox":                       src.token(t, "team-a/gone/uid-x", 2),
		"not a token":                      "x.y.z",
	}
	for name, tok := range cases {
		if _, err := verify(tok); status.Code(err) != codes.Unauthenticated {
			t.Errorf("%s: %v, want Unauthenticated", name, err)
		}
	}
	if _, err := s.VerifySandboxIdentity(ctx, &setecv1grpc.VerifySandboxIdentityRequest{Token: "t"}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("no audience: %v", err)
	}
}
