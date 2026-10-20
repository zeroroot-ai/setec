// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

// Command client is the caller of the federation exit test (setec#251,
// ADR-0164). It runs in a Pod of one trust domain, takes its SVID from the
// SPIRE agent of that domain, and calls the frontend of the setec fleet in
// another trust domain. It reports whether the frontend accepted it.
//
// The call is Wait on a sandbox ID that is not valid. A frontend that
// accepts the caller answers InvalidArgument. A frontend that refuses the
// caller ends the TLS handshake, and the call fails with Unavailable.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/zeroroot-ai/setec/api/grpc/v1"
	"github.com/zeroroot-ai/setec/internal/credentials"
)

func main() {
	var (
		socket   = flag.String("spiffe-socket", "", "Workload API socket of the SPIRE agent of this domain")
		addr     = flag.String("addr", "", "host:port of the frontend")
		serverID = flag.String("server-spiffe-id", "", "SPIFFE ID of the frontend")
		expect   = flag.String("expect", "accepted", "accepted or refused")
		window   = flag.Duration("window", 5*time.Minute, "how long to try")
	)
	flag.Parse()
	if *socket == "" || *addr == "" || *serverID == "" {
		fmt.Fprintln(os.Stderr, "client: --spiffe-socket, --addr and --server-spiffe-id are required")
		os.Exit(2)
	}
	switch *expect {
	case "accepted":
		os.Exit(runAccepted(*socket, *addr, *serverID, *window))
	case "refused":
		os.Exit(runRefused(*socket, *addr, *serverID, *window))
	default:
		fmt.Fprintf(os.Stderr, "client: --expect must be accepted or refused, got %q\n", *expect)
		os.Exit(2)
	}
}

// attempt makes one call. It returns the gRPC code of the answer and the
// error text, or an error when the credentials are not there yet.
func attempt(ctx context.Context, socket, addr, serverID string) (codes.Code, string, error) {
	p, err := credentials.New(credentials.SPIFFESource{SocketPath: socket, AuthorizedIDs: []string{serverID}})
	if err != nil {
		return codes.OK, "", fmt.Errorf("credentials: %w", err)
	}
	creds, err := p.ClientCredentials(ctx)
	if err != nil {
		return codes.OK, "", fmt.Errorf("client credentials: %w", err)
	}
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(creds))
	if err != nil {
		return codes.OK, "", err
	}
	defer func() { _ = conn.Close() }()
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	_, err = pb.NewSandboxServiceClient(conn).Wait(cctx, &pb.WaitRequest{Tenant: "federation", SandboxId: "not-a-sandbox-id"})
	st, _ := status.FromError(err)
	return st.Code(), err.Error(), nil
}

// runAccepted tries until the frontend answers at the application level.
func runAccepted(socket, addr, serverID string, window time.Duration) int {
	deadline := time.Now().Add(window)
	for {
		code, msg, err := attempt(context.Background(), socket, addr, serverID)
		switch {
		case err != nil:
			fmt.Printf("waiting: %v\n", err)
		case code == codes.Unavailable || code == codes.Unauthenticated || code == codes.DeadlineExceeded:
			fmt.Printf("waiting: %s: %s\n", code, msg)
		default:
			fmt.Printf("ACCEPTED code=%s\n", code)
			return 0
		}
		if time.Now().After(deadline) {
			fmt.Println("FAILED: the frontend never accepted this caller")
			return 1
		}
		time.Sleep(5 * time.Second)
	}
}

// runRefused needs the caller to hold its SVID and to be refused by the
// frontend, on each try of the window. An error before the call (no SVID
// yet) is not a refusal, so it is retried until it passes.
func runRefused(socket, addr, serverID string, window time.Duration) int {
	deadline := time.Now().Add(window)
	refused := 0
	for time.Now().Before(deadline) {
		code, msg, err := attempt(context.Background(), socket, addr, serverID)
		switch {
		case err != nil:
			fmt.Printf("waiting: %v\n", err)
		case code == codes.Unavailable && strings.Contains(msg, "tls"):
			refused++
			fmt.Printf("REFUSED %d code=%s: %s\n", refused, code, msg)
		default:
			fmt.Printf("FAILED: the frontend did not refuse this caller: code=%s: %s\n", code, msg)
			return 1
		}
		time.Sleep(5 * time.Second)
	}
	if refused < 3 {
		fmt.Printf("FAILED: %d refusals, want at least 3\n", refused)
		return 1
	}
	return 0
}
