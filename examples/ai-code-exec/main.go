// SPDX-License-Identifier: Apache-2.0
//
// ai-code-exec demonstrates running LLM-generated Python code inside a
// Setec-managed Firecracker microVM.
//
// Usage:
//
//	cat script.py | ai-code-exec \
//	  --addr=setec-frontend.example.com:8443 \
//	  --client-cert=./client.crt \
//	  --client-key=./client.key \
//	  --ca=./ca.crt
//
// The program reads Python source from stdin, launches a sandbox running
// `python3 -c <source>`, streams the sandbox's logs to stdout, and exits with
// the sandbox's exit code.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	pb "github.com/zeroroot-ai/setec/api/grpc/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

type options struct {
	addr       string
	clientCert string
	clientKey  string
	caCert     string
	image      string
	vcpu       uint
	memoryMiB  uint
	timeout    time.Duration
	dialWait   time.Duration
}

func parseFlags() (options, error) {
	var o options
	flag.StringVar(&o.addr, "addr", "localhost:8443", "address of the Setec gRPC frontend")
	flag.StringVar(&o.clientCert, "client-cert", "", "path to client TLS certificate (PEM)")
	flag.StringVar(&o.clientKey, "client-key", "", "path to client TLS private key (PEM)")
	flag.StringVar(&o.caCert, "ca", "", "path to the CA certificate that signed the frontend's cert (PEM)")
	flag.StringVar(&o.image, "image", "docker.io/library/python:3.12-slim", "OCI image for the sandbox")
	flag.UintVar(&o.vcpu, "vcpu", 1, "vCPUs allocated to the sandbox")
	flag.UintVar(&o.memoryMiB, "memory-mib", 512, "memory allocated to the sandbox in MiB")
	flag.DurationVar(&o.timeout, "timeout", 5*time.Minute, "sandbox lifecycle timeout")
	flag.DurationVar(&o.dialWait, "dial-timeout", 15*time.Second, "gRPC dial timeout")
	flag.Parse()

	if o.clientCert == "" || o.clientKey == "" || o.caCert == "" {
		return o, errors.New("--client-cert, --client-key, and --ca are required")
	}
	return o, nil
}

func loadTLS(o options) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(o.clientCert, o.clientKey)
	if err != nil {
		return nil, fmt.Errorf("load client keypair: %w", err)
	}
	ca, err := os.ReadFile(o.caCert)
	if err != nil {
		return nil, fmt.Errorf("read CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, errors.New("CA file contained no valid PEM certificates")
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,
		MinVersion:   tls.VersionTLS12,
	}, nil
}

func run(ctx context.Context, o options, source []byte, out io.Writer) (int, error) {
	tlsCfg, err := loadTLS(o)
	if err != nil {
		return 2, err
	}

	dialCtx, cancel := context.WithTimeout(ctx, o.dialWait)
	defer cancel()
	conn, err := grpc.DialContext(dialCtx, o.addr,
		grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)),
		grpc.WithBlock(),
	)
	if err != nil {
		return 2, fmt.Errorf("dial frontend: %w", err)
	}
	defer conn.Close()

	client := pb.NewSandboxServiceClient(conn)

	launch, err := client.Launch(ctx, &pb.LaunchRequest{
		Image:   o.image,
		Command: []string{"python3", "-c", string(source)},
		Resources: &pb.Resources{
			Vcpu:   uint32(o.vcpu),
			Memory: fmt.Sprintf("%dMi", o.memoryMiB),
		},
		Lifecycle: &pb.Lifecycle{Timeout: o.timeout.String()},
	})
	if err != nil {
		return 2, fmt.Errorf("launch sandbox: %w", err)
	}
	fmt.Fprintf(os.Stderr, "ai-code-exec: launched %s\n", launch.GetSandboxId())

	logs, err := client.StreamLogs(ctx, &pb.StreamLogsRequest{
		SandboxId: launch.GetSandboxId(),
		Follow:    true,
	})
	if err != nil {
		return 2, fmt.Errorf("stream logs: %w", err)
	}
	for {
		chunk, err := logs.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return 2, fmt.Errorf("log stream: %w", err)
		}
		if _, werr := out.Write(chunk.GetData()); werr != nil {
			return 2, fmt.Errorf("write log chunk: %w", werr)
		}
	}

	wait, err := client.Wait(ctx, &pb.WaitRequest{SandboxId: launch.GetSandboxId()})
	if err != nil {
		return 2, fmt.Errorf("wait sandbox: %w", err)
	}
	fmt.Fprintf(os.Stderr, "ai-code-exec: phase=%s exit=%d reason=%q\n",
		wait.GetPhase(), wait.GetExitCode(), wait.GetReason())

	return int(wait.GetExitCode()), nil
}

func main() {
	opts, err := parseFlags()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		flag.Usage()
		os.Exit(2)
	}

	source, err := io.ReadAll(os.Stdin)
	if err != nil {
		log.Fatalf("read stdin: %v", err)
	}
	if len(source) == 0 {
		log.Fatal("no Python source provided on stdin")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	code, err := run(ctx, opts, source, os.Stdout)
	if err != nil {
		log.Printf("ai-code-exec: %v", err)
	}
	os.Exit(code)
}
