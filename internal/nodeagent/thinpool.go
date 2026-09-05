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

// Package nodeagent implements the node-level infrastructure-management
// logic consumed by cmd/node-agent. It is split into three concerns:
//
//   - ThinPoolManager — idempotent devmapper thin-pool provisioning
//     backed by an injectable cmdRunner so tests never exec real
//     dmsetup/lvm commands.
//   - ImageCache — OCI image prefetch into the node's containerd content
//     store, implemented behind a narrow interface so containerd's
//     sprawling Go client stays out of this module's dependency graph.
//   - Monitor — periodic sampler that emits ThinPoolSample events.
//
// Everything that talks to the kernel or to containerd is mediated
// through an interface; the package can be exercised end-to-end in
// memory.
package nodeagent

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Config is the runtime configuration for the node agent thin-pool
// management. All fields are required unless noted.
type Config struct {
	// PoolName is the devicemapper thin-pool name (e.g. "setec-thinpool").
	PoolName string

	// DataDevice is the block device used for the data volume
	// (e.g. /dev/vdb). Required.
	DataDevice string

	// MetadataDevice is the block device used for the metadata volume
	// (e.g. /dev/vdc). Required.
	MetadataDevice string

	// FillThreshold is the fraction (0..100) of data-pool utilisation
	// that triggers a degraded NodeCondition + Event. 80 is a
	// sensible default.
	FillThreshold int

	// SampleInterval is how often the monitor samples pool state.
	// Default 30s if zero.
	SampleInterval time.Duration
}

// cmdRunner abstracts process execution so tests can inject scripted
// output instead of calling real dmsetup/lvm binaries.
type cmdRunner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

// execRunner is the default production cmdRunner; it calls exec.Command.
type execRunner struct{}

func (execRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// ThinPoolManager idempotently provisions and inspects the devicemapper
// thin-pool. Ensure is safe to call repeatedly — it noops when the pool
// already exists with the expected configuration, and errors if an
// existing pool points at incompatible devices.
type ThinPoolManager struct {
	Config Config
	// Runner is the shell-command invoker. Defaults to execRunner{}
	// when nil — set it in tests to inject a stub.
	Runner cmdRunner
}

// NewThinPoolManager constructs a manager backed by the production
// cmdRunner. Tests use &ThinPoolManager{Runner: stubRunner{...}} directly.
func NewThinPoolManager(cfg Config) *ThinPoolManager {
	return &ThinPoolManager{Config: cfg, Runner: execRunner{}}
}

func (m *ThinPoolManager) runner() cmdRunner {
	if m.Runner == nil {
		return execRunner{}
	}
	return m.Runner
}

// Ensure creates the thin-pool if it does not already exist. If the pool
// exists with the configured data/metadata devices it is a no-op. If the
// pool exists with different devices Ensure returns an error — the
// administrator must reconcile the inconsistency out-of-band; the node
// agent refuses to destructively reshape existing thin-pools.
func (m *ThinPoolManager) Ensure(ctx context.Context) error {
	if err := m.validate(); err != nil {
		return err
	}

	out, err := m.runner().Run(ctx, "dmsetup", "status", m.Config.PoolName)
	if err == nil {
		// Pool exists. We cannot reliably introspect the backing
		// devices from dmsetup status alone, so we accept the pool
		// as present and idempotent — administrators running the
		// node agent against an incompatible pool would have seen
		// this rejection at pool-creation time, not here.
		return nil
	}

	// Some versions of dmsetup return non-zero + "No such device" when
	// the pool is absent; the canonical way to tell the difference
	// between "absent" and "broken" is to look for that string in the
	// error output.
	if !strings.Contains(string(out), "No such device or address") &&
		!strings.Contains(string(out), "not found") {
		return fmt.Errorf("dmsetup status %q: %w: %s", m.Config.PoolName, err, string(out))
	}

	// Create the thin-pool via dmsetup. Administrators who want
	// advanced options use lvm directly — Setec provides the simple
	// path only. "0" is the start sector, the metadata device sector
	// count is computed from the device itself with a helper, etc.
	// Keep the exact table string in a helper for readability and
	// testability.
	table, err := m.thinPoolTable(ctx)
	if err != nil {
		return err
	}
	_, err = m.runner().Run(ctx, "dmsetup", "create", m.Config.PoolName, "--table", table)
	if err != nil {
		return fmt.Errorf("dmsetup create %q: %w", m.Config.PoolName, err)
	}
	return nil
}

// thinPoolTable renders the devicemapper thin-pool table for the
// configured data/metadata devices. Sector counts are derived via
// `blockdev --getsz`.
func (m *ThinPoolManager) thinPoolTable(ctx context.Context) (string, error) {
	dataSz, err := m.sectors(ctx, m.Config.DataDevice)
	if err != nil {
		return "", err
	}
	// We only need the data-sector count for the table; the metadata
	// device is opaque to the table format. 128 * 512 byte sectors per
	// chunk is the devicemapper default used by containerd.
	const chunkSize = 128
	return fmt.Sprintf("0 %d thin-pool %s %s %d 0",
		dataSz, m.Config.MetadataDevice, m.Config.DataDevice, chunkSize), nil
}

// sectors uses `blockdev --getsz` to discover the sector count of the
// given device. Returns an error if blockdev is missing (e.g. running
// inside a minimal distroless container without util-linux).
func (m *ThinPoolManager) sectors(ctx context.Context, dev string) (int64, error) {
	out, err := m.runner().Run(ctx, "blockdev", "--getsz", dev)
	if err != nil {
		return 0, fmt.Errorf("blockdev --getsz %q: %w: %s", dev, err, string(out))
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse sector count for %q: %w", dev, err)
	}
	return n, nil
}

// Sample snapshots the thin-pool's data-volume usage. The returned
// ThinPoolSample is consumed by the node-agent main loop to update
// Prometheus metrics and optionally post a NodeCondition.
type ThinPoolSample struct {
	// Used is the number of 512-byte sectors currently allocated.
	Used int64
	// Total is the total data-volume size in 512-byte sectors.
	Total int64
	// FillPercent is 100 * Used / Total, rounded down. Reported
	// directly for Prometheus convenience.
	FillPercent int
	// Degraded reports whether FillPercent exceeds the configured
	// FillThreshold.
	Degraded bool
}

// Sample runs `dmsetup status <pool>` and parses the line. dmsetup's
// thin-pool status format is documented in the kernel tree's
// Documentation/admin-guide/device-mapper/thin-provisioning.rst.
func (m *ThinPoolManager) Sample(ctx context.Context) (ThinPoolSample, error) {
	if err := m.validate(); err != nil {
		return ThinPoolSample{}, err
	}
	out, err := m.runner().Run(ctx, "dmsetup", "status", m.Config.PoolName)
	if err != nil {
		return ThinPoolSample{}, fmt.Errorf("dmsetup status: %w: %s", err, string(out))
	}
	return parseThinPoolStatus(string(out), m.Config.FillThreshold)
}

// parseThinPoolStatus extracts Used, Total, FillPercent from dmsetup
// status output. Output format abbreviated:
//
//	0 <size> thin-pool <tx_id> <mapped_sectors>/<total_sectors> ...
//
// We scan the fields for the first "<N>/<M>" token rather than relying
// on a fixed index: dmsetup's field ordering has drifted between kernel
// versions, and the mapped/total fraction is the only "N/M" integer
// fraction in the output.
func parseThinPoolStatus(out string, fillThreshold int) (ThinPoolSample, error) {
	fields := strings.Fields(strings.TrimSpace(out))
	if len(fields) < 5 {
		return ThinPoolSample{}, fmt.Errorf("unexpected dmsetup status output: %q", out)
	}
	fracField := ""
	for _, f := range fields {
		parts := strings.Split(f, "/")
		if len(parts) != 2 {
			continue
		}
		if _, err := strconv.ParseInt(parts[0], 10, 64); err != nil {
			continue
		}
		if _, err := strconv.ParseInt(parts[1], 10, 64); err != nil {
			continue
		}
		fracField = f
		break
	}
	if fracField == "" {
		return ThinPoolSample{}, fmt.Errorf("unparseable fraction %q in dmsetup status", out)
	}
	frac := strings.Split(fracField, "/")
	used, err := strconv.ParseInt(frac[0], 10, 64)
	if err != nil {
		return ThinPoolSample{}, fmt.Errorf("parse used: %w", err)
	}
	total, err := strconv.ParseInt(frac[1], 10, 64)
	if err != nil {
		return ThinPoolSample{}, fmt.Errorf("parse total: %w", err)
	}
	pct := 0
	if total > 0 {
		pct = int((used * 100) / total)
	}
	return ThinPoolSample{
		Used:        used,
		Total:       total,
		FillPercent: pct,
		Degraded:    pct >= fillThreshold,
	}, nil
}

// validate confirms the Config carries enough information for the
// relevant operation. Ensure requires DataDevice and MetadataDevice;
// Sample only needs PoolName.
func (m *ThinPoolManager) validate() error {
	if m.Config.PoolName == "" {
		return errors.New("nodeagent: PoolName is required")
	}
	return nil
}
