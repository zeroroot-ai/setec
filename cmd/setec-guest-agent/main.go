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
)

// Options carries the agent's flag values.
type Options struct {
	// Port is the AF_VSOCK port to listen on.
	Port uint32
	// RandomDevice is the device node the RNDADDENTROPY ioctl is
	// issued against.
	RandomDevice string
}

func parseFlags(args []string) (Options, error) {
	fs := flag.NewFlagSet("setec-guest-agent", flag.ContinueOnError)
	var port uint
	var dev string
	fs.UintVar(&port, "vsock-port", uint(entropy.DefaultVsockPort),
		"AF_VSOCK port to listen on for entropy-reseed requests")
	fs.StringVar(&dev, "random-device", "/dev/urandom",
		"device node the RNDADDENTROPY ioctl is issued against")
	if err := fs.Parse(args); err != nil {
		return Options{}, err
	}
	if port == 0 || port > 0xFFFFFFFF {
		return Options{}, fmt.Errorf("--vsock-port must be in 1..2^32-1, got %d", port)
	}
	return Options{Port: uint32(port), RandomDevice: dev}, nil
}

// run serves reseed requests from ln until ctx is cancelled. Split
// from main so the loop is unit-testable with any net.Listener.
func run(ctx context.Context, ln net.Listener, pool entropy.Pool, logf func(string, ...any)) error {
	h := &entropy.GuestHandler{Pool: pool, Logf: logf}
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
	log.Printf("setec-guest-agent: listening on vsock port %d (random device %s)", opts.Port, opts.RandomDevice)

	pool := newKernelPool(opts.RandomDevice)
	if err := run(ctx, ln, pool, log.Printf); err != nil {
		log.Printf("setec-guest-agent: %v", err)
		os.Exit(1)
	}
}
