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

package installer

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Host locations managed by the kata payload step. kataHostDir mirrors
// the stock kata static release layout (the tarball is rooted at
// ./opt/kata) so every path inside configuration-fc.toml resolves
// unchanged; the shim symlink gives containerd's runtime_type
// "io.containerd.kata-fc.v2" a containerd-shim-kata-fc-v2 on PATH,
// exactly as kata-deploy and the packer AMI do.
const (
	kataHostDir  = "/opt/kata"
	kataShimLink = "/usr/local/bin/containerd-shim-kata-fc-v2"
	kataCLILink  = "/usr/local/bin/kata-runtime"

	kataShimBin = "/opt/kata/bin/containerd-shim-kata-v2"
	kataFCBin   = "/opt/kata/bin/firecracker"
	kataJailer  = "/opt/kata/bin/jailer"
	kataFCConf  = "/opt/kata/share/defaults/kata-containers/configuration-fc.toml"
)

// requiredKataArtifacts lists everything the kata-fc handler needs at
// runtime, host-absolute. Verified in the payload (image build should
// have caught a bad prune, but never trust the image) and on the host
// after installation.
var requiredKataArtifacts = []string{
	kataShimBin,
	kataFCBin,
	kataJailer,
	kataFCConf,
}

// ensureKataPayload lays the bundled kata static release onto the host.
// The payload's VERSION file is the idempotence key: matching version +
// intact artifacts means zero writes.
func (in *Installer) ensureKataPayload() (bool, error) {
	payloadVersion, err := os.ReadFile(filepath.Join(in.cfg.PayloadDir, "VERSION"))
	if err != nil {
		return false, fmt.Errorf("installer image payload has no readable VERSION file at %s: %w", in.cfg.PayloadDir, err)
	}
	for _, artifact := range requiredKataArtifacts {
		rel := strings.TrimPrefix(artifact, kataHostDir+"/")
		if _, err := os.Stat(filepath.Join(in.cfg.PayloadDir, rel)); err != nil {
			return false, fmt.Errorf("installer image payload is missing %s: %w", rel, err)
		}
	}

	changed := false
	hostVersion, err := os.ReadFile(in.hostPath(kataHostDir, "VERSION"))
	upToDate := err == nil && string(hostVersion) == string(payloadVersion)
	if upToDate {
		// Version matches — verify the artifacts are actually intact
		// before trusting it (a partially deleted /opt/kata with a
		// surviving VERSION file must reconverge).
		for _, artifact := range requiredKataArtifacts {
			if _, err := os.Stat(in.hostPath(artifact)); err != nil {
				upToDate = false
				break
			}
		}
	}
	if !upToDate {
		in.log("laying kata payload version %s onto host %s", strings.TrimSpace(string(payloadVersion)), kataHostDir)
		// Stage into a sibling directory and swap, so a crash mid-copy
		// never leaves a half-written /opt/kata that passes the VERSION
		// check on the next run (VERSION is copied last only by luck of
		// walk order; the swap removes the ordering dependency).
		staging := in.hostPath(kataHostDir) + ".setec-staging"
		if err := os.RemoveAll(staging); err != nil {
			return false, err
		}
		if err := copyTree(in.cfg.PayloadDir, staging); err != nil {
			return false, err
		}
		old := in.hostPath(kataHostDir) + ".setec-old"
		if err := os.RemoveAll(old); err != nil {
			return false, err
		}
		if _, err := os.Stat(in.hostPath(kataHostDir)); err == nil {
			if err := os.Rename(in.hostPath(kataHostDir), old); err != nil {
				return false, err
			}
		}
		if err := os.Rename(staging, in.hostPath(kataHostDir)); err != nil {
			return false, err
		}
		if err := os.RemoveAll(old); err != nil {
			return false, err
		}
		changed = true
	}

	// kata-deploy-parity shim + CLI symlinks.
	for link, target := range map[string]string{
		kataShimLink: kataShimBin,
		kataCLILink:  "/opt/kata/bin/kata-runtime",
	} {
		linkChanged, err := ensureSymlink(in.hostPath(link), target)
		if err != nil {
			return changed, err
		}
		changed = changed || linkChanged
	}

	// Final on-host sanity: everything the handler needs must exist now.
	for _, artifact := range requiredKataArtifacts {
		if _, err := os.Stat(in.hostPath(artifact)); err != nil {
			return changed, fmt.Errorf("kata artifact missing on host after install: %s: %w", artifact, err)
		}
	}
	return changed, nil
}
