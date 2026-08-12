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

// Command setec-guest-agent is the tiny in-guest daemon that receives
// fresh entropy from the setec node-agent after a snapshot restore and
// credits it into the kernel CRNG via the RNDADDENTROPY ioctl
// (setec#72). Without it, every microVM restored from the same
// Snapshot resumes with an identical CSPRNG state — catastrophic for
// keys and nonces minted right after resume.
//
// The agent listens on an AF_VSOCK port (default 2600) reachable
// through the vsock device setec-pool-vm attaches at boot. Guest image
// builders must bundle this binary in the microVM rootfs and start it
// early (before any workload that consumes randomness); the node-agent
// refuses to hand over a restored sandbox until the agent has
// acknowledged the reseed, unless the operator explicitly opted out
// with --entropy-reseed=off.
//
// The binary is static, dependency-light, and speaks nothing but the
// setec entropy-reseed wire protocol (internal/entropy). It never
// initiates connections and exposes no other surface to the guest
// workload.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/zeroroot-ai/setec/internal/entropy"
	"github.com/zeroroot-ai/setec/internal/uniquify"
)

// Options carries the agent's flag values.
type Options struct {
	// Port is the AF_VSOCK port to listen on for entropy-reseed
	// requests.
	Port uint32
	// UniquifyPort is the AF_VSOCK port to listen on for per-restore
	// identity uniquification directives (ADR-0005 invariant 2,
	// setec#189).
	UniquifyPort uint32
	// RandomDevice is the device node the RNDADDENTROPY ioctl is
	// issued against.
	RandomDevice string
}

func parseFlags(args []string) (Options, error) {
	fs := flag.NewFlagSet("setec-guest-agent", flag.ContinueOnError)
	var port, uniquifyPort uint
	var dev string
	fs.UintVar(&port, "vsock-port", uint(entropy.DefaultVsockPort),
		"AF_VSOCK port to listen on for entropy-reseed requests")
	fs.UintVar(&uniquifyPort, "uniquify-vsock-port", uint(uniquify.DefaultVsockPort),
		"AF_VSOCK port to listen on for restore-uniquification directives")
	fs.StringVar(&dev, "random-device", "/dev/urandom",
		"device node the RNDADDENTROPY ioctl is issued against")
	if err := fs.Parse(args); err != nil {
		return Options{}, err
	}
	if port == 0 || port > 0xFFFFFFFF {
		return Options{}, fmt.Errorf("--vsock-port must be in 1..2^32-1, got %d", port)
	}
	if uniquifyPort == 0 || uniquifyPort > 0xFFFFFFFF || uniquifyPort == port {
		return Options{}, fmt.Errorf(
			"--uniquify-vsock-port must be in 1..2^32-1 and differ from --vsock-port, got %d", uniquifyPort)
	}
	return Options{Port: uint32(port), UniquifyPort: uint32(uniquifyPort), RandomDevice: dev}, nil
}

// run serves reseed requests from ln until ctx is cancelled. Split
// from main so the loop is unit-testable with any net.Listener.
func run(ctx context.Context, ln net.Listener, pool entropy.Pool, logf func(string, ...any)) error {
	h := &entropy.GuestHandler{Pool: pool, Logf: logf}
	return h.Serve(ctx, ln)
}

// runUniquify serves restore-uniquification directives from ln until
// ctx is cancelled. Split from main so the loop is unit-testable with
// any net.Listener and injected appliers.
func runUniquify(
	ctx context.Context,
	ln net.Listener,
	identity uniquify.IdentityApplier,
	network uniquify.NetworkReconciler,
	cid uniquify.CIDReporter,
	logf func(string, ...any),
) error {
	h := &uniquify.GuestHandler{Identity: identity, Network: network, CID: cid, Logf: logf}
	return h.Serve(ctx, ln)
}

func main() {
	opts, err := parseFlags(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	ln, err := listenVsock(opts.Port)
	if err != nil {
		log.Printf("setec-guest-agent: listen vsock port %d: %v", opts.Port, err)
		os.Exit(1)
	}
	uln, err := listenVsock(opts.UniquifyPort)
	if err != nil {
		log.Printf("setec-guest-agent: listen vsock port %d: %v", opts.UniquifyPort, err)
		os.Exit(1)
	}
	log.Printf("setec-guest-agent: listening on vsock ports %d (entropy) and %d (uniquify), random device %s",
		opts.Port, opts.UniquifyPort, opts.RandomDevice)

	errCh := make(chan error, 2)
	go func() {
		errCh <- runUniquify(ctx, uln,
			uniquify.NewLinuxIdentity(), uniquify.NewLinuxNetwork(), uniquify.VsockCID{}, log.Printf)
	}()

	pool := newKernelPool(opts.RandomDevice)
	go func() { errCh <- run(ctx, ln, pool, log.Printf) }()

	if err := <-errCh; err != nil {
		log.Printf("setec-guest-agent: %v", err)
		os.Exit(1)
	}
}
