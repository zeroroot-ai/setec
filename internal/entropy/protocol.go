// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

// Package entropy implements the active entropy-reseed channel between
// the setec host (node-agent) and a restored microVM guest
// (setec-guest-agent), closing the snapshot-clone RNG-sharing window
// described in SECURITY.md ("Entropy reseed on restore", setec#72).
//
// The package has three cooperating pieces:
//
//   - a tiny framed wire protocol (this file) spoken over a byte
//     stream — in production a Firecracker hybrid-vsock connection;
//   - the guest-side handler (guest.go) that injects the received
//     payload into the kernel entropy pool and acks with the
//     payload's SHA-256, so the host can verify the exact bytes
//     arrived;
//   - the host-side reseeder (host.go) that dials the Firecracker
//     vsock Unix socket, performs the hybrid-vsock CONNECT handshake,
//     sends fresh entropy from crypto/rand, and verifies the ack.
//
// Everything is deliberately dependency-free and transport-agnostic so
// both ends stay unit-testable without a real vsock device.
package entropy

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
)

const (
	// ProtocolVersion is the wire version byte. Both sides reject
	// anything else.
	ProtocolVersion byte = 1

	// DefaultVsockPort is the guest AF_VSOCK port setec-guest-agent
	// listens on and the host reseeder connects to.
	DefaultVsockPort uint32 = 2600

	// DefaultPayloadBytes is the amount of fresh entropy pushed per
	// reseed: 256 bytes = 2048 bits, comfortably above the kernel
	// CRNG's 256-bit security level.
	DefaultPayloadBytes = 256

	// MaxPayloadBytes bounds the request payload so neither side can
	// be made to allocate unboundedly by a misbehaving peer.
	MaxPayloadBytes = 4096
)

// Status codes carried in the ack.
const (
	// StatusOK means the guest wrote the payload into the kernel
	// entropy pool AND credited it.
	StatusOK byte = 0
	// StatusError means the guest received the payload but could not
	// inject it.
	StatusError byte = 1
)

var (
	reqMagic = [4]byte{'S', 'R', 'S', 'D'} // Setec ReSeeD
	ackMagic = [4]byte{'S', 'R', 'S', 'A'} // Setec ReSeed Ack
)

// digestSize is the SHA-256 digest length carried in the ack.
const digestSize = sha256.Size

// Ack is the guest's reply to a reseed request.
type Ack struct {
	// Status is StatusOK or StatusError.
	Status byte
	// Digest is the SHA-256 of the payload the guest received. The
	// host compares it against the payload it sent so a truncated or
	// corrupted transfer can never be mistaken for a completed
	// reseed.
	Digest [digestSize]byte
}

// WriteRequest frames payload onto w:
//
//	magic[4] | version[1] | payloadLen uint16 BE | payload
func WriteRequest(w io.Writer, payload []byte) error {
	if len(payload) == 0 {
		return fmt.Errorf("entropy: refusing to send an empty payload")
	}
	if len(payload) > MaxPayloadBytes {
		return fmt.Errorf("entropy: payload %d exceeds max %d", len(payload), MaxPayloadBytes)
	}
	header := make([]byte, 0, len(reqMagic)+1+2)
	header = append(header, reqMagic[:]...)
	header = append(header, ProtocolVersion)
	header = binary.BigEndian.AppendUint16(header, uint16(len(payload)))
	if _, err := w.Write(header); err != nil {
		return fmt.Errorf("entropy: write request header: %w", err)
	}
	if _, err := w.Write(payload); err != nil {
		return fmt.Errorf("entropy: write request payload: %w", err)
	}
	return nil
}

// ReadRequest reads one framed request from r and returns the payload.
func ReadRequest(r io.Reader) ([]byte, error) {
	header := make([]byte, len(reqMagic)+1+2)
	if _, err := io.ReadFull(r, header); err != nil {
		return nil, fmt.Errorf("entropy: read request header: %w", err)
	}
	if [4]byte(header[:4]) != reqMagic {
		return nil, fmt.Errorf("entropy: bad request magic %q", header[:4])
	}
	if header[4] != ProtocolVersion {
		return nil, fmt.Errorf("entropy: unsupported protocol version %d", header[4])
	}
	n := binary.BigEndian.Uint16(header[5:7])
	if n == 0 {
		return nil, fmt.Errorf("entropy: zero-length payload")
	}
	if int(n) > MaxPayloadBytes {
		return nil, fmt.Errorf("entropy: payload length %d exceeds max %d", n, MaxPayloadBytes)
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, fmt.Errorf("entropy: read request payload: %w", err)
	}
	return payload, nil
}

// WriteAck frames an ack onto w:
//
//	magic[4] | version[1] | status[1] | sha256[32]
func WriteAck(w io.Writer, status byte, digest [digestSize]byte) error {
	frame := make([]byte, 0, len(ackMagic)+2+digestSize)
	frame = append(frame, ackMagic[:]...)
	frame = append(frame, ProtocolVersion, status)
	frame = append(frame, digest[:]...)
	if _, err := w.Write(frame); err != nil {
		return fmt.Errorf("entropy: write ack: %w", err)
	}
	return nil
}

// ReadAck reads one framed ack from r.
func ReadAck(r io.Reader) (Ack, error) {
	frame := make([]byte, len(ackMagic)+2+digestSize)
	if _, err := io.ReadFull(r, frame); err != nil {
		return Ack{}, fmt.Errorf("entropy: read ack: %w", err)
	}
	if [4]byte(frame[:4]) != ackMagic {
		return Ack{}, fmt.Errorf("entropy: bad ack magic %q", frame[:4])
	}
	if frame[4] != ProtocolVersion {
		return Ack{}, fmt.Errorf("entropy: unsupported ack version %d", frame[4])
	}
	var ack Ack
	ack.Status = frame[5]
	copy(ack.Digest[:], frame[6:])
	return ack, nil
}
