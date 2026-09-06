// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package firecracker

// HostBinaryPath is the host-absolute path of the Firecracker binary on a node
// prepared by setec's own installer DaemonSet — the default node-prep path
// (ADR-0003).
//
// It is the SINGLE definition shared by the two sides that must agree: the
// installer, which places the kata payload (internal/installer.kataFCBin), and
// the pool-VM launcher, which execs the binary
// (cmd/setec-pool-vm --firecracker-binary).
//
// Those two used to carry independent string literals and had drifted: the
// launcher defaulted to /usr/local/bin/firecracker ("the standard path
// kata-deploy lays down") while the installer only ever created the shim
// symlink and left Firecracker at /opt/kata/bin/firecracker, where the stock
// configuration-fc.toml references it. On any installer-prepared node the
// launcher's default therefore pointed at a file that does not exist
// (setec#287).
//
// Keep it a shared constant rather than two literals kept in sync by review:
// drift here is silent until a pool VM fails to spawn on a real node.
const HostBinaryPath = "/opt/kata/bin/firecracker"
