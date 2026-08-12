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

// Command setec-pool-vm is the minimal Firecracker-wrapper binary the
// node-agent invokes to populate the pre-warm pool. It:
//
//  1. Writes a Firecracker bootSource/drive/machine-config to the API
//     socket after spawning `firecracker --api-sock <socket>`.
//  2. Issues InstanceActionInfo to boot the VM.
//  3. Pauses the VM via the firecracker.Client interface.
//  4. Writes a Full snapshot (state + memory) under
//     <storage-root>/<pool-entry-id>/.
//  5. Exits 0, leaving the Firecracker process running in its paused
//     state. The node-agent pool Manager tracks the socket path and
//     reuses the running VM for restore-on-claim.
//
// On any failure the launcher SIGTERMs the Firecracker process,
// waits up to five seconds, SIGKILLs if necessary, and removes
// partial state files before exiting non-zero.
//
// The binary intentionally speaks no Kubernetes and no gRPC; it is
// the smallest piece of glue between the pool Manager and the
// Firecracker API. Keeping it separate makes it trivially testable
// and distroless-shippable.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/zeroroot-ai/setec/internal/firecracker"
	"github.com/zeroroot-ai/setec/internal/nodeagent/poolentry"
	"github.com/zeroroot-ai/setec/internal/snapshot/atrest"
	"github.com/zeroroot-ai/setec/internal/snapshot/secretscan"
)

const (
	// defaultFirecrackerBinary is the standard path kata-deploy lays down.
	defaultFirecrackerBinary = "/usr/local/bin/firecracker"

	// defaultKeyFile is the node-local KEK the per-entry DEK is sealed
	// with (ADR-0005 invariant 5). Matches the node-agent's
	// --snapshot-key-file default.
	defaultKeyFile = "/var/lib/setec/keys/node.key"

	// socketReadyPollInterval is how often we poll the Firecracker API
	// socket during startup. 100ms keeps the tight worst-case budget
	// while not hammering the filesystem.
	socketReadyPollInterval = 100 * time.Millisecond

	// stateFileName and memFileName are the local filenames inside
	// <storage-root>/<pool-entry-id>/ where Firecracker writes the
	// serialised VM state and guest memory respectively. They are
	// encrypted in place before the launcher commits the entry.
	stateFileName = poolentry.StateFile
	memFileName   = poolentry.MemFile

	// vsockFileName is the local filename inside
	// <storage-root>/<pool-entry-id>/ where Firecracker binds the
	// host side of the guest vsock device (the active entropy-reseed
	// transport, setec#72).
	vsockFileName = "vsock.sock"

	// defaultGuestCID is the fallback vsock context id when the
	// node-agent does not pass an explicit --guest-cid. 0-2 are
	// reserved (hypervisor/loopback/host); 3 is the conventional
	// first guest CID. Production pool boots ALWAYS pass an explicit
	// node-unique CID from the node-agent's allocator (ADR-0005
	// invariant 2) so two entries never share one.
	defaultGuestCID = 3
)

// Options carries every knob the launcher takes. Kept as an explicit
// type so tests can instantiate it without going through flag parsing.
type Options struct {
	ImageRef           string
	KernelPath         string
	RootfsPath         string
	VCPUs              int
	MemoryMiB          int
	SocketPath         string
	StorageRoot        string
	PoolEntryID        string
	FirecrackerBinary  string
	BootReadyTimeout   time.Duration
	ShutdownGracePause time.Duration
	BootArgs           string
	VsockUDSPath       string
	KeyFile            string
	GuestCID           uint
}

func parseFlags(args []string) (Options, error) {
	fs := flag.NewFlagSet("setec-pool-vm", flag.ContinueOnError)
	var o Options
	fs.StringVar(&o.ImageRef, "image-ref", "",
		"OCI image reference the VM runs (recorded for the pool entry; the rootfs must already be extracted on disk)")
	fs.StringVar(&o.KernelPath, "kernel-path", "", "path to the uncompressed Linux kernel image")
	fs.StringVar(&o.RootfsPath, "rootfs-path", "", "path to the rootfs image or block device")
	fs.IntVar(&o.VCPUs, "vcpus", 1, "vCPUs allocated to the microVM")
	fs.IntVar(&o.MemoryMiB, "memory-mib", 512, "memory allocated to the microVM, in MiB")
	fs.StringVar(&o.SocketPath, "socket-path", "",
		"absolute path where Firecracker should expose its API socket (pre-existing files are removed)")
	fs.StringVar(&o.StorageRoot, "storage-root", "", "root directory under which pool entry state is written")
	fs.StringVar(&o.PoolEntryID, "pool-entry-id", "", "stable identifier for this entry (becomes <storage-root>/<id>/)")
	fs.StringVar(&o.FirecrackerBinary, "firecracker-binary", defaultFirecrackerBinary,
		"path to the firecracker binary to exec")
	fs.DurationVar(&o.BootReadyTimeout, "boot-ready-timeout", 30*time.Second,
		"maximum time to wait for the Firecracker API socket to accept requests")
	fs.DurationVar(&o.ShutdownGracePause, "shutdown-grace", 5*time.Second,
		"how long to wait after SIGTERM before SIGKILL on cleanup")
	fs.StringVar(&o.BootArgs, "boot-args", "console=ttyS0 reboot=k panic=1 pci=off",
		"kernel cmdline passed via boot-source")
	fs.StringVar(&o.VsockUDSPath, "vsock-uds-path", "",
		"host path where Firecracker binds the guest vsock device's Unix socket "+
			"(the entropy-reseed transport). Empty derives <storage-root>/<pool-entry-id>/vsock.sock")
	fs.StringVar(&o.KeyFile, "key-file", defaultKeyFile,
		"node-local key-encryption-key file the per-entry snapshot DEK is sealed with "+
			"(created on first use). Snapshot state is ALWAYS encrypted at rest; there is no opt-out.")
	fs.UintVar(&o.GuestCID, "guest-cid", defaultGuestCID,
		"vsock context id assigned to the guest; the node-agent allocates a node-unique "+
			"value per pool entry (ADR-0005 invariant 2). Must be >= 3 (0-2 are reserved)")

	if err := fs.Parse(args); err != nil {
		return o, err
	}

	var missing []string
	if o.KernelPath == "" {
		missing = append(missing, "--kernel-path")
	}
	if o.RootfsPath == "" {
		missing = append(missing, "--rootfs-path")
	}
	if o.SocketPath == "" {
		missing = append(missing, "--socket-path")
	}
	if o.StorageRoot == "" {
		missing = append(missing, "--storage-root")
	}
	if o.PoolEntryID == "" {
		missing = append(missing, "--pool-entry-id")
	}
	if o.KeyFile == "" {
		// Encryption at rest is not optional (ADR-0005 invariant 5);
		// an explicitly-emptied flag is a misconfiguration.
		missing = append(missing, "--key-file")
	}
	if len(missing) > 0 {
		return o, fmt.Errorf("missing required flag(s): %v", missing)
	}
	if o.VCPUs <= 0 || o.MemoryMiB <= 0 {
		return o, errors.New("--vcpus and --memory-mib must be positive")
	}
	if o.GuestCID < 3 || o.GuestCID > 0xFFFFFFFF {
		return o, fmt.Errorf("--guest-cid must be in 3..2^32-1, got %d", o.GuestCID)
	}
	return o, nil
}

// Spawner is the narrow interface runLauncher uses to exec Firecracker.
// A real implementation wraps exec.Cmd; tests inject a fake.
type Spawner interface {
	// Start launches the process detached. The returned handle is
	// responsible for its lifecycle (Wait / Signal / Kill).
	Start(ctx context.Context, binary string, args []string) (SpawnedProcess, error)
}

// SpawnedProcess models the subset of os.Process / exec.Cmd the
// launcher manipulates.
type SpawnedProcess interface {
	// Signal forwards a POSIX signal to the process.
	Signal(os.Signal) error
	// Wait blocks until the process exits and returns its exit error.
	Wait() error
	// Pid returns the OS process id (used only for log messages).
	Pid() int
}

// ClientFactory returns a firecracker.Client bound to the given socket.
type ClientFactory func(socketPath string) firecracker.Client

// execSpawner is the production Spawner.
type execSpawner struct{}

func (execSpawner) Start(ctx context.Context, binary string, args []string) (SpawnedProcess, error) {
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	// Run in its own process group so we can SIGTERM the whole tree
	// without catching the launcher's own signal handling.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &execProcess{cmd: cmd}, nil
}

type execProcess struct {
	cmd *exec.Cmd
}

func (p *execProcess) Signal(sig os.Signal) error {
	if p.cmd.Process == nil {
		return errors.New("firecracker process not started")
	}
	return p.cmd.Process.Signal(sig)
}

func (p *execProcess) Wait() error { return p.cmd.Wait() }
func (p *execProcess) Pid() int {
	if p.cmd.Process == nil {
		return -1
	}
	return p.cmd.Process.Pid
}

// runLauncher is the testable core: given fully-materialized
// dependencies, drive the launcher through its happy path or cleanup
// path. On success it leaves the Firecracker process alive, paused,
// and the snapshot files written.
func runLauncher(
	ctx context.Context,
	o Options,
	spawner Spawner,
	factory ClientFactory,
) (err error) {
	// Callers constructing Options directly (tests) may leave GuestCID
	// zero; fall back to the conventional first guest CID.
	if o.GuestCID == 0 {
		o.GuestCID = defaultGuestCID
	}

	// Ensure the storage directory exists before we spend time booting
	// a VM whose state we cannot persist.
	entryDir := filepath.Join(o.StorageRoot, o.PoolEntryID)
	if mkErr := os.MkdirAll(entryDir, 0o750); mkErr != nil {
		return fmt.Errorf("mkdir %q: %w", entryDir, mkErr)
	}

	// Template provenance (ADR-0005 invariant 4): this launcher only
	// ever snapshots the VM it cold-boots itself from the class
	// kernel/rootfs/image. A LIVE process already answering on the
	// requested socket would mean snapshotting a pre-existing —
	// possibly used — VM, so refuse outright rather than adopting it.
	if socketAlive(o.SocketPath) {
		return fmt.Errorf("socket %q has a live listener; refusing to snapshot a pre-existing VM (template provenance, ADR-0005 invariant 4)", o.SocketPath)
	}
	// Clean any stale (dead) socket file so Firecracker can bind.
	if rmErr := os.Remove(o.SocketPath); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
		return fmt.Errorf("remove stale socket %q: %w", o.SocketPath, rmErr)
	}

	fcArgs := []string{"--api-sock", o.SocketPath, "--id", o.PoolEntryID}
	proc, err := spawner.Start(ctx, o.FirecrackerBinary, fcArgs)
	if err != nil {
		_ = os.RemoveAll(entryDir)
		return fmt.Errorf("spawn firecracker: %w", err)
	}
	// From here on, any early return must clean up the firecracker
	// process plus the partial entry directory. The defer below does
	// that unless commit is set.
	var commit bool
	defer func() {
		if commit {
			return
		}
		killProcess(proc, o.ShutdownGracePause)
		_ = os.Remove(o.SocketPath)
		if rmErr := os.RemoveAll(entryDir); rmErr != nil {
			log.Printf("setec-pool-vm: cleanup entry dir %q: %v", entryDir, rmErr)
		}
	}()

	log.Printf("setec-pool-vm: firecracker pid=%d socket=%s", proc.Pid(), o.SocketPath)

	if err := waitForSocket(ctx, o.SocketPath, o.BootReadyTimeout); err != nil {
		return fmt.Errorf("wait for firecracker socket: %w", err)
	}

	fc := factory(o.SocketPath)

	if err := configureAndBoot(ctx, fc, o); err != nil {
		return fmt.Errorf("configure firecracker: %w", err)
	}

	if err := fc.Pause(ctx); err != nil {
		return fmt.Errorf("pause firecracker: %w", err)
	}

	statePath := filepath.Join(entryDir, stateFileName)
	memPath := filepath.Join(entryDir, memFileName)
	if err := fc.CreateSnapshot(ctx, statePath, memPath); err != nil {
		return fmt.Errorf("create snapshot: %w", err)
	}
	if err := verifySnapshotFiles(statePath, memPath); err != nil {
		return fmt.Errorf("verify snapshot: %w", err)
	}

	// Secret-scan the freshly written plaintext pair (ADR-0005
	// invariant 1): a pool entry that carries secret-shaped material
	// is never persisted — the launch fails and the cleanup path
	// removes every artifact. On a clean scan the verdict (scanner
	// version + the pair's plaintext digests) becomes part of the
	// entry artifact, giving every future claim an independent
	// clean-base signal.
	verdict, err := scanEntryClean(statePath, memPath)
	if err != nil {
		return fmt.Errorf("secret scan: %w", err)
	}

	// Encrypt the entry at rest (ADR-0005 invariant 5): a fresh
	// per-entry DEK encrypts both files in place (the plaintext is
	// zero-overwritten before unlink), and the DEK is sealed with the
	// node KEK under an AAD binding the entry's identity, its
	// class-image-boot provenance AND its secret-scan verdict.
	// Unencrypted pool state never survives this function.
	if err := sealEntryAtRest(o, entryDir, statePath, memPath, verdict); err != nil {
		return fmt.Errorf("seal entry at rest: %w", err)
	}

	// All good. Leave the paused VM alive for the node-agent to pick up.
	commit = true
	log.Printf("setec-pool-vm: paused and snapshotted entry %q", o.PoolEntryID)
	return nil
}

// waitForSocket returns once the Firecracker Unix-domain socket accepts
// connections. Before that point the API is not safe to call.
func waitForSocket(ctx context.Context, path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if time.Now().After(deadline) {
			return fmt.Errorf("socket %q not ready after %s", path, timeout)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		conn, err := net.DialTimeout("unix", path, 500*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(socketReadyPollInterval):
		}
	}
}

// configureAndBoot walks the Firecracker REST sequence required to
// bring a fresh VM up to the `Running` state:
//
//	PUT /boot-source { kernel_image_path, boot_args }
//	PUT /drives/rootfs { path_on_host, is_root_device, is_read_only }
//	PUT /machine-config { vcpu_count, mem_size_mib }
//	PUT /actions { action_type: InstanceStart }
//
// We issue these via extraClient so we do not have to widen the
// existing firecracker.Client interface (which intentionally stays
// narrow around the snapshot/pause surface).
func configureAndBoot(ctx context.Context, _ firecracker.Client, o Options) error {
	// The launcher speaks the full Firecracker API directly. It does
	// not use firecracker.Client for the bring-up sequence because
	// that Client deliberately covers only the snapshot/pause subset.
	ec := newExtraClient(o.SocketPath)

	bootBody := map[string]any{
		"kernel_image_path": o.KernelPath,
		"boot_args":         o.BootArgs,
	}
	if err := ec.do(ctx, "/boot-source", bootBody); err != nil {
		return fmt.Errorf("/boot-source: %w", err)
	}

	driveBody := map[string]any{
		"drive_id":       "rootfs",
		"path_on_host":   o.RootfsPath,
		"is_root_device": true,
		"is_read_only":   false,
	}
	if err := ec.do(ctx, "/drives/rootfs", driveBody); err != nil {
		return fmt.Errorf("/drives/rootfs: %w", err)
	}

	machineBody := map[string]any{
		"vcpu_count":   o.VCPUs,
		"mem_size_mib": o.MemoryMiB,
	}
	if err := ec.do(ctx, "/machine-config", machineBody); err != nil {
		return fmt.Errorf("/machine-config: %w", err)
	}

	// Attach a virtio-rng (entropy) device so the guest kernel has a
	// continuous host-backed entropy source. This is the snapshot RNG-safety
	// mechanism (ADR-0052, setec#66): a microVM restored from a Snapshot would
	// otherwise resume with the exact CRNG state captured at snapshot time —
	// every clone shares it, making nonces/keys/IDs predictable across
	// restores. With virtio-rng present, the guest's add_hwgenerator_randomness
	// path reseeds the kernel CRNG from fresh host entropy after resume. The
	// device is part of the VM config, so it is captured in the snapshot and
	// re-established on restore. No rate limiter — entropy must never be the
	// bottleneck. Configured before InstanceStart like every other device.
	if err := ec.do(ctx, "/entropy", map[string]any{}); err != nil {
		return fmt.Errorf("/entropy: %w", err)
	}

	// Attach a vsock device so the host can reach the in-guest
	// setec-guest-agent AFTER a snapshot restore. This is the ACTIVE
	// entropy-reseed transport (setec#72): post-LoadSnapshot the
	// node-agent connects to the device's host-side Unix socket,
	// performs the hybrid-vsock CONNECT handshake, and pushes fresh
	// entropy that the guest agent credits into the kernel CRNG. The
	// device is part of the VM config, so it is captured in the
	// snapshot and re-established on restore, exactly like /entropy.
	vsockBody := map[string]any{
		"guest_cid": o.GuestCID,
		"uds_path":  vsockUDSPath(o),
	}
	if err := ec.do(ctx, "/vsock", vsockBody); err != nil {
		return fmt.Errorf("/vsock: %w", err)
	}

	actionBody := map[string]any{"action_type": "InstanceStart"}
	if err := ec.do(ctx, "/actions", actionBody); err != nil {
		return fmt.Errorf("/actions InstanceStart: %w", err)
	}
	return nil
}

// vsockUDSPath resolves the host path Firecracker binds the vsock
// device's Unix socket to: the explicit --vsock-uds-path when set,
// otherwise <storage-root>/<pool-entry-id>/vsock.sock so the socket
// lives (and is cleaned up) with the rest of the entry state.
func vsockUDSPath(o Options) string {
	if o.VsockUDSPath != "" {
		return o.VsockUDSPath
	}
	return filepath.Join(o.StorageRoot, o.PoolEntryID, vsockFileName)
}

// socketAlive reports whether a live listener answers on the Unix
// socket at path. A dead leftover socket file does not connect.
func socketAlive(path string) bool {
	conn, err := net.DialTimeout("unix", path, 250*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// scanEntryClean runs the secret scanner over the plaintext
// state/memory pair and returns the clean verdict to record in the
// entry artifact (ADR-0005 invariant 1). Any finding fails the bake:
// findings are logged redacted, and the returned error wraps
// secretscan.ErrSecretsFound so the caller's cleanup path destroys
// the entry. A dirty verdict is therefore never persisted.
func scanEntryClean(statePath, memPath string) (poolentry.ScanVerdict, error) {
	stateFindings, stateDigest, err := secretscan.ScanArtifactSHA256(statePath)
	if err != nil && !errors.Is(err, secretscan.ErrSecretsFound) {
		return poolentry.ScanVerdict{}, err
	}
	memFindings, memDigest, err := secretscan.ScanArtifactSHA256(memPath)
	if err != nil && !errors.Is(err, secretscan.ErrSecretsFound) {
		return poolentry.ScanVerdict{}, err
	}
	if findings := append(stateFindings, memFindings...); len(findings) > 0 {
		for _, f := range findings {
			log.Printf("setec-pool-vm: secret scan: %s: %s", f.Path, f.Finding)
		}
		return poolentry.ScanVerdict{}, fmt.Errorf("%d finding(s) in the snapshot pair; refusing to persist the entry: %w",
			len(findings), secretscan.ErrSecretsFound)
	}
	return poolentry.ScanVerdict{
		ScannerVersion: secretscan.Version(),
		Clean:          true,
		StateSHA256:    stateDigest,
		MemorySHA256:   memDigest,
	}, nil
}

// sealEntryAtRest encrypts the entry's state/memory pair with a fresh
// per-entry DEK, seals the DEK with the node KEK (AAD = entry identity
// + provenance + scan verdict), and writes the provenance and scan
// records. Called only after verifySnapshotFiles and scanEntryClean,
// so both plaintext files exist, are non-empty, and scanned clean.
func sealEntryAtRest(o Options, entryDir, statePath, memPath string, verdict poolentry.ScanVerdict) error {
	kek, err := atrest.LoadOrCreateKEK(o.KeyFile)
	if err != nil {
		return err
	}
	dek, err := atrest.NewDEK()
	if err != nil {
		return err
	}
	if err := atrest.EncryptFile(statePath, dek); err != nil {
		return err
	}
	if err := atrest.EncryptFile(memPath, dek); err != nil {
		return err
	}
	prov := poolentry.Provenance{
		Source:     poolentry.SourceClassImageBoot,
		ImageRef:   o.ImageRef,
		KernelPath: o.KernelPath,
		RootfsPath: o.RootfsPath,
	}
	sealed, err := atrest.SealDEK(kek, dek, poolentry.DEKAAD(o.PoolEntryID, prov, verdict))
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(entryDir, poolentry.DEKFile), sealed, 0o600); err != nil {
		return fmt.Errorf("write sealed DEK: %w", err)
	}
	if err := poolentry.WriteProvenance(entryDir, prov); err != nil {
		return err
	}
	return poolentry.WriteScan(entryDir, verdict)
}

// verifySnapshotFiles asserts both files exist and are non-empty.
// Firecracker returns success on CreateSnapshot even when it has
// written zero-byte files in pathological I/O conditions; we surface
// that immediately rather than handing the node-agent a broken entry.
func verifySnapshotFiles(statePath, memPath string) error {
	for _, p := range []string{statePath, memPath} {
		fi, err := os.Stat(p)
		if err != nil {
			return fmt.Errorf("stat %q: %w", p, err)
		}
		if fi.Size() == 0 {
			return fmt.Errorf("snapshot file %q is zero-length", p)
		}
	}
	return nil
}

// killProcess attempts a graceful SIGTERM, waits up to the configured
// grace, and SIGKILLs on timeout. Errors are logged but not fatal —
// cleanup is best-effort.
func killProcess(proc SpawnedProcess, grace time.Duration) {
	if proc == nil {
		return
	}
	if err := proc.Signal(syscall.SIGTERM); err != nil {
		log.Printf("setec-pool-vm: sigterm firecracker: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- proc.Wait() }()
	select {
	case <-done:
		return
	case <-time.After(grace):
		log.Printf("setec-pool-vm: firecracker did not exit within %s; sending SIGKILL", grace)
		if err := proc.Signal(syscall.SIGKILL); err != nil {
			log.Printf("setec-pool-vm: sigkill firecracker: %v", err)
		}
		<-done
	}
}

func main() {
	opts, err := parseFlags(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := runLauncher(ctx, opts, execSpawner{}, firecracker.NewClientFromSocket); err != nil {
		log.Printf("setec-pool-vm: %v", err)
		os.Exit(1)
	}
}

// ensureUnused keeps io imported so future readers do not chase a
// compile error if they re-introduce stream-based configuration.
var _ = io.Discard
