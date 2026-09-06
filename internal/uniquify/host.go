// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package uniquify

import (
	"bufio"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Uniquifier is the host-side surface the node-agent invokes after a
// snapshot restore (and after the entropy reseed). Implementations
// MUST only return a nil error once the guest has verifiably applied
// the directed identity — a nil return is what lets a restored
// sandbox be reported Ready.
type Uniquifier interface {
	Uniquify(ctx context.Context, vsockUDSPath string, spec Spec) (Report, error)
}

// NewSpec mints a fresh per-restore identity: a random machine-id and
// boot-id from crypto/rand plus the caller-provided hostname and
// expected Pod IP. Because every field is freshly generated (or
// derives from the unique Sandbox), any two restores from the same
// template necessarily differ.
func NewSpec(hostname, podIP string) (Spec, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return Spec{}, fmt.Errorf("uniquify: gather machine-id entropy: %w", err)
	}
	return Spec{
		MachineID: fmt.Sprintf("%x", raw),
		BootID:    uuid.NewString(),
		Hostname:  SanitizeHostname(hostname),
		PodIP:     podIP,
	}, nil
}

// hostnameStrip matches every character not allowed in a hostname
// label.
var hostnameStrip = regexp.MustCompile(`[^a-z0-9-]`)

// SanitizeHostname reduces an arbitrary identifier (typically the
// Sandbox name) to a valid hostname label: lowercase alphanumerics
// and dashes, at most 63 characters, non-empty.
func SanitizeHostname(name string) string {
	s := hostnameStrip.ReplaceAllString(strings.ToLower(name), "-")
	s = strings.Trim(s, "-")
	if len(s) > 63 {
		s = strings.Trim(s[:63], "-")
	}
	if s == "" {
		s = "setec-sandbox"
	}
	return s
}

// Verify checks a guest report against the spec that was sent and the
// raw spec bytes it was framed as. It is the single fail-closed gate:
// any mismatch means the restore must not be handed to a caller.
func Verify(spec Spec, rawSpec []byte, report Report) error {
	if report.Status != StatusOK {
		return fmt.Errorf("uniquify: guest failed to apply identity (status=%d): %s", report.Status, report.Error)
	}
	if report.Digest != DigestHex(rawSpec) {
		return errors.New("uniquify: report digest does not match the directive sent; refusing to trust the ack")
	}
	if report.MachineID != spec.MachineID {
		return fmt.Errorf("uniquify: guest machine-id %q does not match the directed value", report.MachineID)
	}
	if report.BootID != spec.BootID {
		return fmt.Errorf("uniquify: guest boot-id %q does not match the directed value", report.BootID)
	}
	if report.Hostname != spec.Hostname {
		return fmt.Errorf("uniquify: guest hostname %q does not match the directed value %q", report.Hostname, spec.Hostname)
	}
	if spec.PodIP != "" {
		if !slices.Contains(report.ObservedIPs, spec.PodIP) {
			return fmt.Errorf("uniquify: guest does not observe its Pod IP %s (observed %v)", spec.PodIP, report.ObservedIPs)
		}
	}
	if report.GuestCID == 0 {
		return errors.New("uniquify: guest could not report its vsock CID")
	}
	return nil
}

// VsockUniquifier pushes the identity directive over the Firecracker
// hybrid-vsock Unix socket: it dials udsPath, performs the
// "CONNECT <port>\n" / "OK <n>\n" handshake Firecracker (and Kata's
// hybrid vsock) use for host-initiated connections, sends the Spec,
// and verifies the guest's Report.
type VsockUniquifier struct {
	// Port is the guest AF_VSOCK port setec-guest-agent listens on
	// for uniquification directives.
	Port uint32

	// DialTimeout bounds the Unix-socket dial plus the vsock
	// handshake plus the request/report exchange when the caller's
	// ctx carries no earlier deadline.
	DialTimeout time.Duration
}

// NewVsockUniquifier returns a VsockUniquifier with production
// defaults.
func NewVsockUniquifier() *VsockUniquifier {
	return &VsockUniquifier{
		Port:        DefaultVsockPort,
		DialTimeout: 5 * time.Second,
	}
}

// Uniquify sends spec to the guest reachable via the Firecracker
// vsock UDS at udsPath and fails unless the guest's report passes
// Verify.
func (u *VsockUniquifier) Uniquify(ctx context.Context, udsPath string, spec Spec) (Report, error) {
	port := u.Port
	if port == 0 {
		port = DefaultVsockPort
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		timeout := u.DialTimeout
		if timeout <= 0 {
			timeout = 5 * time.Second
		}
		deadline = time.Now().Add(timeout)
	}

	d := net.Dialer{Deadline: deadline}
	conn, err := d.DialContext(ctx, "unix", udsPath)
	if err != nil {
		return Report{}, fmt.Errorf("uniquify: dial vsock uds %q: %w", udsPath, err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(deadline)

	// Firecracker hybrid-vsock handshake for host-initiated
	// connections: CONNECT <guest-port>, expect "OK <host-port>".
	if _, err := fmt.Fprintf(conn, "CONNECT %d\n", port); err != nil {
		return Report{}, fmt.Errorf("uniquify: vsock CONNECT: %w", err)
	}
	br := bufio.NewReader(conn)
	line, err := br.ReadString('\n')
	if err != nil {
		return Report{}, fmt.Errorf("uniquify: vsock handshake (guest agent not listening on port %d?): %w", port, err)
	}
	if !strings.HasPrefix(line, "OK ") {
		return Report{}, fmt.Errorf("uniquify: vsock handshake rejected: %q", strings.TrimSpace(line))
	}

	raw, err := WriteSpec(conn, spec)
	if err != nil {
		return Report{}, err
	}
	report, err := ReadReport(br)
	if err != nil {
		return Report{}, fmt.Errorf("uniquify: guest did not report: %w", err)
	}
	if err := Verify(spec, raw, report); err != nil {
		return report, err
	}
	return report, nil
}

// UniquifyFirst tries each candidate vsock UDS path in order and
// returns the first verified report. It fails when the candidate list
// is empty or every candidate fails — callers treat that as a
// fail-closed restore.
func UniquifyFirst(ctx context.Context, u Uniquifier, candidates []string, spec Spec) (Report, error) {
	if len(candidates) == 0 {
		return Report{}, errors.New("uniquify: no vsock UDS candidates to uniquify through")
	}
	var errs []error
	for _, path := range candidates {
		report, err := u.Uniquify(ctx, path, spec)
		if err == nil {
			return report, nil
		}
		errs = append(errs, fmt.Errorf("%s: %w", path, err))
	}
	return Report{}, fmt.Errorf("uniquify: failed on every candidate: %w", errors.Join(errs...))
}
