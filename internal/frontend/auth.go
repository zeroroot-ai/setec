// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

// Package frontend hosts the gRPC server that translates
// setec.v1.SandboxService RPCs into Sandbox CR operations. Auth
// extraction is in this file; the service logic is in service.go.
package frontend

import (
	"context"
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"github.com/zeroroot-ai/setec/internal/tenancy"
)

// ErrNoPeerCert is returned when a request arrives without a TLS peer
// certificate (the frontend rejects it as UNAUTHENTICATED).
var ErrNoPeerCert = errors.New("frontend: no TLS peer certificate")

// TenantFromContext extracts a tenant identity from the gRPC call's
// peer context. Returns UNAUTHENTICATED when no cert is present and
// PERMISSION_DENIED when the cert carries no usable identity.
//
// Callers that want to accept JWTs as a fallback may wrap this with
// additional metadata inspection; Phase 2 ships with mTLS-only auth.
func TenantFromContext(ctx context.Context) (tenancy.TenantID, error) {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return "", status.Error(codes.Unauthenticated, "missing peer")
	}
	tlsInfo, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok {
		return "", status.Error(codes.Unauthenticated, "peer is not TLS-authenticated")
	}
	if len(tlsInfo.State.PeerCertificates) == 0 {
		return "", status.Error(codes.Unauthenticated, "no client certificate presented")
	}
	tid, err := tenancy.FromCertificate(tlsInfo.State.PeerCertificates[0])
	if err != nil {
		return "", status.Error(codes.PermissionDenied,
			"certificate does not carry a tenant identity")
	}
	return tid, nil
}
