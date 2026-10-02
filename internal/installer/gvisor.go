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
// Why this exists: the sanctioned kind profile runs setec with gvisor as its ONE
// backend, and until now the installer laid kata only. Nothing else installs
// runsc — kata-deploy does not ship it — so a plain `helm install` produced a
// node whose runtime agent reported `setec.zeroroot.ai/runtime.gvisor=false`,
// and no Sandbox ever scheduled. The bring-up scripts did this prep outside the
// chart, which left a developer who ran only helm with a platform agents cannot
// run jobs on (setec#89).
//
// THE WHOLE PAYLOAD IS LAID, not a prune. Measured on release-20260928.0: with
// only runsc and containerd-shim-runsc-v1 on disk, runsc dies at sandbox
// creation with `sidecar "gvisor_sentry" not usable ... --sidecar-usage-policy
// is set to STRICT`. The sentry is a separate binary under gvisor-bin/ in modern
// releases, and the only policy that tolerates its absence is
// LEGACY_DEPRECATED_SLOW_EMBEDDED_FALLBACK — deprecated and slow by upstream's
// own naming, on the cold-start path this repo exports as
// setec_sandbox_cold_start_seconds, for the one backend chosen because it has
// LOWER overhead than a microVM.
//
// The layout mirrors /opt/kata on purpose: runsc resolves gvisor-bin/ relative
// to the RESOLVED target of its own path, so a /usr/local/bin symlink works and
// a directory never has to live inside a bin dir. Verified both directly and
// through a symlink.
const (
	gvisorHostDir = "/opt/gvisor"

	gvisorRunscBin = "/opt/gvisor/runsc"
	gvisorShimBin  = "/opt/gvisor/containerd-shim-runsc-v1"
	// gvisorSentryBin is what every sandbox launch needs. runsc refuses to
	// create a sandbox without it under the default sidecar policy, so it is
	// verified like a first-class artifact rather than assumed present.
	gvisorSentryBin = "/opt/gvisor/gvisor-bin/gvisor_sentry"

	// Symlinks giving containerd's runtime_type "io.containerd.runsc.v1" a
	// containerd-shim-runsc-v1 on PATH, and that shim a runsc on PATH, exactly
	// as the kata step does for its own shim.
	gvisorRunscLink = "/usr/local/bin/runsc"
	gvisorShimLink  = "/usr/local/bin/containerd-shim-runsc-v1"
)

// requiredGvisorArtifacts lists everything the gvisor handler needs at runtime,
// host-absolute. Verified in the payload before any host write, and on the host
// after, exactly as the kata step does: the image build should have caught a bad
// payload, but the installer never trusts the image.
var requiredGvisorArtifacts = []string{
	gvisorRunscBin,
	gvisorShimBin,
	gvisorSentryBin,
}

// ensureGvisorPayload lays the bundled gVisor release onto the host. The
// payload's VERSION file is the idempotence key: matching version plus intact
// artifacts means zero writes.
func (in *Installer) ensureGvisorPayload() (bool, error) {
	payloadRoot := in.cfg.GvisorPayloadDir

	payloadVersion, err := os.ReadFile(filepath.Join(payloadRoot, "VERSION"))
	if err != nil {
		return false, fmt.Errorf("installer image gvisor payload has no readable VERSION file at %s: %w", payloadRoot, err)
	}

	// Payload completeness before any host write, so a bad image cannot
	// half-prepare a node.
	for _, artifact := range requiredGvisorArtifacts {
		rel := strings.TrimPrefix(artifact, gvisorHostDir+"/")
		if _, err := os.Stat(filepath.Join(payloadRoot, rel)); err != nil {
			return false, fmt.Errorf("installer image gvisor payload is missing %s: %w", rel, err)
		}
	}

	changed := false
	hostVersion, err := os.ReadFile(in.hostPath(gvisorHostDir, "VERSION"))
	upToDate := err == nil && string(hostVersion) == string(payloadVersion)
	if upToDate {
		// Version matches — verify the artifacts are intact before trusting it.
		// A partially deleted /opt/gvisor with a surviving VERSION file must
		// reconverge rather than pass.
		for _, artifact := range requiredGvisorArtifacts {
			if _, err := os.Stat(in.hostPath(artifact)); err != nil {
				upToDate = false
				break
			}
		}
	}
	if !upToDate {
		in.log("laying gvisor payload version %s onto host %s", strings.TrimSpace(string(payloadVersion)), gvisorHostDir)
		// Stage into a sibling and swap, so a crash mid-copy never leaves a
		// half-written /opt/gvisor that passes the VERSION check next run.
		staging := in.hostPath(gvisorHostDir) + ".setec-staging"
		if err := os.RemoveAll(staging); err != nil {
			return false, err
		}
		if err := copyTree(payloadRoot, staging); err != nil {
			return false, err
		}
		old := in.hostPath(gvisorHostDir) + ".setec-old"
		if err := os.RemoveAll(old); err != nil {
			return false, err
		}
		if _, err := os.Stat(in.hostPath(gvisorHostDir)); err == nil {
			if err := os.Rename(in.hostPath(gvisorHostDir), old); err != nil {
				return false, err
			}
		}
		if err := os.Rename(staging, in.hostPath(gvisorHostDir)); err != nil {
			return false, err
		}
		if err := os.RemoveAll(old); err != nil {
			return false, err
		}
		changed = true
	}

	// PATH links. containerd resolves the shim by name, and the shim resolves
	// runsc by name.
	for link, target := range map[string]string{
		gvisorRunscLink: gvisorRunscBin,
		gvisorShimLink:  gvisorShimBin,
	} {
		linkChanged, err := ensureSymlink(in.hostPath(link), target)
		if err != nil {
			return changed, err
		}
		changed = changed || linkChanged
	}

	// Final on-host sanity: everything the handler needs must exist now.
	for _, artifact := range requiredGvisorArtifacts {
		if _, err := os.Stat(in.hostPath(artifact)); err != nil {
			return changed, fmt.Errorf("gvisor artifact %s missing after install: %w", artifact, err)
		}
	}
	return changed, nil
}
