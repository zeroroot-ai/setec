// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package installer

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// gVisor node preparation.
//
// Why this exists: the sanctioned kind profile runs setec with gvisor as its
// ONE backend, and until now the installer laid kata only. So a plain
// `helm install` produced a cluster where the runtime agent reported
// `setec.zeroroot.ai/runtime.gvisor=false` and no Sandbox ever scheduled. The
// bring-up scripts did this node prep outside the chart, which left a developer
// who ran only helm with a platform agents cannot run jobs on (setec#89).
//
// Two binaries, both laid from the image payload rather than downloaded, so an
// air-gapped install works and the node never reaches the internet:
//
//   - runsc, the gVisor sentry.
//   - containerd-shim-runsc-v1, which containerd's runtime_type
//     "io.containerd.runsc.v1" resolves to on PATH.
//
// Unlike kata there is no tarball and no VERSION file: gVisor ships two
// standalone binaries, so the idempotence key is the pinned version this image
// was built with, recorded beside them on the host.
const (
	gvisorBinDir = "/usr/local/bin"

	gvisorRunscBin = "/usr/local/bin/runsc"
	gvisorShimBin  = "/usr/local/bin/containerd-shim-runsc-v1"

	// gvisorVersionFile records the runsc version laid by this installer, so a
	// re-run with a newer image replaces the binaries and a re-run with the
	// same one writes nothing. It lives beside the binaries rather than in
	// /opt, because gVisor has no payload directory of its own.
	gvisorVersionFile = "/usr/local/lib/setec/gvisor-version"

	// gvisorPayloadSubdir is where Dockerfile.installer stages the two
	// binaries plus their VERSION, under the installer's payload root.
	gvisorPayloadSubdir = "gvisor"
)

// requiredGvisorArtifacts lists everything the gvisor handler needs at runtime,
// host-absolute. Verified in the payload before any write, and on the host
// after, exactly as the kata step does: the image build should have caught a
// bad prune, but the installer never trusts the image.
var requiredGvisorArtifacts = []string{
	gvisorRunscBin,
	gvisorShimBin,
}

// ensureGvisorPayload lays the bundled runsc and its containerd shim onto the
// host. Reports whether anything changed, so the caller can decide about a
// containerd restart.
func (in *Installer) ensureGvisorPayload() (bool, error) {
	payloadRoot := filepath.Join(in.cfg.PayloadDir, gvisorPayloadSubdir)

	payloadVersion, err := os.ReadFile(filepath.Join(payloadRoot, "VERSION"))
	if err != nil {
		return false, fmt.Errorf("installer image gvisor payload has no readable VERSION file at %s: %w", payloadRoot, err)
	}

	// Payload completeness first. A missing binary here is an image defect, and
	// failing before any host write keeps a bad image from half-preparing a node.
	for _, artifact := range requiredGvisorArtifacts {
		name := filepath.Base(artifact)
		if _, err := os.Stat(filepath.Join(payloadRoot, name)); err != nil {
			return false, fmt.Errorf("installer image gvisor payload is missing %s: %w", name, err)
		}
	}

	hostVersion, err := os.ReadFile(in.hostPath(gvisorVersionFile))
	upToDate := err == nil && string(hostVersion) == string(payloadVersion)
	if upToDate {
		// Version matches, so verify the binaries are actually there before
		// trusting it. A node someone cleaned /usr/local/bin on must reconverge
		// rather than pass on a surviving version file.
		for _, artifact := range requiredGvisorArtifacts {
			if _, err := os.Stat(in.hostPath(artifact)); err != nil {
				upToDate = false
				break
			}
		}
	}
	if upToDate {
		return false, nil
	}

	in.log("laying gvisor payload version %s onto host %s", strings.TrimSpace(string(payloadVersion)), gvisorBinDir)
	for _, artifact := range requiredGvisorArtifacts {
		name := filepath.Base(artifact)
		src := filepath.Join(payloadRoot, name)
		dst := in.hostPath(artifact)
		// 0755: runsc is invoked by containerd as root and must be executable.
		if err := copyFile(src, dst, 0o755); err != nil {
			return false, fmt.Errorf("install %s: %w", name, err)
		}
	}

	if err := os.MkdirAll(filepath.Dir(in.hostPath(gvisorVersionFile)), 0o755); err != nil {
		return false, fmt.Errorf("create gvisor version dir: %w", err)
	}
	if _, err := writeFileIfChanged(in.hostPath(gvisorVersionFile), payloadVersion, 0o644); err != nil {
		return false, fmt.Errorf("record gvisor version: %w", err)
	}

	// Final on-host sanity: the handler's binaries must exist now. The version
	// file is written before this check on purpose — a node that passes here
	// and fails later reconverges, but one that silently lacks a version file
	// would re-copy on every single pass.
	for _, artifact := range requiredGvisorArtifacts {
		if _, err := os.Stat(in.hostPath(artifact)); err != nil {
			return true, fmt.Errorf("gvisor artifact %s missing after install: %w", artifact, err)
		}
	}
	return true, nil
}
