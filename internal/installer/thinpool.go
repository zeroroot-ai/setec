// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package installer

import (
	"context"
	_ "embed"
	"fmt"
	"strings"
)

// The boot-time provisioner script ships inside the installer binary so
// the exact bytes that were unit-tested are the bytes on every node.
//
//go:embed payload/setec-thinpool.sh
var thinpoolScript []byte

// Host locations managed by the thin-pool step.
const (
	thinpoolScriptPath = "/usr/local/sbin/setec-thinpool.sh"
	thinpoolEnvPath    = "/etc/setec/thinpool.env"
	thinpoolUnitPath   = "/etc/systemd/system/setec-thinpool.service"
	thinpoolUnitName   = "setec-thinpool.service"
)

// thinpoolUnit is the systemd unit that runs the provisioner at every
// boot, before any container runtime this chart supports. Before= lines
// naming units that do not exist on a given host are ignored by systemd,
// so one static unit covers stock containerd, k3s servers and k3s
// agents.
const thinpoolUnit = `# Managed by the setec installer DaemonSet (zeroroot-ai/setec) — DO NOT EDIT.
# Provisions the containerd devmapper thin-pool before the container
# runtime starts. containerd's devmapper snapshotter fails plugin init
# when the pool device is absent, so the runtime must not start first.
[Unit]
Description=Setec containerd devmapper thin-pool
DefaultDependencies=no
After=local-fs.target
Before=containerd.service k3s.service k3s-agent.service
ConditionPathExists=` + thinpoolScriptPath + `

[Service]
Type=oneshot
RemainAfterExit=yes
EnvironmentFile=-` + thinpoolEnvPath + `
ExecStart=/bin/sh ` + thinpoolScriptPath + `

[Install]
WantedBy=multi-user.target
`

// orderingDropin is installed at <runtime unit>.d/10-setec-thinpool.conf
// so the runtime hard-requires the pool at boot (Before= on our side
// orders but does not require).
const orderingDropin = `# Managed by the setec installer DaemonSet (zeroroot-ai/setec) — DO NOT EDIT.
# The devmapper snapshotter fails plugin init when the thin-pool device
# is absent, so the container runtime must not start until the pool
# exists.
[Unit]
Requires=` + thinpoolUnitName + `
After=` + thinpoolUnitName + `
`

// thinpoolEnv renders /etc/setec/thinpool.env from the configuration.
func (in *Installer) thinpoolEnv() []byte {
	var b strings.Builder
	b.WriteString("# Managed by the setec installer DaemonSet (zeroroot-ai/setec) — DO NOT EDIT.\n")
	fmt.Fprintf(&b, "SETEC_THINPOOL_MODE=%s\n", in.cfg.ThinpoolMode)
	fmt.Fprintf(&b, "SETEC_THINPOOL_NAME=%s\n", in.cfg.PoolName)
	fmt.Fprintf(&b, "SETEC_DEVMAPPER_ROOT=%s\n", in.cfg.DevmapperRoot)
	switch in.cfg.ThinpoolMode {
	case ThinpoolModeLoop:
		fmt.Fprintf(&b, "SETEC_THINPOOL_DIR=%s\n", in.cfg.LoopDir)
		fmt.Fprintf(&b, "SETEC_THINPOOL_DATA_GB=%d\n", in.cfg.LoopDataGB)
		fmt.Fprintf(&b, "SETEC_THINPOOL_META_GB=%d\n", in.cfg.LoopMetaGB)
	case ThinpoolModeDevice:
		fmt.Fprintf(&b, "SETEC_THINPOOL_DATA_DEVICE=%s\n", in.cfg.DataDevice)
		fmt.Fprintf(&b, "SETEC_THINPOOL_META_DEVICE=%s\n", in.cfg.MetadataDevice)
	}
	return []byte(b.String())
}

// ensureThinpool installs the provisioner assets, wires boot ordering,
// and provisions the pool immediately so the containerd restart that may
// follow finds it. Returns whether any asset changed.
func (in *Installer) ensureThinpool(ctx context.Context, flavor runtimeFlavor) (bool, error) {
	changed := false

	c, err := writeFileIfChanged(in.hostPath(thinpoolScriptPath), thinpoolScript, 0o755)
	if err != nil {
		return changed, err
	}
	changed = changed || c

	c, err = writeFileIfChanged(in.hostPath(thinpoolEnvPath), in.thinpoolEnv(), 0o644)
	if err != nil {
		return changed, err
	}
	envChanged := c
	changed = changed || c

	c, err = writeFileIfChanged(in.hostPath(thinpoolUnitPath), []byte(thinpoolUnit), 0o644)
	if err != nil {
		return changed, err
	}
	changed = changed || c

	dropinPath := fmt.Sprintf("/etc/systemd/system/%s.d/10-setec-thinpool.conf", flavor.unit)
	c, err = writeFileIfChanged(in.hostPath(dropinPath), []byte(orderingDropin), 0o644)
	if err != nil {
		return changed, err
	}
	changed = changed || c

	if changed {
		if _, err := in.cfg.Runner.Run(ctx, "systemctl", "daemon-reload"); err != nil {
			return changed, err
		}
	}
	// enable is idempotent and cheap; always assert it so a node where
	// someone ran `systemctl disable` reconverges.
	if _, err := in.cfg.Runner.Run(ctx, "systemctl", "enable", thinpoolUnitName); err != nil {
		return changed, err
	}

	// Provision the pool NOW. When only the env changed (e.g. resized
	// values) the unit must actually re-run, so restart instead of start
	// in that case; a plain start is a no-op on an already-active
	// oneshot with RemainAfterExit.
	verb := "start"
	if envChanged {
		verb = "restart"
	}
	if _, err := in.cfg.Runner.Run(ctx, "systemctl", verb, thinpoolUnitName); err != nil {
		return changed, fmt.Errorf("provisioning thin-pool via systemctl %s %s: %w", verb, thinpoolUnitName, err)
	}

	// The pool device must exist before containerd (re)starts.
	if _, err := in.cfg.Runner.Run(ctx, "dmsetup", "info", in.cfg.PoolName); err != nil {
		return changed, fmt.Errorf("thin-pool %s not active after provisioning: %w", in.cfg.PoolName, err)
	}
	return changed, nil
}
