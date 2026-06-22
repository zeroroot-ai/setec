// SPDX-License-Identifier: Apache-2.0
//
// ci-sandbox demonstrates running a Node.js test suite from a local project
// directory inside a Setec-managed Firecracker microVM.
//
// Usage:
//
//	ci-sandbox \
//	  --addr=setec-frontend.example.com:8443 \
//	  --client-cert=./client.crt \
//	  --client-key=./client.key \
//	  --ca=./ca.crt \
//	  --project=./my-node-app \
//	  --command='npm test'
//
// The program packages the project directory as a gzipped tarball in memory,
// inlines it into the sandbox command as a base64 payload, runs the provided
// shell command inside the extracted workspace, and exits with the sandbox's
// exit code.
package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
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
	project    string
	command    string
	image      string
	vcpu       uint
	memoryMiB  uint
	timeout    time.Duration
	dialWait   time.Duration
	maxBytes   int64
}

func parseFlags() (options, error) {
	var o options
	flag.StringVar(&o.addr, "addr", "localhost:8443", "address of the Setec gRPC frontend")
	flag.StringVar(&o.clientCert, "client-cert", "", "path to client TLS certificate (PEM)")
	flag.StringVar(&o.clientKey, "client-key", "", "path to client TLS private key (PEM)")
	flag.StringVar(&o.caCert, "ca", "", "path to the CA certificate that signed the frontend's cert (PEM)")
	flag.StringVar(&o.project, "project", ".", "local project directory to package and ship to the sandbox")
	flag.StringVar(&o.command, "command", "npm test", "shell command to run inside the extracted workspace")
	flag.StringVar(&o.image, "image", "docker.io/library/node:20-slim", "OCI image for the sandbox")
	flag.UintVar(&o.vcpu, "vcpu", 2, "vCPUs allocated to the sandbox")
	flag.UintVar(&o.memoryMiB, "memory-mib", 2048, "memory allocated to the sandbox in MiB")
	flag.DurationVar(&o.timeout, "timeout", 30*time.Minute, "sandbox lifecycle timeout")
	flag.DurationVar(&o.dialWait, "dial-timeout", 15*time.Second, "gRPC dial timeout")
	flag.Int64Var(&o.maxBytes, "max-bytes", 32*1024*1024, "hard cap on packaged tarball size before refusing to launch")
	flag.Parse()

	if o.clientCert == "" || o.clientKey == "" || o.caCert == "" {
		return o, errors.New("--client-cert, --client-key, and --ca are required")
	}
	if o.project == "" {
		return o, errors.New("--project is required")
	}
	return o, nil
}

func packageDir(root string, max int64) ([]byte, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve project path: %w", err)
	}
	info, err := os.Stat(absRoot)
	if err != nil {
		return nil, fmt.Errorf("stat project: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", absRoot)
	}

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	walkErr := filepath.Walk(absRoot, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		// Skip VCS and node_modules to keep the payload small.
		rel, relErr := filepath.Rel(absRoot, path)
		if relErr != nil {
			return relErr
		}
		if rel == "." {
			return nil
		}
		first := strings.Split(filepath.ToSlash(rel), "/")[0]
		if first == ".git" || first == "node_modules" {
			if fi.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		hdr, err := tar.FileInfoHeader(fi, "")
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if fi.Mode().IsRegular() {
			f, oerr := os.Open(path)
			if oerr != nil {
				return oerr
			}
			defer f.Close()
			if _, cerr := io.Copy(tw, f); cerr != nil {
				return cerr
			}
		}
		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("walk project tree: %w", walkErr)
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	if int64(buf.Len()) > max {
		return nil, fmt.Errorf("packaged workspace is %d bytes, exceeds --max-bytes=%d", buf.Len(), max)
	}
	return buf.Bytes(), nil
}

func buildShellScript(tarGz []byte, command string) string {
	// The sandbox sees the payload as a base64 string embedded in a
	// here-document. We decode it, extract into /workspace, and exec the
	// provided command.
	payload := base64.StdEncoding.EncodeToString(tarGz)
	return fmt.Sprintf(`set -eu
mkdir -p /workspace
echo %q | base64 -d | tar -xz -C /workspace
cd /workspace
%s
`, payload, command)
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

func run(ctx context.Context, o options, out io.Writer) (int, error) {
	tarGz, err := packageDir(o.project, o.maxBytes)
	if err != nil {
		return 2, err
	}
	fmt.Fprintf(os.Stderr, "ci-sandbox: packaged %s -> %d bytes\n", o.project, len(tarGz))

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
		Command: []string{"sh", "-c", buildShellScript(tarGz, o.command)},
		Resources: &pb.Resources{
			Vcpu:   uint32(o.vcpu),
			Memory: fmt.Sprintf("%dMi", o.memoryMiB),
		},
		Lifecycle: &pb.Lifecycle{Timeout: o.timeout.String()},
	})
	if err != nil {
		return 2, fmt.Errorf("launch sandbox: %w", err)
	}
	fmt.Fprintf(os.Stderr, "ci-sandbox: launched %s\n", launch.GetSandboxId())

	logs, err := client.StreamLogs(ctx, &pb.StreamLogsRequest{
		SandboxId: launch.GetSandboxId(),
		Follow:    true,
	})
	if err != nil {
		return 2, fmt.Errorf("stream logs: %w", err)
	}
	for {
		chunk, recvErr := logs.Recv()
		if errors.Is(recvErr, io.EOF) {
			break
		}
		if recvErr != nil {
			return 2, fmt.Errorf("log stream: %w", recvErr)
		}
		if _, werr := out.Write(chunk.GetData()); werr != nil {
			return 2, fmt.Errorf("write log chunk: %w", werr)
		}
	}

	wait, err := client.Wait(ctx, &pb.WaitRequest{SandboxId: launch.GetSandboxId()})
	if err != nil {
		return 2, fmt.Errorf("wait sandbox: %w", err)
	}
	fmt.Fprintf(os.Stderr, "ci-sandbox: phase=%s exit=%d reason=%q\n",
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

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	code, err := run(ctx, opts, os.Stdout)
	if err != nil {
		log.Printf("ci-sandbox: %v", err)
	}
	os.Exit(code)
}
