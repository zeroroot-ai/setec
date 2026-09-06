// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package entropy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- wire protocol -------------------------------------------------

func TestProtocol_RequestRoundtrip(t *testing.T) {
	payload := bytes.Repeat([]byte{0xAB}, 256)
	var buf bytes.Buffer
	if err := WriteRequest(&buf, payload); err != nil {
		t.Fatalf("WriteRequest: %v", err)
	}
	got, err := ReadRequest(&buf)
	if err != nil {
		t.Fatalf("ReadRequest: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("payload mismatch: got %d bytes", len(got))
	}
}

func TestProtocol_RejectsEmptyPayload(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteRequest(&buf, nil); err == nil {
		t.Fatal("WriteRequest must reject an empty payload")
	}
}

func TestProtocol_RejectsOversizedPayload(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteRequest(&buf, make([]byte, MaxPayloadBytes+1)); err == nil {
		t.Fatal("WriteRequest must reject an oversized payload")
	}
	// A hand-crafted oversized length on the wire must be rejected by
	// the reader too (defense against a malicious host/guest peer).
	crafted := append([]byte{'S', 'R', 'S', 'D', ProtocolVersion, 0xFF, 0xFF}, make([]byte, 65535)...)
	if _, err := ReadRequest(bytes.NewReader(crafted)); err == nil {
		t.Fatal("ReadRequest must reject an oversized length")
	}
}

func TestProtocol_RejectsBadMagicAndVersion(t *testing.T) {
	payload := []byte{1, 2, 3, 4}
	var buf bytes.Buffer
	if err := WriteRequest(&buf, payload); err != nil {
		t.Fatalf("WriteRequest: %v", err)
	}
	raw := buf.Bytes()

	badMagic := append([]byte(nil), raw...)
	badMagic[0] = 'X'
	if _, err := ReadRequest(bytes.NewReader(badMagic)); err == nil {
		t.Fatal("ReadRequest must reject a bad magic")
	}

	badVer := append([]byte(nil), raw...)
	badVer[4] = 99
	if _, err := ReadRequest(bytes.NewReader(badVer)); err == nil {
		t.Fatal("ReadRequest must reject an unknown version")
	}
}

func TestProtocol_AckRoundtrip(t *testing.T) {
	digest := sha256.Sum256([]byte("payload"))
	var buf bytes.Buffer
	if err := WriteAck(&buf, StatusOK, digest); err != nil {
		t.Fatalf("WriteAck: %v", err)
	}
	ack, err := ReadAck(&buf)
	if err != nil {
		t.Fatalf("ReadAck: %v", err)
	}
	if ack.Status != StatusOK || ack.Digest != digest {
		t.Fatalf("ack mismatch: %+v", ack)
	}
}

// --- guest handler --------------------------------------------------

// fakePool records injected entropy and can be told to fail.
type fakePool struct {
	mu       sync.Mutex
	payloads [][]byte
	err      error
}

func (f *fakePool) AddEntropy(p []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.payloads = append(f.payloads, append([]byte(nil), p...))
	return nil
}

func (f *fakePool) received() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]byte, len(f.payloads))
	copy(out, f.payloads)
	return out
}

func TestGuestHandler_InjectsAndAcks(t *testing.T) {
	pool := &fakePool{}
	h := &GuestHandler{Pool: pool}
	client, server := net.Pipe()
	defer func() { _ = client.Close() }()

	go func() { _ = h.ServeConn(server) }()

	payload := bytes.Repeat([]byte{0x42}, 64)
	if err := WriteRequest(client, payload); err != nil {
		t.Fatalf("WriteRequest: %v", err)
	}
	ack, err := ReadAck(client)
	if err != nil {
		t.Fatalf("ReadAck: %v", err)
	}
	if ack.Status != StatusOK {
		t.Fatalf("expected StatusOK, got %d", ack.Status)
	}
	want := sha256.Sum256(payload)
	if ack.Digest != want {
		t.Fatal("ack digest must be the SHA-256 of the received payload")
	}
	got := pool.received()
	if len(got) != 1 || !bytes.Equal(got[0], payload) {
		t.Fatalf("pool did not receive the payload: %v", got)
	}
}

func TestGuestHandler_PoolFailureAcksError(t *testing.T) {
	pool := &fakePool{err: errors.New("ioctl failed")}
	h := &GuestHandler{Pool: pool}
	client, server := net.Pipe()
	defer func() { _ = client.Close() }()

	go func() { _ = h.ServeConn(server) }()

	if err := WriteRequest(client, []byte{1, 2, 3}); err != nil {
		t.Fatalf("WriteRequest: %v", err)
	}
	ack, err := ReadAck(client)
	if err != nil {
		t.Fatalf("ReadAck: %v", err)
	}
	if ack.Status == StatusOK {
		t.Fatal("a pool failure must NOT be acked as StatusOK (false assurance)")
	}
}

func TestGuestHandler_ServeAcceptLoop(t *testing.T) {
	dir := t.TempDir()
	ln, err := net.Listen("unix", filepath.Join(dir, "guest.sock"))
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	pool := &fakePool{}
	h := &GuestHandler{Pool: pool}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = h.Serve(ctx, ln)
	}()

	for i := range 3 {
		conn, err := net.Dial("unix", ln.Addr().String())
		if err != nil {
			t.Fatalf("dial %d: %v", i, err)
		}
		payload := bytes.Repeat([]byte{byte(i + 1)}, 32)
		if err := WriteRequest(conn, payload); err != nil {
			t.Fatalf("WriteRequest %d: %v", i, err)
		}
		ack, err := ReadAck(conn)
		if err != nil {
			t.Fatalf("ReadAck %d: %v", i, err)
		}
		if ack.Status != StatusOK {
			t.Fatalf("conn %d: status %d", i, ack.Status)
		}
		_ = conn.Close()
	}
	if got := len(pool.received()); got != 3 {
		t.Fatalf("expected 3 injections, got %d", got)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not stop on ctx cancel")
	}
}

// --- host reseeder over a fake Firecracker vsock UDS ----------------

// fakeVsockMux emulates the Firecracker hybrid-vsock host-side Unix
// socket: it expects "CONNECT <port>\n", replies "OK <n>\n", then
// bridges the stream to the given handler (or misbehaves per mode).
type fakeVsockMux struct {
	ln       net.Listener
	wantPort uint32

	// mode selects the misbehavior. "" = bridge to handler.
	mode    string // "", "refuse", "silent", "wrong-digest"
	handler *GuestHandler
}

func startFakeVsockMux(t *testing.T, dir string, mode string, h *GuestHandler) string {
	t.Helper()
	path := filepath.Join(dir, "fc-vsock.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen %s: %v", path, err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	mux := &fakeVsockMux{ln: ln, wantPort: DefaultVsockPort, mode: mode, handler: h}
	go mux.run()
	return path
}

func (m *fakeVsockMux) run() {
	for {
		conn, err := m.ln.Accept()
		if err != nil {
			return
		}
		go m.serveConn(conn)
	}
}

func (m *fakeVsockMux) serveConn(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	br := bufio.NewReader(conn)
	line, err := br.ReadString('\n')
	if err != nil {
		return
	}
	var port uint32
	if _, err := fmt.Sscanf(strings.TrimSpace(line), "CONNECT %d", &port); err != nil {
		return
	}
	if port != m.wantPort {
		// Real Firecracker closes the connection when no guest
		// listener is bound on the port.
		return
	}
	switch m.mode {
	case "refuse":
		return // close without OK — guest not listening
	case "silent":
		_, _ = fmt.Fprintf(conn, "OK 1024\n")
		// Handshake fine, but the guest never answers.
		time.Sleep(5 * time.Second)
		return
	case "wrong-digest":
		_, _ = fmt.Fprintf(conn, "OK 1024\n")
		if _, err := ReadRequest(io.MultiReader(br, conn)); err != nil {
			return
		}
		_ = WriteAck(conn, StatusOK, sha256.Sum256([]byte("not the payload")))
		return
	default:
		_, _ = fmt.Fprintf(conn, "OK 1024\n")
		_ = m.handler.ServeConn(&bufferedConn{Conn: conn, r: br})
	}
}

// bufferedConn lets the mux hand remaining buffered bytes to the
// handler after the handshake line was consumed.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (b *bufferedConn) Read(p []byte) (int, error) { return b.r.Read(p) }

func TestVsockReseeder_HappyPath(t *testing.T) {
	pool := &fakePool{}
	path := startFakeVsockMux(t, t.TempDir(), "", &GuestHandler{Pool: pool})

	r := NewVsockReseeder()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := r.Reseed(ctx, path); err != nil {
		t.Fatalf("Reseed: %v", err)
	}
	got := pool.received()
	if len(got) != 1 {
		t.Fatalf("expected exactly one injection, got %d", len(got))
	}
	if len(got[0]) != DefaultPayloadBytes {
		t.Fatalf("expected %d payload bytes, got %d", DefaultPayloadBytes, len(got[0]))
	}
	if bytes.Equal(got[0], make([]byte, len(got[0]))) {
		t.Fatal("payload must not be all-zero")
	}
}

func TestVsockReseeder_FailsClosedWhenSocketMissing(t *testing.T) {
	r := NewVsockReseeder()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := r.Reseed(ctx, filepath.Join(t.TempDir(), "missing.sock")); err == nil {
		t.Fatal("Reseed must fail when the vsock UDS does not exist")
	}
}

func TestVsockReseeder_FailsClosedWhenGuestNotListening(t *testing.T) {
	path := startFakeVsockMux(t, t.TempDir(), "refuse", nil)
	r := NewVsockReseeder()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := r.Reseed(ctx, path); err == nil {
		t.Fatal("Reseed must fail when the guest agent is not listening")
	}
}

func TestVsockReseeder_FailsClosedOnAckTimeout(t *testing.T) {
	path := startFakeVsockMux(t, t.TempDir(), "silent", nil)
	r := NewVsockReseeder()
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if err := r.Reseed(ctx, path); err == nil {
		t.Fatal("Reseed must fail when the guest never acks")
	}
}

func TestVsockReseeder_FailsClosedOnDigestMismatch(t *testing.T) {
	path := startFakeVsockMux(t, t.TempDir(), "wrong-digest", nil)
	r := NewVsockReseeder()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := r.Reseed(ctx, path); err == nil {
		t.Fatal("Reseed must fail when the ack digest does not match the sent payload")
	}
}

func TestReseedFirst_TriesCandidatesInOrder(t *testing.T) {
	pool := &fakePool{}
	dir := t.TempDir()
	good := startFakeVsockMux(t, dir, "", &GuestHandler{Pool: pool})
	missing := filepath.Join(dir, "missing.sock")

	r := NewVsockReseeder()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := ReseedFirst(ctx, r, []string{missing, good}); err != nil {
		t.Fatalf("ReseedFirst: %v", err)
	}
	if len(pool.received()) != 1 {
		t.Fatal("the good candidate must have been reseeded")
	}

	if err := ReseedFirst(ctx, r, []string{missing}); err == nil {
		t.Fatal("ReseedFirst must fail when every candidate fails")
	}
	if err := ReseedFirst(ctx, r, nil); err == nil {
		t.Fatal("ReseedFirst must fail on an empty candidate list")
	}
}

// TestReseed_RestoredClonesDiverge is the unit-level divergence proof:
// two "guests" (fake entropy pools) that would otherwise share the
// exact CSPRNG state captured in a common snapshot receive provably
// different, fresh entropy from the host reseeder.
func TestReseed_RestoredClonesDiverge(t *testing.T) {
	poolA, poolB := &fakePool{}, &fakePool{}
	dirA, dirB := t.TempDir(), t.TempDir()
	pathA := startFakeVsockMux(t, dirA, "", &GuestHandler{Pool: poolA})
	pathB := startFakeVsockMux(t, dirB, "", &GuestHandler{Pool: poolB})

	r := NewVsockReseeder()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := r.Reseed(ctx, pathA); err != nil {
		t.Fatalf("Reseed A: %v", err)
	}
	if err := r.Reseed(ctx, pathB); err != nil {
		t.Fatalf("Reseed B: %v", err)
	}

	a, b := poolA.received(), poolB.received()
	if len(a) != 1 || len(b) != 1 {
		t.Fatalf("expected one injection each, got %d/%d", len(a), len(b))
	}
	if bytes.Equal(a[0], b[0]) {
		t.Fatal("two restored clones must NOT receive identical entropy")
	}
}
