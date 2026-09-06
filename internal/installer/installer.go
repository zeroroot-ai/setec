// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

// Package installer implements the portable node installer consumed by
// cmd/installer (ADR-0003). Given any x86 KVM-capable node, it converges
// the node to run kata-fc Firecracker microVMs:
//
//   - lays down the stock Kata Containers static release (bundled in the
//     installer image as an immutable payload) under /opt/kata,
//   - installs a boot-time devmapper thin-pool provisioner
//     (setec-thinpool.service) ordered Before= the container runtime so
//     containerd never starts against a missing pool,
//   - registers the kata-fc runtime handler and the devmapper snapshotter
//     with containerd (stock containerd drop-in, or the k3s config
//     template mechanism), and restarts the runtime exactly once when the
//     registration changed.
//
// The kata-fc RuntimeClass itself is rendered by the Helm chart
// (runtimes.kata-fc.install), and node capability labels are owned by the
// runtime-agent DaemonSet — the installer needs no Kubernetes API access
// at all.
//
// Everything that touches the host goes through two seams so the whole
// convergence loop is unit-testable without a real node: file operations
// are rooted at Config.HostRoot, and subprocesses go through the Runner
// interface (production: chroot into HostRoot, see exec.go).
//
// Idempotence contract (issue #187): every step compares desired state
// against on-disk state and reports whether it changed anything; a re-run
// on a converged node performs zero writes and zero runtime restarts.
// When the node's kata-fc registration is owned by something else
// (kata-deploy, a baked AMI, an admin) the installer stands down rather
// than fight for ownership.
package installer

import (
	"context"
	"fmt"
	"time"

	"github.com/zeroroot-ai/setec/internal/runtimeagent/probe"
)

// Thin-pool provisioning modes.
const (
	// ThinpoolModeLoop backs the pool with sparse files attached to loop
	// devices. Works on any node with a filesystem — the portable default.
	ThinpoolModeLoop = "loop"
	// ThinpoolModeDevice builds the pool from two dedicated block devices
	// supplied by the cluster administrator.
	ThinpoolModeDevice = "device"
)

// Config parameterises a convergence run. Zero values are filled in by
// ApplyDefaults.
type Config struct {
	// HostRoot is where the node's root filesystem is mounted inside the
	// installer container (the DaemonSet mounts hostPath / here).
	HostRoot string

	// PayloadDir is the kata static release tree bundled in the installer
	// image (the directory that becomes /opt/kata on the host).
	PayloadDir string

	// PoolName is the devmapper thin-pool name registered with the
	// containerd devmapper snapshotter.
	PoolName string

	// ThinpoolMode selects how the pool is backed: ThinpoolModeLoop or
	// ThinpoolModeDevice.
	ThinpoolMode string

	// LoopDir is the host directory holding the sparse backing files in
	// loop mode.
	LoopDir string

	// LoopDataGB / LoopMetaGB size the sparse data and metadata backing
	// files in loop mode.
	LoopDataGB int
	LoopMetaGB int

	// DataDevice / MetadataDevice are the dedicated block devices used in
	// device mode.
	DataDevice     string
	MetadataDevice string

	// DevmapperRoot is the containerd devmapper snapshotter root_path.
	DevmapperRoot string

	// BaseImageSize is the devmapper snapshotter base_image_size (bytes,
	// as a string, matching containerd's config format).
	BaseImageSize string

	// RestartTimeout bounds how long to wait for the container runtime to
	// come back active after a config-triggered restart.
	RestartTimeout time.Duration

	// Runner executes commands on the host. Production uses NewHostRunner;
	// tests inject a fake.
	Runner Runner
}

// ApplyDefaults fills zero fields with production defaults.
func (c *Config) ApplyDefaults() {
	if c.HostRoot == "" {
		c.HostRoot = "/host"
	}
	if c.PayloadDir == "" {
		c.PayloadDir = "/opt/kata"
	}
	if c.PoolName == "" {
		c.PoolName = "setec-thinpool"
	}
	if c.ThinpoolMode == "" {
		c.ThinpoolMode = ThinpoolModeLoop
	}
	if c.LoopDir == "" {
		c.LoopDir = "/var/lib/setec/thinpool"
	}
	if c.LoopDataGB == 0 {
		c.LoopDataGB = 50
	}
	if c.LoopMetaGB == 0 {
		c.LoopMetaGB = 2
	}
	if c.DevmapperRoot == "" {
		c.DevmapperRoot = "/var/lib/containerd/io.containerd.snapshotter.v1.devmapper"
	}
	if c.BaseImageSize == "" {
		c.BaseImageSize = "8589934592"
	}
	if c.RestartTimeout == 0 {
		c.RestartTimeout = 2 * time.Minute
	}
	if c.Runner == nil {
		c.Runner = NewHostRunner(c.HostRoot)
	}
}

// Validate rejects inconsistent configuration before any host mutation.
func (c *Config) Validate() error {
	switch c.ThinpoolMode {
	case ThinpoolModeLoop:
		if c.LoopDataGB <= 0 || c.LoopMetaGB <= 0 {
			return fmt.Errorf("thinpool mode %q requires positive loop data/meta sizes", c.ThinpoolMode)
		}
	case ThinpoolModeDevice:
		if c.DataDevice == "" || c.MetadataDevice == "" {
			return fmt.Errorf("thinpool mode %q requires both data-device and metadata-device", c.ThinpoolMode)
		}
	default:
		return fmt.Errorf("unknown thinpool mode %q (want %q or %q)", c.ThinpoolMode, ThinpoolModeLoop, ThinpoolModeDevice)
	}
	return nil
}

// Outcome classifies the result of a convergence attempt.
type Outcome string

const (
	// OutcomeConverged — the node is (now) kata-fc capable and every
	// managed artifact matches desired state.
	OutcomeConverged Outcome = "converged"
	// OutcomeIdleNoKVM — the node exposes no KVM; nothing was touched.
	// Not an error: the DaemonSet targets x86 nodes by selector, but KVM
	// capability is only observable at runtime.
	OutcomeIdleNoKVM Outcome = "idle-no-kvm"
	// OutcomeIdleForeignOwner — kata-fc is already registered with the
	// node's containerd by something the installer does not manage
	// (kata-deploy, a baked image, an administrator). Nothing was touched.
	OutcomeIdleForeignOwner Outcome = "idle-foreign-owner"
)

// Result reports what a convergence run did.
type Result struct {
	Outcome Outcome
	// Changed is true when any host artifact was written this run.
	Changed bool
	// RuntimeRestarted is true when the container runtime was restarted.
	RuntimeRestarted bool
	// Flavor is the detected runtime flavor ("containerd" or "k3s");
	// empty when the run idled before detection.
	Flavor string
}

// Installer converges a node. Construct with New.
type Installer struct {
	cfg Config
	log func(format string, args ...any)
}

// New builds an Installer. logf receives human-readable progress lines;
// nil discards them.
func New(cfg Config, logf func(format string, args ...any)) (*Installer, error) {
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Installer{cfg: cfg, log: logf}, nil
}

// Converge drives the node to the desired state. It is safe to call
// repeatedly; a converged node results in zero writes and no restart.
func (in *Installer) Converge(ctx context.Context) (Result, error) {
	res := Result{}

	// 1. KVM gate. Reuses the runtime-agent's probe logic so the
	// installer and the capability labeller can never disagree about
	// what "KVM-capable" means.
	if ok, reason := probe.KVMAvailable(in.cfg.HostRoot); !ok {
		in.log("KVM not available: %s — idling, node left untouched", reason)
		res.Outcome = OutcomeIdleNoKVM
		return res, nil
	}
	if _, loaded := probe.LoadedKVMModule(in.cfg.HostRoot); !loaded {
		in.log("no KVM kernel module loaded — idling, node left untouched")
		res.Outcome = OutcomeIdleNoKVM
		return res, nil
	}

	// 2. Runtime flavor + ownership. If kata-fc is registered by someone
	// else, the node is already capable — do not fight for ownership.
	flavor, err := detectFlavor(in.cfg)
	if err != nil {
		return res, fmt.Errorf("detecting container runtime flavor: %w", err)
	}
	res.Flavor = flavor.name
	owner := kataFCOwnership(in.cfg, flavor)
	if owner == ownerForeign {
		in.log("kata-fc already registered with %s by an external owner — standing down", flavor.name)
		res.Outcome = OutcomeIdleForeignOwner
		return res, nil
	}

	// 3. Host tooling preflight: everything the boot-time thin-pool
	// provisioner shells out to must exist on the host.
	if err := in.preflightHostTools(ctx); err != nil {
		return res, err
	}

	// 4. Kata payload.
	kataChanged, err := in.ensureKataPayload()
	if err != nil {
		return res, fmt.Errorf("installing kata payload: %w", err)
	}
	res.Changed = res.Changed || kataChanged

	// 5. Thin-pool provisioner assets + boot ordering, then provision the
	// pool NOW so the containerd restart below finds it.
	tpChanged, err := in.ensureThinpool(ctx, flavor)
	if err != nil {
		return res, fmt.Errorf("provisioning thin-pool: %w", err)
	}
	res.Changed = res.Changed || tpChanged

	// 6. Containerd registration. Restart the runtime only when the
	// registration actually changed.
	cdChanged, err := in.ensureContainerdConfig(ctx, flavor)
	if err != nil {
		return res, fmt.Errorf("configuring %s: %w", flavor.name, err)
	}
	res.Changed = res.Changed || cdChanged
	if cdChanged {
		if err := in.restartRuntime(ctx, flavor); err != nil {
			return res, err
		}
		res.RuntimeRestarted = true
	}

	// 7. Verify converged state.
	if err := in.verify(ctx, flavor); err != nil {
		return res, fmt.Errorf("post-convergence verification: %w", err)
	}

	res.Outcome = OutcomeConverged
	return res, nil
}

// preflightHostTools verifies the host binaries the thin-pool provisioner
// depends on. Failing here — before any write — keeps a missing tool from
// producing a half-converged node.
func (in *Installer) preflightHostTools(_ context.Context) error {
	required := []string{"sh", "dmsetup", "blockdev"}
	if in.cfg.ThinpoolMode == ThinpoolModeLoop {
		required = append(required, "losetup", "truncate")
	}
	var missing []string
	for _, tool := range required {
		if _, err := lookPathIn(in.cfg.HostRoot, tool); err != nil {
			missing = append(missing, tool)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf(
			"host is missing required tools %v: install them on the node (dmsetup ships in the device-mapper/dmsetup package, losetup and friends in util-linux/coreutils)",
			missing)
	}
	return nil
}
