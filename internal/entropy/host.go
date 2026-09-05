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

package entropy

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"
)

// Reseeder is the host-side surface the node-agent invokes after a
// snapshot restore. Implementations MUST only return nil once the
// guest has verifiably received and injected fresh entropy — a nil
// return is what lets a restored sandbox be reported Ready.
type Reseeder interface {
	Reseed(ctx context.Context, vsockUDSPath string) error
}

// VsockReseeder reseeds a restored guest over the Firecracker
// hybrid-vsock Unix socket: it dials udsPath, performs the
// "CONNECT <port>\n" / "OK <n>\n" handshake Firecracker (and Kata's
// hybrid vsock) use for host-initiated connections, sends
// PayloadBytes of fresh entropy, and verifies the guest's ack digest.
type VsockReseeder struct {
	// Port is the guest AF_VSOCK port setec-guest-agent listens on.
	Port uint32

	// PayloadBytes is how much fresh entropy to push per reseed.
	PayloadBytes int

	// Rand is the entropy source. Defaults to crypto/rand.Reader.
	Rand io.Reader

	// DialTimeout bounds the Unix-socket dial plus the vsock
	// handshake plus the request/ack exchange when the caller's ctx
	// carries no earlier deadline.
	DialTimeout time.Duration
}

// NewVsockReseeder returns a VsockReseeder with production defaults.
func NewVsockReseeder() *VsockReseeder {
	return &VsockReseeder{
		Port:         DefaultVsockPort,
		PayloadBytes: DefaultPayloadBytes,
		Rand:         rand.Reader,
		DialTimeout:  5 * time.Second,
	}
}

// Reseed pushes fresh entropy into the guest reachable via the
// Firecracker vsock UDS at udsPath and fails unless the guest acks
// with the exact SHA-256 of the payload sent.
func (r *VsockReseeder) Reseed(ctx context.Context, udsPath string) error {
	port := r.Port
	if port == 0 {
		port = DefaultVsockPort
	}
	n := r.PayloadBytes
	if n <= 0 {
		n = DefaultPayloadBytes
	}
	src := r.Rand
	if src == nil {
		src = rand.Reader
	}

	deadline, ok := ctx.Deadline()
	if !ok {
		timeout := r.DialTimeout
		if timeout <= 0 {
			timeout = 5 * time.Second
		}
		deadline = time.Now().Add(timeout)
	}

	payload := make([]byte, n)
	if _, err := io.ReadFull(src, payload); err != nil {
		return fmt.Errorf("entropy: gather host entropy: %w", err)
	}
	want := sha256.Sum256(payload)

	d := net.Dialer{Deadline: deadline}
	conn, err := d.DialContext(ctx, "unix", udsPath)
	if err != nil {
		return fmt.Errorf("entropy: dial vsock uds %q: %w", udsPath, err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(deadline)

	// Firecracker hybrid-vsock handshake for host-initiated
	// connections: CONNECT <guest-port>, expect "OK <host-port>".
	if _, err := fmt.Fprintf(conn, "CONNECT %d\n", port); err != nil {
		return fmt.Errorf("entropy: vsock CONNECT: %w", err)
	}
	br := bufio.NewReader(conn)
	line, err := br.ReadString('\n')
	if err != nil {
		return fmt.Errorf("entropy: vsock handshake (guest agent not listening on port %d?): %w", port, err)
	}
	if !strings.HasPrefix(line, "OK ") {
		return fmt.Errorf("entropy: vsock handshake rejected: %q", strings.TrimSpace(line))
	}

	if err := WriteRequest(conn, payload); err != nil {
		return err
	}
	ack, err := ReadAck(br)
	if err != nil {
		return fmt.Errorf("entropy: guest did not ack the reseed: %w", err)
	}
	if ack.Status != StatusOK {
		return fmt.Errorf("entropy: guest failed to inject entropy (status=%d)", ack.Status)
	}
	if ack.Digest != want {
		return errors.New("entropy: ack digest does not match the payload sent; refusing to trust the reseed")
	}
	return nil
}

// ReseedFirst tries each candidate vsock UDS path in order and returns
// nil on the first verified reseed. It fails when the candidate list
// is empty or every candidate fails — callers treat that as a
// fail-closed restore.
func ReseedFirst(ctx context.Context, r Reseeder, candidates []string) error {
	if len(candidates) == 0 {
		return errors.New("entropy: no vsock UDS candidates to reseed through")
	}
	var errs []error
	for _, path := range candidates {
		err := r.Reseed(ctx, path)
		if err == nil {
			return nil
		}
		errs = append(errs, fmt.Errorf("%s: %w", path, err))
	}
	return fmt.Errorf("entropy: reseed failed on every candidate: %w", errors.Join(errs...))
}
