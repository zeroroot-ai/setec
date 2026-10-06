// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package frontend

import (
	"context"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	setecv1grpc "github.com/zeroroot-ai/setec/api/grpc/v1"
	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
	"github.com/zeroroot-ai/setec/internal/sandboxid"
)

// VerifySandboxIdentity checks an identity token of a launcher sandbox
// (setec#235) and returns the sandbox that it names. The key and the
// generation come from the status of that sandbox: a token of a fork never
// verifies as its source, and a token from before a snapshot of the
// sandbox no longer verifies.
func (s *Service) VerifySandboxIdentity(
	ctx context.Context, req *setecv1grpc.VerifySandboxIdentityRequest,
) (*setecv1grpc.VerifySandboxIdentityResponse, error) {
	if req.GetToken() == "" || req.GetAudience() == "" {
		return nil, status.Error(codes.InvalidArgument, "token and audience are required")
	}
	sub, err := sandboxid.Subject(req.GetToken())
	if err != nil {
		return nil, status.Error(codes.Unauthenticated, err.Error())
	}
	parts := strings.Split(sub, "/")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return nil, status.Error(codes.Unauthenticated, "the token names no sandbox id")
	}
	ns, name, uid := parts[0], parts[1], parts[2]
	if err := s.checkTenantNamespace(ctx, req.GetTenant(), ns); err != nil {
		return nil, err
	}
	sb := &setecv1alpha1.Sandbox{}
	if err := s.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, sb); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, status.Error(codes.Unauthenticated, "the sandbox of the token does not exist")
		}
		return nil, status.Errorf(grpcCodeFor(err), "get Sandbox: %v", err)
	}
	if string(sb.UID) != uid {
		return nil, status.Error(codes.Unauthenticated, "the sandbox of the token was replaced")
	}
	id := sb.Status.Identity
	if id == nil {
		return nil, status.Error(codes.Unauthenticated, "the sandbox has no identity")
	}
	key, err := sandboxid.PublicKey(id.PublicKey)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	c, err := sandboxid.Verify(req.GetToken(), sandboxid.Expect{
		Key: key, Subject: sub, Audience: req.GetAudience(), Generation: id.Generation, Now: time.Now(),
	})
	if err != nil {
		return nil, status.Error(codes.Unauthenticated, err.Error())
	}
	return &setecv1grpc.VerifySandboxIdentityResponse{
		SandboxId: sub, Tenant: c.Tenant, Generation: c.Generation, TokenId: c.ID, ExpiresUnix: c.Expires,
	}, nil
}
