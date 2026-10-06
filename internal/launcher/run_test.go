// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package launcher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/zeroroot-ai/setec/internal/guestagent"
)

type fakeNet struct {
	joinErr                 error
	joined, connected, left int
}

func (f *fakeNet) Join() (PodNet, error) {
	f.joined++
	if f.joinErr != nil {
		return PodNet{}, f.joinErr
	}
	return PodNet{
		Address: netip.MustParsePrefix("10.42.0.7/32"), MAC: "02:00:00:00:00:07",
		MTU: 1500, Gateway: netip.MustParseAddr("10.42.0.1"),
	}, nil
}
func (f *fakeNet) Leave() error   { f.left++; return nil }
func (f *fakeNet) Connect() error { f.connected++; return nil }

// fakeVMM acts as Firecracker and a guest. After start it reports exit code
// report on the exit port, unless report is negative; then it ends alone.
type fakeVMM struct {
	report    int
	startErr  error
	config    string
	snapshot  []string
	mu        sync.Mutex
	stopped   bool
	done      chan struct{}
	closeOnce sync.Once
}

func (f *fakeVMM) Start(_ context.Context, workDir, configFile string, console io.Writer) error {
	if f.startErr != nil {
		return f.startErr
	}
	if configFile != "" {
		raw, _ := os.ReadFile(configFile)
		f.config = string(raw)
	}
	_, _ = fmt.Fprintln(console, "guest console line")
	f.done = make(chan struct{})
	go func() {
		if f.report < 0 {
			time.Sleep(20 * time.Millisecond)
			f.end()
			return
		}
		sock := filepath.Join(workDir, VsockSocket+"_"+strconv.Itoa(ExitPort))
		for range 100 {
			c, err := net.Dial("unix", sock)
			if err == nil {
				_ = json.NewEncoder(c).Encode(ExitReport{ExitCode: f.report})
				_ = c.Close()
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	return nil
}
func (f *fakeVMM) LoadSnapshot(_ context.Context, _, state, memory string) error {
	f.snapshot = []string{state, memory}
	return nil
}
func (f *fakeVMM) end()        { f.closeOnce.Do(func() { close(f.done) }) }
func (f *fakeVMM) Wait() error { <-f.done; return nil }
func (f *fakeVMM) Stop(time.Duration) error {
	f.mu.Lock()
	f.stopped = true
	f.mu.Unlock()
	f.end()
	return nil
}

func testSpec(t *testing.T) *Spec {
	t.Helper()
	dir := t.TempDir()
	img := filepath.Join(dir, "image.sqfs")
	if err := os.WriteFile(img, []byte("disk"), 0o600); err != nil {
		t.Fatal(err)
	}
	return &Spec{
		VCPU: 2, MemoryMiB: 512,
		ImageDisk: img, WritableDisk: filepath.Join(dir, "rw.ext4"), WritableBytes: 1 << 20,
		WorkDir:  filepath.Join(dir, "work"),
		Source:   Source{Boot: &BootSource{Kernel: "/opt/setec/vmlinux"}},
		Workload: &guestagent.Process{Argv: []string{"/bin/true"}},
	}
}

func TestRun_ReturnsTheExitCodeOfTheWorkload(t *testing.T) {
	t.Parallel()
	for _, want := range []int{0, 3, 255} {
		t.Run(strconv.Itoa(want), func(t *testing.T) {
			t.Parallel()
			nw, vmm, console := &fakeNet{}, &fakeVMM{report: want}, &syncBuf{}
			l := &Launcher{Spec: testSpec(t), Net: nw, VMM: vmm, Console: console, Grace: time.Second, Format: noFormat}
			code, err := l.Run(t.Context())
			if err != nil || code != want {
				t.Fatalf("Run = %d, %v; want %d", code, err, want)
			}
			if !vmm.stopped || nw.left != 1 {
				t.Fatalf("stopped=%t left=%d; want the machine stopped and the network removed", vmm.stopped, nw.left)
			}
			if console.String() != "guest console line\n" {
				t.Fatalf("console = %q", console.String())
			}
		})
	}
}

func TestRun_BootConfigTakesTheSandboxLimitsAndThePodMAC(t *testing.T) {
	t.Parallel()
	vmm := &fakeVMM{}
	l := &Launcher{Spec: testSpec(t), Net: &fakeNet{}, VMM: vmm, Console: io.Discard, Grace: time.Second, Format: noFormat}
	if _, err := l.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	var cfg vmConfig
	if err := json.Unmarshal([]byte(vmm.config), &cfg); err != nil {
		t.Fatalf("config: %v", err)
	}
	if cfg.MachineConfig.VCPUCount != 2 || cfg.MachineConfig.MemSizeMiB != 512 || !cfg.MachineConfig.TrackDirtyPages {
		t.Fatalf("machine = %+v", cfg.MachineConfig)
	}
	if cfg.NetworkInterfaces[0].HostDevName != TapDevice || cfg.NetworkInterfaces[0].GuestMAC != "02:00:00:00:00:07" {
		t.Fatalf("network = %+v", cfg.NetworkInterfaces)
	}
	if len(cfg.Drives) != 2 || !cfg.Drives[0].IsReadOnly || cfg.Drives[1].IsReadOnly {
		t.Fatalf("drives = %+v; want a read-only image and a writable layer", cfg.Drives)
	}
	if cfg.CPUConfig != "" {
		t.Fatalf("cpu-config = %q with no template", cfg.CPUConfig)
	}
	if cfg.BootSource.BootArgs != defaultBootArgs {
		t.Fatalf("boot args = %q", cfg.BootSource.BootArgs)
	}
	fi, err := os.Stat(l.Spec.WritableDisk)
	if err != nil || fi.Size() != 1<<20 {
		t.Fatalf("the writable layer: %v %v", fi, err)
	}
}

func TestRun_SnapshotIsTheSameCall(t *testing.T) {
	t.Parallel()
	s := testSpec(t)
	s.Source = Source{Snapshot: &SnapshotSource{State: "/snap/state", Memory: "/snap/mem"}}
	vmm := &fakeVMM{report: 0}
	var after []bool
	l := &Launcher{Spec: s, Net: &fakeNet{}, VMM: vmm, Console: io.Discard, Grace: time.Second, Format: noFormat,
		AfterStart: func(_ context.Context, _ PodNet, fromSnapshot bool) error {
			after = append(after, fromSnapshot)
			return nil
		}}
	if _, err := l.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	if vmm.config != "" || len(vmm.snapshot) != 2 || vmm.snapshot[0] != "/snap/state" {
		t.Fatalf("config=%q snapshot=%v; want a snapshot load and no boot config", vmm.config, vmm.snapshot)
	}
	if len(after) != 1 || !after[0] {
		t.Fatalf("AfterStart = %v; want one call for a snapshot", after)
	}
}

func TestRun_FailuresHaveATypedReasonAndLeaveNothing(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		mutate func(*Launcher)
		reason Reason
	}{
		{"bad spec", func(l *Launcher) { l.Spec.VCPU = 0 }, ReasonBadSpec},
		{"network", func(l *Launcher) { l.Net = &fakeNet{joinErr: errors.New("no eth0")} }, ReasonNetwork},
		{"no image disk", func(l *Launcher) { l.Spec.ImageDisk = "/no/such/disk" }, ReasonDisks},
		{"firecracker does not start", func(l *Launcher) { l.VMM = &fakeVMM{startErr: errors.New("no kvm")} }, ReasonVMMStart},
		{"firecracker ends with no report", func(l *Launcher) { l.VMM = &fakeVMM{report: -1} }, ReasonVMMExited},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			nw := &fakeNet{}
			l := &Launcher{Spec: testSpec(t), Net: nw, VMM: &fakeVMM{}, Console: io.Discard, Grace: time.Second, Format: noFormat}
			tc.mutate(l)
			if fn, ok := l.Net.(*fakeNet); ok {
				nw = fn
			}
			code, err := l.Run(t.Context())
			var le *Error
			if code != LaunchFailedExit || !errors.As(err, &le) || le.Reason != tc.reason {
				t.Fatalf("Run = %d, %v; want %d and reason %s", code, err, LaunchFailedExit, tc.reason)
			}
			if tc.reason != ReasonBadSpec && nw.left != 1 {
				t.Fatalf("network leave calls = %d, want 1", nw.left)
			}
		})
	}
}

type syncBuf struct {
	mu sync.Mutex
	b  []byte
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.b = append(s.b, p...)
	return len(p), nil
}
func (s *syncBuf) String() string { s.mu.Lock(); defer s.mu.Unlock(); return string(s.b) }

func noFormat(string) error { return nil }

// TestRun_ChecksTheDiskBeforeTheNetworkJoin proves that a disk that fails
// its check stops the launch before the network join, with a typed reason.
func TestRun_ChecksTheDiskBeforeTheNetworkJoin(t *testing.T) {
	t.Parallel()
	s := testSpec(t)
	nw := &fakeNet{}
	var joinedAtCheck int
	checked := 0
	l := &Launcher{Spec: s, Net: nw, VMM: &fakeVMM{}, Console: io.Discard, Grace: time.Second, Format: noFormat,
		CheckDisk: func(*Spec) error {
			joinedAtCheck = nw.joined
			checked++
			return nil
		}}
	if _, err := l.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	if checked != 1 || joinedAtCheck != 0 {
		t.Fatalf("checked = %d, joined at the check = %d; want one check before the join", checked, joinedAtCheck)
	}

	bad := errors.New("the disk has no valid signature")
	l = &Launcher{Spec: testSpec(t), Net: &fakeNet{}, VMM: &fakeVMM{}, Console: io.Discard, Grace: time.Second,
		Format: noFormat, CheckDisk: func(*Spec) error { return bad }}
	code, err := l.Run(t.Context())
	var le *Error
	if code != LaunchFailedExit || !errors.As(err, &le) || le.Reason != ReasonDisks || !errors.Is(err, bad) {
		t.Fatalf("Run = %d, %v; want exit %d with reason %s", code, err, LaunchFailedExit, ReasonDisks)
	}
}

// TestReadSpec_FromTheEnvironment reads the spec the way the operator passes
// it, and checks that the env wins over the file.
func TestReadSpec_FromTheEnvironment(t *testing.T) {
	s := testSpec(t)
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(SpecEnv, string(raw))
	got, err := ReadSpec("/no/such/file")
	if err != nil || got.VCPU != 2 || got.ImageDisk != s.ImageDisk {
		t.Fatalf("ReadSpec = %+v, %v", got, err)
	}
}

// TestRun_RestoreWaitsForTheStagedFilesAndConnectsLast proves the restore
// order: no load before the node agent staged the files, no network before
// the guest is confirmed, and evidence for the node agent at the end.
func TestRun_RestoreWaitsForTheStagedFilesAndConnectsLast(t *testing.T) {
	t.Parallel()
	s := testSpec(t)
	dir := t.TempDir()
	s.Source = Source{Snapshot: &SnapshotSource{
		State: filepath.Join(dir, "state.bin"), Memory: filepath.Join(dir, "memory.bin"),
		Staged: filepath.Join(dir, "staged"), Evidence: filepath.Join(dir, "evidence.json"),
	}}
	nw := &fakeNet{}
	vmm := &fakeVMM{report: 0}
	connectedAtAfterStart := -1
	l := &Launcher{Spec: s, Net: nw, VMM: vmm, Console: io.Discard, Grace: time.Second, Format: noFormat,
		AfterStart: func(context.Context, PodNet, bool) error {
			connectedAtAfterStart = nw.connected
			return nil
		}}
	go func() {
		time.Sleep(150 * time.Millisecond)
		_ = os.WriteFile(s.Source.Snapshot.Staged, nil, 0o600)
	}()
	start := time.Now()
	if _, err := l.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) < 150*time.Millisecond {
		t.Fatal("the launcher did not wait for the staged marker")
	}
	if connectedAtAfterStart != 0 || nw.connected != 1 {
		t.Fatalf("connected at AfterStart = %d, total = %d; want 0 then 1", connectedAtAfterStart, nw.connected)
	}
	var ev RestoreEvidence
	raw, err := os.ReadFile(s.Source.Snapshot.Evidence)
	if err != nil || json.Unmarshal(raw, &ev) != nil || !ev.EntropyReseeded || !ev.Uniquified || !ev.ClockSet {
		t.Fatalf("evidence = %s, %v", raw, err)
	}
}

// TestRun_RestoreFailureWritesTheErrorAndNeverConnects proves that a guest
// that is not confirmed never reaches the Pod network.
func TestRun_RestoreFailureWritesTheErrorAndNeverConnects(t *testing.T) {
	t.Parallel()
	s := testSpec(t)
	dir := t.TempDir()
	s.Source = Source{Snapshot: &SnapshotSource{
		State: filepath.Join(dir, "state.bin"), Memory: filepath.Join(dir, "memory.bin"),
		Evidence: filepath.Join(dir, "evidence.json"),
	}}
	nw := &fakeNet{}
	l := &Launcher{Spec: s, Net: nw, VMM: &fakeVMM{report: 0}, Console: io.Discard, Grace: time.Second, Format: noFormat,
		AfterStart: func(context.Context, PodNet, bool) error { return errors.New("no fresh entropy") }}
	code, err := l.Run(t.Context())
	if err == nil || code != LaunchFailedExit || nw.connected != 0 {
		t.Fatalf("Run = %d, %v, connected %d", code, err, nw.connected)
	}
	var ev RestoreEvidence
	raw, _ := os.ReadFile(s.Source.Snapshot.Evidence)
	if json.Unmarshal(raw, &ev) != nil || ev.Error == "" || ev.EntropyReseeded {
		t.Fatalf("evidence = %s", raw)
	}
}

func TestRun_BootConfigCarriesTheCPUTemplate(t *testing.T) {
	t.Parallel()
	s := testSpec(t)
	s.CPUTemplate = "/opt/setec/cpu-templates/fleet-v1.json"
	vmm := &fakeVMM{}
	l := &Launcher{Spec: s, Net: &fakeNet{}, VMM: vmm, Console: io.Discard, Grace: time.Second, Format: noFormat}
	if _, err := l.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	var cfg vmConfig
	if err := json.Unmarshal([]byte(vmm.config), &cfg); err != nil || cfg.CPUConfig != s.CPUTemplate {
		t.Fatalf("cpu-config = %q, %v", cfg.CPUConfig, err)
	}
}

func TestSpec_BaseBootsWithNoWorkload(t *testing.T) {
	t.Parallel()
	s := testSpec(t)
	s.Workload = nil
	if err := s.Validate(); err == nil {
		t.Fatal("a boot with no workload and no base was accepted")
	}
	s.Base = true
	if err := s.Validate(); err != nil {
		t.Fatalf("a base boot: %v", err)
	}
	s.Workload = &guestagent.Process{Argv: []string{"true"}}
	if err := s.Validate(); err == nil {
		t.Fatal("a base with a workload was accepted")
	}
}

// TestPrepareDisks_FormatsANewWorkspaceOnce proves that the first launch of
// a session formats its empty workspace and a later launch keeps it.
func TestPrepareDisks_FormatsANewWorkspaceOnce(t *testing.T) {
	t.Parallel()
	s := testSpec(t)
	s.WorkspaceDevice = filepath.Join(t.TempDir(), "ws")
	if err := os.WriteFile(s.WorkspaceDevice, make([]byte, 1<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(s.WorkDir, 0o700); err != nil {
		t.Fatal(err)
	}
	formats := 0
	fake := func(path string) error {
		formats++
		if path != s.WorkspaceDevice && path != s.WritableDisk {
			t.Fatalf("formatted %s", path)
		}
		f, err := os.OpenFile(path, os.O_WRONLY, 0)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		_, err = f.WriteAt([]byte{0x53, 0xEF}, ext4MagicOffset)
		return err
	}
	if err := s.prepareDisks(fake); err != nil {
		t.Fatal(err)
	}
	if err := s.prepareDisks(fake); err != nil {
		t.Fatal(err)
	}
	// One format of the writable layer and one of the workspace.
	if formats != 2 {
		t.Fatalf("formats = %d, want 2: the workspace is formatted once", formats)
	}
	if _, err := os.Readlink(filepath.Join(s.WorkDir, workspaceLink)); err != nil {
		t.Fatalf("no workspace link: %v", err)
	}
}
