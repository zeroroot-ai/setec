// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package frontend

import (
	"bufio"
	"context"
	"errors"
	"io"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	setecv1grpc "github.com/zeroroot-ai/setec/api/grpc/v1"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// apiIsNotFound is a thin alias so the lease backend reads cleanly.
func apiIsNotFound(err error) bool { return apierrors.IsNotFound(err) }

// leaseTokenFor binds a manager-local lease id to the tenant namespace so
// the token is self-describing and a lease cannot be replayed against a
// different tenant. Form: <namespace>|<lease-id>.
func leaseTokenFor(ns, leaseID string) string {
	return ns + "|" + leaseID
}

// parseLeaseToken splits a lease token into namespace and manager-local id.
func parseLeaseToken(tok string) (ns, leaseID string, err error) {
	parts := strings.SplitN(tok, "|", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", status.Errorf(codes.InvalidArgument,
			"lease_id %q is malformed", tok)
	}
	return parts[0], parts[1], nil
}

// relayExecLogs forwards Pod log bytes as ExecResponse output chunks. A
// client cancel becomes a clean return; the final done message is sent by
// the caller, not here.
func relayExecLogs(ctx context.Context, r io.Reader, stream setecv1grpc.LeaseService_ExecServer) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), maxLogLineBytes)
	for scanner.Scan() {
		// Every log read asks the kubelet for timestamps, so a resumed
		// read can position itself. The exec caller wants the workload's
		// own bytes, so the stamp comes off here.
		_, payload, _ := splitLogTimestamp(scanner.Bytes())
		chunk := &setecv1grpc.ExecResponse{
			Data:   append(append([]byte(nil), payload...), '\n'),
			Stream: "stdout",
		}
		if err := stream.Send(chunk); err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
				return nil
			}
			return err
		}
	}
	return scanner.Err()
}
