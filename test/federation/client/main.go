// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

// Command client is the caller of the federation exit test (setec#251).
// It runs in a Pod of one trust domain and takes its SVID from the SPIRE
// agent of that domain. It calls the frontend of the setec fleet in another
// trust domain, and it reports whether the frontend accepted it.
//
// The call is Wait on a sandbox ID that has the right form and names a
// namespace that no pair owns. The frontend checks the enrollment of the
// caller first. A caller that is enrolled gets NotFound or a PermissionDenied
// answer about the namespace. A caller that is not enrolled gets
// PermissionDenied "is not an enrolled client", which is a failure. A frontend
// that refuses the caller in the TLS handshake ends the call with Unavailable.
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
	"github.com/zeroroot-ai/setec/internal/frontend"
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
		try := func(ctx context.Context) (codes.Code, string, error) {
			return attempt(ctx, *socket, *addr, *serverID)
		}
		os.Exit(runAccepted(try, *window, 5*time.Second))
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
		return codes.OK, "", fmt.Errorf("dial the fleet frontend: %w", err)
	}
	defer func() { _ = conn.Close() }()
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req := &pb.WaitRequest{Tenant: "federation", SandboxId: "federation-check/none/none"}
	_, err = pb.NewSandboxServiceClient(conn).Wait(cctx, req)
	if err == nil {
		return codes.OK, "", nil
	}
	st, _ := status.FromError(err)
	return st.Code(), err.Error(), nil
}

// attemptFunc makes one call of the client. A test replaces it.
type attemptFunc func(ctx context.Context) (codes.Code, string, error)

// verdict says what an answer of the frontend proves.
type verdict int

const (
	// retry: the call did not get an answer, or the credentials are not there yet.
	retry verdict = iota
	// enrolled: the caller passed the enrollment check.
	enrolled
	// notEnrolled: the frontend answered that the caller is not enrolled.
	notEnrolled
	// unexpected: any other answer.
	unexpected
)

// classify decides what the answer of the frontend proves. Only an answer
// that comes after the enrollment check counts as enrolled.
func classify(code codes.Code, msg string) verdict {
	switch {
	case code == codes.OK, code == codes.NotFound:
		return enrolled
	case code == codes.PermissionDenied && strings.Contains(msg, frontend.NotEnrolledText):
		return notEnrolled
	case code == codes.PermissionDenied &&
		(strings.Contains(msg, frontend.NoNamespaceText) || strings.Contains(msg, frontend.NotOwnerText)):
		return enrolled
	case code == codes.Unavailable, code == codes.Unauthenticated, code == codes.DeadlineExceeded:
		return retry
	default:
		return unexpected
	}
}

// runAccepted tries until the frontend answers that the caller is enrolled.
// It fails at once on an answer that says the caller is not enrolled, or on
// any answer that proves nothing about the enrollment.
func runAccepted(try attemptFunc, window, pause time.Duration) int {
	deadline := time.Now().Add(window)
	for {
		code, msg, err := try(context.Background())
		switch {
		case err != nil:
			fmt.Printf("waiting: %v\n", err)
		default:
			switch classify(code, msg) {
			case enrolled:
				fmt.Printf("ACCEPTED code=%s: %s\n", code, msg)
				return 0
			case notEnrolled:
				fmt.Printf("FAILED: the caller passed TLS but is not enrolled: code=%s: %s\n", code, msg)
				return 1
			case unexpected:
				fmt.Printf("FAILED: the answer proves no enrollment: code=%s: %s\n", code, msg)
				return 1
			case retry:
				fmt.Printf("waiting: %s: %s\n", code, msg)
			}
		}
		if time.Now().After(deadline) {
			fmt.Println("FAILED: the frontend never accepted this caller")
			return 1
		}
		time.Sleep(pause)
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
		case strings.Contains(msg, "x509") || strings.Contains(msg, "failed to verify"):
			// The client refused the fleet. The fleet did not refuse the client.
			fmt.Printf("FAILED: the client rejected the fleet, not the reverse: %s\n", msg)
			return 1
		case code == codes.Unavailable && strings.Contains(msg, "remote error: tls:"):
			// A TLS alert that the fleet sent.
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
