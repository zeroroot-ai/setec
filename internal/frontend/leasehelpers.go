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
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := append(scanner.Bytes(), '\n')
		chunk := &setecv1grpc.ExecResponse{
			Data:   append([]byte(nil), line...),
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
