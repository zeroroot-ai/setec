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
	try := func(ctx context.Context) (codes.Code, string, error) {
		return attempt(ctx, *socket, *addr, *serverID)
	}
	switch *expect {
	case "accepted":
		os.Exit(runAccepted(try, *window, 5*time.Second))
	case "refused":
		os.Exit(runRefused(try, *window, 5*time.Second, refusalFloor))
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

// isTransportFailure reports whether the answer comes from the transport of
// the client (grpc-go) and not from a handler of the fleet. A handler status
// can hold any text, so the prefix of the transport is part of the proof.
func isTransportFailure(code codes.Code, msg string) bool {
	return code == codes.Unavailable &&
		(strings.Contains(msg, "connection error:") || strings.Contains(msg, "authentication handshake failed"))
}

// isServerAlert reports whether the fleet sent a TLS alert at the handshake.
func isServerAlert(code codes.Code, msg string) bool {
	return isTransportFailure(code, msg) && strings.Contains(msg, "remote error: tls:")
}

// isClientTransportError reports a failure of the write or the read of the
// client. The fleet closes after its alert while the client still writes, so
// this proves nothing either way. grpc-go gives it as "connection error: desc
// = ..." or, when the write fails after the connection is up, as "desc =
// write tcp ...: write: broken pipe" (charts run 39455771427). Any other
// Unavailable and any DeadlineExceeded can come from a caller that got in, so
// they are not here.
func isClientTransportError(code codes.Code, msg string) bool {
	if code != codes.Unavailable {
		return false
	}
	text := strings.Contains(msg, "write: broken pipe") || strings.Contains(msg, "connection reset by peer")
	conn := strings.Contains(msg, "connection error:") ||
		strings.Contains(msg, "desc = write tcp ") || strings.Contains(msg, "desc = read tcp ")
	return text && conn
}

// refusalFloor is the number of TLS alerts from the fleet that proves a
// refusal.
const refusalFloor = 3

// runRefused needs the caller to hold its SVID and to get at least floor TLS
// alerts from the fleet. An error before the call (no SVID yet) is retried.
// A broken pipe or a reset connection of the client transport is
// inconclusive: the fleet closes after its alert while the client still
// writes. It counts for nothing and fails nothing. Every other answer fails
// the check, because the caller got in: an answer of the handler, any other
// Unavailable, and a deadline.
func runRefused(try attemptFunc, window, pause time.Duration, floor int) int {
	deadline := time.Now().Add(window)
	refused := 0
	for time.Now().Before(deadline) {
		code, msg, err := try(context.Background())
		switch {
		case err != nil:
			fmt.Printf("waiting: %v\n", err)
		case strings.Contains(msg, "x509") || strings.Contains(msg, "failed to verify"):
			// The client refused the fleet. The fleet did not refuse the client.
			fmt.Printf("FAILED: the client rejected the fleet, not the reverse: %s\n", msg)
			return 1
		case isServerAlert(code, msg):
			// A TLS alert that the fleet sent.
			refused++
			fmt.Printf("REFUSED %d code=%s: %s\n", refused, code, msg)
			if refused >= floor {
				return 0
			}
		case isClientTransportError(code, msg):
			fmt.Printf("inconclusive: code=%s: %s\n", code, msg)
		default:
			fmt.Printf("FAILED: the frontend did not refuse this caller: code=%s: %s\n", code, msg)
			return 1
		}
		time.Sleep(pause)
	}
	fmt.Printf("FAILED: %d refusals, want at least %d\n", refused, floor)
	return 1
}
