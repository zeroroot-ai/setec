// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

// Package guestagent is the process supervisor inside a launcher machine
// (docs/design/runtime.md). It runs as PID 1. It starts the workload with
// the entry point, user, directory and environment of the image, reports
// the exit code to the launcher, runs exec sessions, copies files, and
// reaps orphan processes.
//
// The launcher reaches it on one vsock port. A connection from any other
// vsock address, such as a process inside the machine, is refused, and the
// agent listens on no IP address, so the network of the workload cannot
// reach it.
package guestagent

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/zeroroot-ai/setec/internal/errwrap"
)

// ControlPort is the vsock port of the guest agent in the machine.
const ControlPort = 5200

// HostCID is the vsock address of the host, the only allowed caller.
const HostCID = 2

// Op names a request.
type Op string

// The requests of the launcher.
const (
	OpPing         Op = "ping"
	OpConfigureNet Op = "configure-net"
	OpSetTime      Op = "set-time"
	// OpResumed tells the workload that it resumed from a snapshot of the
	// time UnixNano (setec#194).
	OpResumed Op = "resumed"
	OpStart   Op = "start"
	OpExec    Op = "exec"
	OpCopyIn  Op = "copy-in"
	OpCopyOut Op = "copy-out"
)

// Process is what to run: argv, user, directory and environment.
type Process struct {
	Argv []string `json:"argv"`
	Env  []string `json:"env,omitempty"`
	// User is "name", "uid" or "uid:gid", looked up in /etc/passwd of the
	// image. Empty means root.
	User string `json:"user,omitempty"`
	Dir  string `json:"dir,omitempty"`
}

// Request is the first line of each connection.
type Request struct {
	Op      Op       `json:"op"`
	Process *Process `json:"process,omitempty"`
	// ConfigureNet: the Pod identity that the machine takes.
	Address string `json:"address,omitempty"` // CIDR
	MAC     string `json:"mac,omitempty"`
	MTU     int    `json:"mtu,omitempty"`
	Gateway string `json:"gateway,omitempty"`
	DNS     []byte `json:"dns,omitempty"` // the resolv.conf of the Pod
	// Hostname is the name of the Pod, which the machine takes.
	Hostname string `json:"hostname,omitempty"`
	// SetTime: the time of the node, in Unix nanoseconds.
	UnixNano int64 `json:"unixNano,omitempty"`
	// CopyIn and CopyOut: the path in the machine. CopyIn sends Size bytes
	// after the request line, with Mode.
	Path string `json:"path,omitempty"`
	Size int64  `json:"size,omitempty"`
	Mode uint32 `json:"mode,omitempty"`
}

// Response is the answer line to a request. For CopyOut, Size bytes follow
// it. For Exec, frames follow it.
type Response struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
	Size  int64  `json:"size,omitempty"`
}

// MaxLine bounds a request or response line.
const MaxLine = 1 << 20

// ReadLine reads one JSON line into v.
func ReadLine(r *bufio.Reader, v any) error {
	line, err := r.ReadSlice('\n')
	if errors.Is(err, bufio.ErrBufferFull) {
		return errors.New("guestagent: line too long")
	}
	if err != nil {
		return errwrap.Wrap(err, "bufio.Reader.ReadSlice")
	}
	return errwrap.Wrap(json.Unmarshal(line, v), "json.Unmarshal")
}

// WriteLine writes v as one JSON line.
func WriteLine(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return errwrap.Wrap(err, "json.Marshal")
	}
	_, err = w.Write(append(b, '\n'))
	return errwrap.Wrap(err, "io.Writer.Write")
}

// FrameType tags a frame of an exec stream.
type FrameType byte

// The frames of an exec stream. The launcher sends Stdin and StdinEOF. The
// agent sends Stdout, Stderr and, last, Exit with a 4-byte exit code.
const (
	FrameStdin FrameType = iota
	FrameStdout
	FrameStderr
	FrameExit
	FrameStdinEOF
)

// MaxFrame bounds the data of one frame.
const MaxFrame = 1 << 16

// WriteFrame writes one frame: type, 4-byte big-endian length, data.
func WriteFrame(w io.Writer, t FrameType, data []byte) error {
	if len(data) > MaxFrame {
		return fmt.Errorf("guestagent: frame of %d bytes exceeds %d", len(data), MaxFrame)
	}
	hdr := [5]byte{byte(t)}
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(data))) //nolint:gosec // G115: the check above bounds the data to MaxFrame
	if _, err := w.Write(hdr[:]); err != nil {
		return errwrap.Wrap(err, "io.Writer.Write")
	}
	_, err := w.Write(data)
	return errwrap.Wrap(err, "io.Writer.Write")
}

// ReadFrame reads one frame.
func ReadFrame(r io.Reader) (FrameType, []byte, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, errwrap.Wrap(err, "io.ReadFull")
	}
	n := binary.BigEndian.Uint32(hdr[1:])
	if n > MaxFrame {
		return 0, nil, fmt.Errorf("guestagent: frame of %d bytes exceeds %d", n, MaxFrame)
	}
	data := make([]byte, n)
	if _, err := io.ReadFull(r, data); err != nil {
		return 0, nil, errwrap.Wrap(err, "io.ReadFull")
	}
	return FrameType(hdr[0]), data, nil
}

// ExitFrame encodes an exit code for FrameExit.
func ExitFrame(code int) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, uint32(int32(code))) //nolint:gosec // an exit code fits
	return b
}

// ExitCode decodes the data of a FrameExit.
func ExitCode(data []byte) int {
	if len(data) != 4 {
		return 255
	}
	return int(int32(binary.BigEndian.Uint32(data))) //nolint:gosec // round trip of ExitFrame
}
