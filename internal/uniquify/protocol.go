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

// Package uniquify implements the per-restore identity uniquification
// channel between the setec host (node-agent) and a restored microVM
// guest (setec-guest-agent), closing the remaining ADR-0005
// invariant-2 residuals after the entropy reseed (setec#72):
//
//   - fresh machine-id, boot-id, and hostname per restore, so any two
//     sandboxes restored from the same snapshot template differ;
//   - the CNI-assigned Pod IP applied in-guest, replacing the stale
//     network identity captured at snapshot time;
//   - the guest's vsock CID reported back so the host can enforce
//     node-local CID uniqueness.
//
// The package mirrors internal/entropy piece for piece:
//
//   - a framed wire protocol (this file) spoken over a byte stream —
//     in production a Firecracker hybrid-vsock connection on a
//     dedicated port;
//   - the guest-side handler (guest.go) that applies the directed
//     identity through narrow, injectable appliers and acks with the
//     SHA-256 of the exact directive bytes it received plus the
//     identity it observed after applying;
//   - the host-side uniquifier (host.go) that mints a fresh identity
//     per restore, pushes it to the guest, and verifies the ack
//     digest and the echoed identity — fail closed on any mismatch.
//
// Everything is dependency-light and transport-agnostic so both ends
// stay unit-testable without a real vsock device.
package uniquify

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
)

const (
	// ProtocolVersion is the wire version byte. Both sides reject
	// anything else.
	ProtocolVersion byte = 1

	// DefaultVsockPort is the guest AF_VSOCK port setec-guest-agent
	// listens on for uniquification directives. Distinct from the
	// entropy-reseed port (2600) so the two exchanges stay
	// independently versioned.
	DefaultVsockPort uint32 = 2601

	// MaxFrameBytes bounds a single framed message so neither side
	// can be made to allocate unboundedly by a misbehaving peer.
	MaxFrameBytes = 64 * 1024
)

// Status codes carried in the report.
const (
	// StatusOK means the guest applied the full directive set.
	StatusOK byte = 0
	// StatusError means the guest received the directive but could
	// not apply it completely.
	StatusError byte = 1
)

var (
	specMagic   = [4]byte{'S', 'U', 'N', 'Q'} // Setec UNiQuify
	reportMagic = [4]byte{'S', 'U', 'N', 'A'} // Setec UNiquify Ack
)

// Spec is the per-restore identity directive the host sends. Every
// field is minted fresh on the host per restore, so verification is
// an exact echo comparison — the same pattern that makes the entropy
// reseed's digest check trustworthy.
type Spec struct {
	// MachineID is the fresh /etc/machine-id value: 32 lowercase hex
	// characters.
	MachineID string `json:"machineId"`

	// BootID is the fresh boot identifier (UUID) the guest must
	// expose in place of the snapshot-time kernel boot_id.
	BootID string `json:"bootId"`

	// Hostname is the fresh hostname the guest must adopt.
	Hostname string `json:"hostname"`

	// PodIP is the CNI-assigned Pod IP the guest's primary interface
	// must observe. Empty means the host has no expected address and
	// the guest only reports what it observes.
	PodIP string `json:"podIp,omitempty"`
}

// Report is the guest's reply. Digest is the SHA-256 (hex) of the
// exact spec frame body received, so a truncated or corrupted
// transfer can never be mistaken for an applied directive. The
// identity fields echo what the guest observes AFTER applying.
type Report struct {
	// Status is StatusOK or StatusError.
	Status byte `json:"status"`

	// Error carries the guest-side failure detail when Status is
	// StatusError.
	Error string `json:"error,omitempty"`

	// Digest is the lowercase-hex SHA-256 of the spec bytes received.
	Digest string `json:"digest"`

	// MachineID, BootID, and Hostname are read back from the guest
	// after applying, not echoed from the request.
	MachineID string `json:"machineId"`
	BootID    string `json:"bootId"`
	Hostname  string `json:"hostname"`

	// ObservedIPs lists the global unicast addresses configured on
	// the guest's interfaces after network reconciliation.
	ObservedIPs []string `json:"observedIps,omitempty"`

	// GuestCID is the guest's local vsock context id
	// (IOCTL_VM_SOCKETS_GET_LOCAL_CID). Zero means the guest could
	// not determine it — the host fails closed on zero.
	GuestCID uint32 `json:"guestCid"`
}

// writeFrame frames body onto w: magic[4] | version[1] | len uint32 BE | body.
func writeFrame(w io.Writer, magic [4]byte, body []byte) error {
	if len(body) == 0 {
		return fmt.Errorf("uniquify: refusing to send an empty frame")
	}
	if len(body) > MaxFrameBytes {
		return fmt.Errorf("uniquify: frame %d exceeds max %d", len(body), MaxFrameBytes)
	}
	header := make([]byte, 0, len(magic)+1+4)
	header = append(header, magic[:]...)
	header = append(header, ProtocolVersion)
	header = binary.BigEndian.AppendUint32(header, uint32(len(body)))
	if _, err := w.Write(header); err != nil {
		return fmt.Errorf("uniquify: write frame header: %w", err)
	}
	if _, err := w.Write(body); err != nil {
		return fmt.Errorf("uniquify: write frame body: %w", err)
	}
	return nil
}

// readFrame reads one frame with the expected magic from r.
func readFrame(r io.Reader, magic [4]byte) ([]byte, error) {
	header := make([]byte, len(magic)+1+4)
	if _, err := io.ReadFull(r, header); err != nil {
		return nil, fmt.Errorf("uniquify: read frame header: %w", err)
	}
	if [4]byte(header[:4]) != magic {
		return nil, fmt.Errorf("uniquify: bad frame magic %q", header[:4])
	}
	if header[4] != ProtocolVersion {
		return nil, fmt.Errorf("uniquify: unsupported protocol version %d", header[4])
	}
	n := binary.BigEndian.Uint32(header[5:9])
	if n == 0 {
		return nil, fmt.Errorf("uniquify: zero-length frame")
	}
	if n > MaxFrameBytes {
		return nil, fmt.Errorf("uniquify: frame length %d exceeds max %d", n, MaxFrameBytes)
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, fmt.Errorf("uniquify: read frame body: %w", err)
	}
	return body, nil
}

// WriteSpec frames a Spec onto w and returns the exact body bytes
// written so the caller can pre-compute the expected ack digest.
func WriteSpec(w io.Writer, spec Spec) ([]byte, error) {
	body, err := json.Marshal(spec)
	if err != nil {
		return nil, fmt.Errorf("uniquify: marshal spec: %w", err)
	}
	if err := writeFrame(w, specMagic, body); err != nil {
		return nil, err
	}
	return body, nil
}

// ReadSpec reads one framed Spec from r, returning both the decoded
// spec and the raw body bytes (for the ack digest).
func ReadSpec(r io.Reader) (Spec, []byte, error) {
	body, err := readFrame(r, specMagic)
	if err != nil {
		return Spec{}, nil, err
	}
	var spec Spec
	if err := json.Unmarshal(body, &spec); err != nil {
		return Spec{}, nil, fmt.Errorf("uniquify: decode spec: %w", err)
	}
	return spec, body, nil
}

// WriteReport frames a Report onto w.
func WriteReport(w io.Writer, report Report) error {
	body, err := json.Marshal(report)
	if err != nil {
		return fmt.Errorf("uniquify: marshal report: %w", err)
	}
	return writeFrame(w, reportMagic, body)
}

// ReadReport reads one framed Report from r.
func ReadReport(r io.Reader) (Report, error) {
	body, err := readFrame(r, reportMagic)
	if err != nil {
		return Report{}, err
	}
	var report Report
	if err := json.Unmarshal(body, &report); err != nil {
		return Report{}, fmt.Errorf("uniquify: decode report: %w", err)
	}
	return report, nil
}

// DigestHex returns the lowercase-hex SHA-256 of b.
func DigestHex(b []byte) string {
	sum := sha256.Sum256(b)
	return fmt.Sprintf("%x", sum)
}
