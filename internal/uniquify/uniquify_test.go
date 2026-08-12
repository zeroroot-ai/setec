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

package uniquify

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- wire protocol -------------------------------------------------

func TestProtocol_SpecRoundtrip(t *testing.T) {
	spec := Spec{MachineID: strings.Repeat("a", 32), BootID: "boot", Hostname: "sb-1", PodIP: "10.1.2.3"}
	var buf bytes.Buffer
	sent, err := WriteSpec(&buf, spec)
	if err != nil {
		t.Fatalf("WriteSpec: %v", err)
	}
	got, raw, err := ReadSpec(&buf)
	if err != nil {
		t.Fatalf("ReadSpec: %v", err)
	}
	if got != spec {
		t.Fatalf("spec mismatch: %+v vs %+v", got, spec)
	}
	if !bytes.Equal(sent, raw) {
		t.Fatal("raw bytes must round-trip so both sides digest the same input")
	}
}

func TestProtocol_ReportRoundtrip(t *testing.T) {
	report := Report{
		Status: StatusOK, Digest: "abc", MachineID: "m", BootID: "b",
		Hostname: "h", ObservedIPs: []string{"10.0.0.1"}, GuestCID: 7,
	}
	var buf bytes.Buffer
	if err := WriteReport(&buf, report); err != nil {
		t.Fatalf("WriteReport: %v", err)
	}
	got, err := ReadReport(&buf)
	if err != nil {
		t.Fatalf("ReadReport: %v", err)
	}
	if got.GuestCID != 7 || got.MachineID != "m" || len(got.ObservedIPs) != 1 {
		t.Fatalf("report mismatch: %+v", got)
	}
}

func TestProtocol_RejectsBadMagicVersionAndLength(t *testing.T) {
	spec := Spec{MachineID: strings.Repeat("a", 32)}
	var buf bytes.Buffer
	if _, err := WriteSpec(&buf, spec); err != nil {
		t.Fatalf("WriteSpec: %v", err)
	}
	raw := buf.Bytes()

	badMagic := append([]byte(nil), raw...)
	badMagic[0] = 'X'
	if _, _, err := ReadSpec(bytes.NewReader(badMagic)); err == nil {
		t.Fatal("ReadSpec must reject a bad magic")
	}

	badVersion := append([]byte(nil), raw...)
	badVersion[4] = 99
	if _, _, err := ReadSpec(bytes.NewReader(badVersion)); err == nil {
		t.Fatal("ReadSpec must reject an unsupported version")
	}

	// Oversized declared length must be rejected before allocation.
	crafted := append([]byte{'S', 'U', 'N', 'Q', ProtocolVersion, 0xFF, 0xFF, 0xFF, 0xFF}, make([]byte, 16)...)
	if _, _, err := ReadSpec(bytes.NewReader(crafted)); err == nil {
		t.Fatal("ReadSpec must reject an oversized length")
	}
}

// --- spec generation ----------------------------------------------

func TestNewSpec_MintsFreshIdentityPerCall(t *testing.T) {
	a, err := NewSpec("My Sandbox", "10.0.0.5")
	if err != nil {
		t.Fatalf("NewSpec: %v", err)
	}
	b, err := NewSpec("My Sandbox", "10.0.0.5")
	if err != nil {
		t.Fatalf("NewSpec: %v", err)
	}
	if a.MachineID == b.MachineID {
		t.Fatal("two specs must never share a machine-id")
	}
	if a.BootID == b.BootID {
		t.Fatal("two specs must never share a boot-id")
	}
	if len(a.MachineID) != 32 {
		t.Fatalf("machine-id must be 32 hex chars, got %q", a.MachineID)
	}
	if a.Hostname != "my-sandbox" {
		t.Fatalf("hostname not sanitized: %q", a.Hostname)
	}
}

func TestSanitizeHostname(t *testing.T) {
	cases := map[string]string{
		"plain":                 "plain",
		"UPPER_case.name":       "upper-case-name",
		"---":                   "setec-sandbox",
		"":                      "setec-sandbox",
		strings.Repeat("x", 80): strings.Repeat("x", 63),
	}
	for in, want := range cases {
		if got := SanitizeHostname(in); got != want {
			t.Errorf("SanitizeHostname(%q) = %q, want %q", in, got, want)
		}
	}
}

// --- verification --------------------------------------------------

func makeVerified(t *testing.T) (Spec, []byte, Report) {
	t.Helper()
	spec, err := NewSpec("sb-1", "10.1.2.3")
	if err != nil {
		t.Fatalf("NewSpec: %v", err)
	}
	var buf bytes.Buffer
	raw, err := WriteSpec(&buf, spec)
	if err != nil {
		t.Fatalf("WriteSpec: %v", err)
	}
	report := Report{
		Status:      StatusOK,
		Digest:      DigestHex(raw),
		MachineID:   spec.MachineID,
		BootID:      spec.BootID,
		Hostname:    spec.Hostname,
		ObservedIPs: []string{"10.1.2.3"},
		GuestCID:    42,
	}
	return spec, raw, report
}

func TestVerify_AcceptsMatchingReport(t *testing.T) {
	spec, raw, report := makeVerified(t)
	if err := Verify(spec, raw, report); err != nil {
		t.Fatalf("Verify rejected a matching report: %v", err)
	}
}

func TestVerify_FailsClosedOnEveryMismatch(t *testing.T) {
	mutations := map[string]func(*Report){
		"status":     func(r *Report) { r.Status = StatusError },
		"digest":     func(r *Report) { r.Digest = "deadbeef" },
		"machine-id": func(r *Report) { r.MachineID = strings.Repeat("f", 32) },
		"boot-id":    func(r *Report) { r.BootID = "other" },
		"hostname":   func(r *Report) { r.Hostname = "other" },
		"pod-ip":     func(r *Report) { r.ObservedIPs = []string{"192.168.9.9"} },
		"cid-zero":   func(r *Report) { r.GuestCID = 0 },
	}
	for name, mutate := range mutations {
		spec, raw, report := makeVerified(t)
		mutate(&report)
		if err := Verify(spec, raw, report); err == nil {
			t.Errorf("Verify must fail closed on %s mismatch", name)
		}
	}
}

func TestVerify_EmptyPodIPSkipsAddressCheck(t *testing.T) {
	spec, _, report := makeVerified(t)
	spec.PodIP = ""
	report.ObservedIPs = nil
	// Digest covers the original raw bytes; rebuild them for the
	// modified spec so only the address check semantics are under
	// test.
	var buf bytes.Buffer
	raw, err := WriteSpec(&buf, spec)
	if err != nil {
		t.Fatalf("WriteSpec: %v", err)
	}
	report.Digest = DigestHex(raw)
	if err := Verify(spec, raw, report); err != nil {
		t.Fatalf("empty PodIP must skip the address check: %v", err)
	}
}

// --- guest handler -------------------------------------------------

type fakeIdentity struct {
	mu        sync.Mutex
	machineID string
	bootID    string
	hostname  string
	failOn    string
}

func (f *fakeIdentity) ApplyMachineID(id string) error {
	if f.failOn == "machine-id" {
		return errors.New("write refused")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.machineID = id
	return nil
}

func (f *fakeIdentity) ApplyBootID(id string) error {
	if f.failOn == "boot-id" {
		return errors.New("bind mount refused")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bootID = id
	return nil
}

func (f *fakeIdentity) ApplyHostname(name string) error {
	if f.failOn == "hostname" {
		return errors.New("sethostname refused")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hostname = name
	return nil
}

func (f *fakeIdentity) Read() (string, string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.machineID, f.bootID, f.hostname, nil
}

type fakeNetwork struct {
	observed []string
	err      error
	gotIP    string
}

func (f *fakeNetwork) Reconcile(expectedIP string) ([]string, error) {
	f.gotIP = expectedIP
	if f.err != nil {
		return nil, f.err
	}
	if len(f.observed) == 0 && expectedIP != "" {
		return []string{expectedIP}, nil
	}
	return f.observed, nil
}

type fakeCID struct {
	cid uint32
	err error
}

func (f fakeCID) LocalCID() (uint32, error) { return f.cid, f.err }

func newTestHandler(id *fakeIdentity, nw *fakeNetwork, cid fakeCID) *GuestHandler {
	return &GuestHandler{Identity: id, Network: nw, CID: cid}
}

func TestGuestHandler_AppliesAndReports(t *testing.T) {
	id := &fakeIdentity{}
	nw := &fakeNetwork{}
	h := newTestHandler(id, nw, fakeCID{cid: 5})
	client, server := net.Pipe()
	defer func() { _ = client.Close() }()

	go func() { _ = h.ServeConn(server) }()

	spec, err := NewSpec("sb-a", "10.9.8.7")
	if err != nil {
		t.Fatalf("NewSpec: %v", err)
	}
	raw, err := WriteSpec(client, spec)
	if err != nil {
		t.Fatalf("WriteSpec: %v", err)
	}
	report, err := ReadReport(client)
	if err != nil {
		t.Fatalf("ReadReport: %v", err)
	}
	if err := Verify(spec, raw, report); err != nil {
		t.Fatalf("report must verify: %v", err)
	}
	if nw.gotIP != "10.9.8.7" {
		t.Fatalf("network reconciler saw %q", nw.gotIP)
	}
}

func TestGuestHandler_ApplierFailureAcksError(t *testing.T) {
	for _, failOn := range []string{"machine-id", "boot-id", "hostname"} {
		id := &fakeIdentity{failOn: failOn}
		h := newTestHandler(id, &fakeNetwork{}, fakeCID{cid: 5})
		client, server := net.Pipe()

		go func() { _ = h.ServeConn(server) }()

		spec, _ := NewSpec("sb-a", "")
		if _, err := WriteSpec(client, spec); err != nil {
			t.Fatalf("WriteSpec: %v", err)
		}
		report, err := ReadReport(client)
		if err != nil {
			t.Fatalf("ReadReport: %v", err)
		}
		if report.Status == StatusOK {
			t.Errorf("a %s failure must NOT be acked StatusOK (false assurance)", failOn)
		}
		_ = client.Close()
	}
}

func TestGuestHandler_CIDFailureAcksError(t *testing.T) {
	h := newTestHandler(&fakeIdentity{}, &fakeNetwork{}, fakeCID{err: errors.New("no /dev/vsock")})
	client, server := net.Pipe()
	defer func() { _ = client.Close() }()

	go func() { _ = h.ServeConn(server) }()

	spec, _ := NewSpec("sb-a", "")
	if _, err := WriteSpec(client, spec); err != nil {
		t.Fatalf("WriteSpec: %v", err)
	}
	report, err := ReadReport(client)
	if err != nil {
		t.Fatalf("ReadReport: %v", err)
	}
	if report.Status == StatusOK {
		t.Fatal("an unreadable CID must NOT be acked StatusOK")
	}
}

// --- host uniquifier over a fake Firecracker vsock UDS -------------

// fakeVsockMux emulates the Firecracker hybrid-vsock host-side Unix
// socket: it expects "CONNECT <port>\n", replies "OK <n>\n", then
// bridges the stream to the handler (or misbehaves per mode).
type fakeVsockMux struct {
	ln       net.Listener
	wantPort uint32
	mode     string // "", "refuse", "silent"
	handler  *GuestHandler
}

func startFakeVsockMux(t *testing.T, mode string, h *GuestHandler) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fc-vsock.sock")
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

type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (b *bufferedConn) Read(p []byte) (int, error) { return b.r.Read(p) }

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
		return
	}
	switch m.mode {
	case "refuse":
		return
	case "silent":
		_, _ = fmt.Fprintf(conn, "OK 1024\n")
		time.Sleep(5 * time.Second)
	default:
		_, _ = fmt.Fprintf(conn, "OK 1024\n")
		_ = m.handler.ServeConn(&bufferedConn{Conn: conn, r: br})
	}
}

func TestVsockUniquifier_HappyPath(t *testing.T) {
	h := newTestHandler(&fakeIdentity{}, &fakeNetwork{}, fakeCID{cid: 9})
	path := startFakeVsockMux(t, "", h)

	u := NewVsockUniquifier()
	spec, err := NewSpec("sb-b", "10.4.4.4")
	if err != nil {
		t.Fatalf("NewSpec: %v", err)
	}
	report, err := u.Uniquify(context.Background(), path, spec)
	if err != nil {
		t.Fatalf("Uniquify: %v", err)
	}
	if report.GuestCID != 9 {
		t.Fatalf("expected reported CID 9, got %d", report.GuestCID)
	}
}

func TestVsockUniquifier_FailsWhenGuestNotListening(t *testing.T) {
	path := startFakeVsockMux(t, "refuse", nil)
	u := NewVsockUniquifier()
	u.DialTimeout = time.Second
	spec, _ := NewSpec("sb-b", "")
	if _, err := u.Uniquify(context.Background(), path, spec); err == nil {
		t.Fatal("a refused handshake must fail the uniquification")
	}
}

func TestVsockUniquifier_FailsOnSilentGuest(t *testing.T) {
	path := startFakeVsockMux(t, "silent", nil)
	u := NewVsockUniquifier()
	u.DialTimeout = 500 * time.Millisecond
	spec, _ := NewSpec("sb-b", "")
	if _, err := u.Uniquify(context.Background(), path, spec); err == nil {
		t.Fatal("a silent guest must fail the uniquification")
	}
}

func TestVsockUniquifier_FailsWhenGuestCannotApply(t *testing.T) {
	h := newTestHandler(&fakeIdentity{failOn: "boot-id"}, &fakeNetwork{}, fakeCID{cid: 9})
	path := startFakeVsockMux(t, "", h)
	u := NewVsockUniquifier()
	spec, _ := NewSpec("sb-b", "")
	if _, err := u.Uniquify(context.Background(), path, spec); err == nil {
		t.Fatal("a StatusError report must fail the uniquification")
	}
}

func TestUniquifyFirst_TriesCandidatesInOrder(t *testing.T) {
	h := newTestHandler(&fakeIdentity{}, &fakeNetwork{}, fakeCID{cid: 11})
	good := startFakeVsockMux(t, "", h)
	u := NewVsockUniquifier()
	u.DialTimeout = time.Second

	spec, _ := NewSpec("sb-c", "")
	report, err := UniquifyFirst(context.Background(), u,
		[]string{filepath.Join(t.TempDir(), "missing.sock"), good}, spec)
	if err != nil {
		t.Fatalf("UniquifyFirst: %v", err)
	}
	if report.GuestCID != 11 {
		t.Fatalf("unexpected CID %d", report.GuestCID)
	}
}

func TestUniquifyFirst_EmptyCandidatesFailClosed(t *testing.T) {
	u := NewVsockUniquifier()
	spec, _ := NewSpec("sb-c", "")
	if _, err := UniquifyFirst(context.Background(), u, nil, spec); err == nil {
		t.Fatal("no candidates must fail closed")
	}
}

// --- CID allocator -------------------------------------------------

func TestCIDAllocator_AllocatesDistinctMonotonicCIDs(t *testing.T) {
	a := NewCIDAllocator()
	seen := map[uint32]bool{}
	for i := range 10 {
		cid, err := a.Allocate(fmt.Sprintf("entry-%d", i))
		if err != nil {
			t.Fatalf("Allocate: %v", err)
		}
		if cid < FirstGuestCID {
			t.Fatalf("allocated reserved CID %d", cid)
		}
		if seen[cid] {
			t.Fatalf("CID %d allocated twice", cid)
		}
		seen[cid] = true
	}
}

func TestCIDAllocator_NeverReusesReleasedCIDs(t *testing.T) {
	a := NewCIDAllocator()
	cid, err := a.Allocate("entry-1")
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	a.Release(cid)
	next, err := a.Allocate("entry-2")
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if next == cid {
		t.Fatal("a released CID must never be re-issued (stale registrations must not mask collisions)")
	}
}

func TestCIDAllocator_ObserveDetectsCollisions(t *testing.T) {
	a := NewCIDAllocator()
	if err := a.Observe(100, "ns/sb-1"); err != nil {
		t.Fatalf("first Observe: %v", err)
	}
	// Idempotent for the same owner.
	if err := a.Observe(100, "ns/sb-1"); err != nil {
		t.Fatalf("re-Observe same owner: %v", err)
	}
	// A different owner with the same CID is THE collision this
	// registry exists to catch (two restores of one Snapshot).
	if err := a.Observe(100, "ns/sb-2"); err == nil {
		t.Fatal("Observe must fail closed when another owner holds the CID")
	}
	// Reserved range is always rejected.
	if err := a.Observe(2, "ns/sb-3"); err == nil {
		t.Fatal("Observe must reject reserved CIDs")
	}
}

func TestCIDAllocator_ReleaseFreesForObserve(t *testing.T) {
	a := NewCIDAllocator()
	if err := a.Observe(200, "ns/sb-1"); err != nil {
		t.Fatalf("Observe: %v", err)
	}
	a.Release(200)
	if err := a.Observe(200, "ns/sb-2"); err != nil {
		t.Fatalf("Observe after Release: %v", err)
	}
}
