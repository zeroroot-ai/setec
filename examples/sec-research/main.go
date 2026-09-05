// SPDX-License-Identifier: Apache-2.0
//
// sec-research demonstrates running an AFL++ fuzzer against a local target
// binary inside a Setec-managed Firecracker microVM. The sandbox is given a
// hard lifecycle timeout (default 1 hour) and capped CPU/memory; the program
// streams fuzzer progress and (optionally) dumps any crash artefacts at the
// end of the run.
//
// Usage:
//
//	sec-research \
//	  --addr=setec-frontend.example.com:8443 \
//	  --client-cert=./client.crt \
//	  --client-key=./client.key \
//	  --ca=./ca.crt \
//	  --target=./path/to/target_binary \
//	  --seed-dir=./seeds
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
	target     string
	seedDir    string
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
	flag.StringVar(&o.target, "target", "", "path to the instrumented target binary to fuzz")
	flag.StringVar(&o.seedDir, "seed-dir", "", "optional directory of seed inputs (defaults to a single empty seed)")
	flag.StringVar(&o.image, "image", "docker.io/aflplusplus/aflplusplus:latest", "OCI image for the sandbox")
	flag.UintVar(&o.vcpu, "vcpu", 2, "vCPUs allocated to the sandbox")
	flag.UintVar(&o.memoryMiB, "memory-mib", 2048, "memory allocated to the sandbox in MiB")
	flag.DurationVar(&o.timeout, "timeout", time.Hour, "sandbox lifecycle timeout")
	flag.DurationVar(&o.dialWait, "dial-timeout", 15*time.Second, "gRPC dial timeout")
	flag.Parse()

	if o.clientCert == "" || o.clientKey == "" || o.caCert == "" {
		return o, errors.New("--client-cert, --client-key, and --ca are required")
	}
	if o.target == "" {
		return o, errors.New("--target is required")
	}
	return o, nil
}

func packageBundle(target, seedDir string) ([]byte, error) {
	if _, err := os.Stat(target); err != nil {
		return nil, fmt.Errorf("stat target: %w", err)
	}

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	// Add the target binary as ./target.
	if err := addFile(tw, target, "target", 0o755); err != nil {
		return nil, err
	}

	// Add seeds, or a single empty seed if none supplied.
	if seedDir == "" {
		if err := addEmptySeed(tw); err != nil {
			return nil, err
		}
	} else {
		err := filepath.Walk(seedDir, func(path string, fi os.FileInfo, werr error) error {
			if werr != nil {
				return werr
			}
			if fi.IsDir() {
				return nil
			}
			rel, rerr := filepath.Rel(seedDir, path)
			if rerr != nil {
				return rerr
			}
			return addFile(tw, path, "seeds/"+filepath.ToSlash(rel), 0o644)
		})
		if err != nil {
			return nil, fmt.Errorf("walk seeds: %w", err)
		}
	}

	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func addFile(tw *tar.Writer, path, name string, mode int64) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	hdr := &tar.Header{
		Name:    name,
		Size:    fi.Size(),
		Mode:    mode,
		ModTime: fi.ModTime(),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	_, err = io.Copy(tw, f)
	return err
}

func addEmptySeed(tw *tar.Writer) error {
	hdr := &tar.Header{
		Name:    "seeds/empty",
		Size:    1,
		Mode:    0o644,
		ModTime: time.Now(),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	_, err := tw.Write([]byte{0})
	return err
}

func buildShellScript(payload []byte, timeout time.Duration) string {
	b64 := base64.StdEncoding.EncodeToString(payload)
	// Give AFL++ a budget slightly under the sandbox lifecycle so the fuzzer
	// exits voluntarily and its crash corpus prints to stdout.
	fuzzSeconds := int(timeout.Seconds()) - 30
	if fuzzSeconds < 60 {
		fuzzSeconds = 60
	}
	return fmt.Sprintf(`set -eu
mkdir -p /work && cd /work
echo %q | base64 -d | tar -xz
chmod +x ./target
mkdir -p findings
echo "sec-research: starting AFL++ for %d seconds"
timeout %ds afl-fuzz -i seeds -o findings -- ./target @@ || true
echo "sec-research: fuzz run finished; crashes:"
if compgen -G "findings/default/crashes/id:*" >/dev/null; then
  for c in findings/default/crashes/id:*; do
    echo "--- $c ---"
    base64 "$c" | head -c 4096
    echo
  done
else
  echo "(none)"
fi
`, b64, fuzzSeconds, fuzzSeconds)
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
	payload, err := packageBundle(o.target, o.seedDir)
	if err != nil {
		return 2, err
	}
	fmt.Fprintf(os.Stderr, "sec-research: packaged target + seeds -> %d bytes\n", len(payload))

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
		Command: []string{"sh", "-c", buildShellScript(payload, o.timeout)},
		Resources: &pb.Resources{
			Vcpu:   uint32(o.vcpu),
			Memory: fmt.Sprintf("%dMi", o.memoryMiB),
		},
		Network: &pb.Network{Mode: "none"}, // Fuzzing does not need network.
		Lifecycle: &pb.Lifecycle{
			Timeout: o.timeout.String(),
		},
	})
	if err != nil {
		return 2, fmt.Errorf("launch sandbox: %w", err)
	}
	fmt.Fprintf(os.Stderr, "sec-research: launched %s\n", launch.GetSandboxId())

	logs, err := client.StreamLogs(ctx, &pb.StreamLogsRequest{
		SandboxId: launch.GetSandboxId(),
		Follow:    true,
	})
	if err != nil {
		return 2, fmt.Errorf("stream logs: %w", err)
	}
	var tail strings.Builder
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
		tail.Write(chunk.GetData())
	}

	wait, err := client.Wait(ctx, &pb.WaitRequest{SandboxId: launch.GetSandboxId()})
	if err != nil {
		return 2, fmt.Errorf("wait sandbox: %w", err)
	}
	fmt.Fprintf(os.Stderr, "sec-research: phase=%s exit=%d reason=%q\n",
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
		log.Printf("sec-research: %v", err)
	}
	os.Exit(code)
}
