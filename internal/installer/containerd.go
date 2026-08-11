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
	"context"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// runtimeFlavor describes how the node's container runtime consumes
// containerd configuration.
type runtimeFlavor struct {
	// name: "containerd" (stock) or "k3s".
	name string
	// unit is the systemd unit that owns containerd on this node.
	unit string
	// configPath is the rendered containerd config (host-absolute).
	configPath string
	// configDir is the directory holding configPath.
	configDir string
}

// Stock containerd paths (also used to build the k3s equivalents).
const (
	stockConfigDir  = "/etc/containerd"
	stockConfigPath = "/etc/containerd/config.toml"
	stockDropinDir  = "/etc/containerd/config.d"
	stockDropinPath = "/etc/containerd/config.d/99-setec-kata-fc.toml"

	k3sConfigDir  = "/var/lib/rancher/k3s/agent/etc/containerd"
	k3sConfigPath = "/var/lib/rancher/k3s/agent/etc/containerd/config.toml"

	backupSuffix = ".setec-orig"

	beginMarker = "# BEGIN setec-installer — managed block, do not edit (zeroroot-ai/setec)"
	endMarker   = "# END setec-installer"
)

// detectFlavor decides whether this node runs stock containerd or k3s's
// embedded containerd, and which systemd unit to restart.
func detectFlavor(cfg Config) (runtimeFlavor, error) {
	if _, err := os.Stat(cfg.HostRoot + k3sConfigDir); err == nil {
		unit := "k3s.service"
		// Agent-only nodes run k3s-agent.service instead.
		if !unitPresent(cfg.HostRoot, "k3s.service") && unitPresent(cfg.HostRoot, "k3s-agent.service") {
			unit = "k3s-agent.service"
		}
		return runtimeFlavor{
			name:       "k3s",
			unit:       unit,
			configPath: k3sConfigPath,
			configDir:  k3sConfigDir,
		}, nil
	}
	if _, err := os.Stat(cfg.HostRoot + stockConfigDir); err == nil {
		return runtimeFlavor{
			name:       "containerd",
			unit:       "containerd.service",
			configPath: stockConfigPath,
			configDir:  stockConfigDir,
		}, nil
	}
	// No /etc/containerd at all: still a valid stock-containerd node
	// (containerd runs on pure defaults) as long as the unit exists.
	if unitPresent(cfg.HostRoot, "containerd.service") {
		return runtimeFlavor{
			name:       "containerd",
			unit:       "containerd.service",
			configPath: stockConfigPath,
			configDir:  stockConfigDir,
		}, nil
	}
	return runtimeFlavor{}, fmt.Errorf(
		"no supported container runtime found: neither %s (k3s) nor %s / containerd.service (stock containerd) exists on the host",
		k3sConfigDir, stockConfigDir)
}

// unitPresent reports whether a systemd unit file exists in the standard
// locations. A filesystem check (not systemctl) keeps flavor detection
// side-effect free and trivially testable.
func unitPresent(hostRoot, unit string) bool {
	for _, dir := range []string{
		"/etc/systemd/system/",
		"/usr/lib/systemd/system/",
		"/lib/systemd/system/",
	} {
		if _, err := os.Stat(hostRoot + dir + unit); err == nil {
			return true
		}
	}
	return false
}

// kata-fc ownership states.
type ownership int

const (
	ownerNone    ownership = iota // kata-fc not registered anywhere
	ownerSelf                     // registered by this installer
	ownerForeign                  // registered by something else (kata-deploy, baked image, admin)
)

// kataFCRuntimeTableRe matches a kata-fc runtime registration in any
// containerd config schema (v2 grpc.v1.cri or v3 cri.v1.runtime table
// names, quoted or bare key).
var kataFCRuntimeTableRe = regexp.MustCompile(`containerd\.runtimes\.("kata-fc"|kata-fc)\b`)

// kataFCOwnership decides who owns the node's kata-fc registration.
//
//   - our drop-in / managed template block present -> ownerSelf
//   - otherwise kata-fc registered in the effective config -> ownerForeign
//   - otherwise -> ownerNone
func kataFCOwnership(cfg Config, flavor runtimeFlavor) (ownership, error) {
	switch flavor.name {
	case "k3s":
		for _, tmpl := range k3sTemplateCandidates() {
			content, err := os.ReadFile(cfg.HostRoot + tmpl)
			if err != nil {
				continue
			}
			if strings.Contains(string(content), beginMarker) {
				return ownerSelf, nil
			}
		}
	default:
		if _, err := os.Stat(cfg.HostRoot + stockDropinPath); err == nil {
			return ownerSelf, nil
		}
	}
	// Not ours — is it anyone's? Check the rendered/main config plus any
	// sibling files in the config dir (imports, drop-ins, templates).
	dirs := []string{flavor.configDir, stockDropinDir}
	if flavor.name == "k3s" {
		dirs = []string{flavor.configDir, flavor.configDir + "/config.toml.d"}
	}
	for _, dir := range dirs {
		entries, err := os.ReadDir(cfg.HostRoot + dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			name := e.Name()
			if !strings.HasSuffix(name, ".toml") && !strings.HasSuffix(name, ".tmpl") {
				continue
			}
			content, err := os.ReadFile(cfg.HostRoot + dir + "/" + name)
			if err != nil {
				continue
			}
			if kataFCRuntimeTableRe.Match(content) {
				return ownerForeign, nil
			}
		}
	}
	return ownerNone, nil
}

// k3sTemplateCandidates lists the template filenames k3s may render the
// containerd config from, newest scheme first.
func k3sTemplateCandidates() []string {
	return []string{
		k3sConfigDir + "/config-v3.toml.tmpl",
		k3sConfigDir + "/config.toml.tmpl",
	}
}

// detectConfigVersion determines the containerd config schema version
// (2 for containerd 1.x, 3 for containerd 2.x). It prefers asking the
// host's containerd binary for its default config; when that fails (no
// `containerd` on PATH — k3s embeds it) it falls back to the version
// line of the rendered config, then to 2.
var versionLineRe = regexp.MustCompile(`(?m)^\s*version\s*=\s*(\d+)`)

func (in *Installer) detectConfigVersion(ctx context.Context, flavor runtimeFlavor) int {
	if flavor.name == "containerd" {
		if out, err := in.cfg.Runner.Run(ctx, "containerd", "config", "default"); err == nil {
			if m := versionLineRe.FindSubmatch(out); m != nil {
				if v, err := strconv.Atoi(string(m[1])); err == nil {
					return v
				}
			}
		}
	}
	if content, err := os.ReadFile(in.hostPath(flavor.configPath)); err == nil {
		if m := versionLineRe.FindSubmatch(content); m != nil {
			if v, err := strconv.Atoi(string(m[1])); err == nil {
				return v
			}
		}
	}
	return 2
}

// runtimeTableName returns the CRI runtime table prefix for the config
// schema version.
func runtimeTableName(version int) string {
	if version >= 3 {
		return `plugins."io.containerd.cri.v1.runtime".containerd.runtimes.kata-fc`
	}
	return `plugins."io.containerd.grpc.v1.cri".containerd.runtimes.kata-fc`
}

// registrationTOML renders the devmapper snapshotter + kata-fc runtime
// registration for the given schema version. This is the packer AMI's
// drop-in, kept in one place for both flavors.
func (in *Installer) registrationTOML(version int) string {
	table := runtimeTableName(version)
	var b strings.Builder
	fmt.Fprintf(&b, `# Firecracker needs a block device per container rootfs (no overlayfs);
# the devmapper snapshotter carves thin volumes out of the pool that
# setec-thinpool.service builds at boot.
[plugins."io.containerd.snapshotter.v1.devmapper"]
  root_path = "%s"
  pool_name = "%s"
  base_image_size = "%s"
  discard_blocks = true

[%s]
  runtime_type = "io.containerd.kata-fc.v2"
  privileged_without_host_devices = true
  pod_annotations = ["io.katacontainers.*"]
  snapshotter = "devmapper"
  [%s.options]
    ConfigPath = "%s"
`, in.cfg.DevmapperRoot, in.cfg.PoolName, in.cfg.BaseImageSize, table, table, kataFCConf)
	return b.String()
}

// ensureContainerdConfig registers kata-fc + devmapper with the node's
// containerd, flavor-appropriately. Returns whether the effective config
// changed (i.e. whether the runtime needs a restart).
func (in *Installer) ensureContainerdConfig(ctx context.Context, flavor runtimeFlavor) (bool, error) {
	version := in.detectConfigVersion(ctx, flavor)
	switch flavor.name {
	case "k3s":
		return in.ensureK3sTemplate(version)
	default:
		return in.ensureStockDropin(version)
	}
}

// --- stock containerd -------------------------------------------------

// ensureStockDropin writes the registration as a drop-in under
// /etc/containerd/config.d and makes sure the main config imports that
// directory. The drop-in mechanism is containerd's supported extension
// point; the only mutation of the admin's own config.toml is the
// one-line imports entry, and the original is backed up first.
func (in *Installer) ensureStockDropin(version int) (bool, error) {
	changed := false

	dropin := fmt.Sprintf("# Managed by the setec installer DaemonSet (zeroroot-ai/setec) — DO NOT EDIT.\nversion = %d\n\n%s", version, in.registrationTOML(version))
	c, err := writeFileIfChanged(in.hostPath(stockDropinPath), []byte(dropin), 0o644)
	if err != nil {
		return changed, err
	}
	changed = changed || c

	c, err = in.ensureImportsLine(version)
	if err != nil {
		return changed, err
	}
	changed = changed || c
	return changed, nil
}

// importsLineRe matches a single-line top-level imports assignment.
var importsLineRe = regexp.MustCompile(`(?m)^\s*imports\s*=\s*\[(.*)\]\s*$`)

const importsGlob = "/etc/containerd/config.d/*.toml"

// ensureImportsLine makes the stock config.toml import the drop-in
// directory. Cases:
//
//   - no config.toml: create a minimal one (version + imports) — every
//     other setting stays containerd's compiled-in default.
//   - imports line already contains the glob: no-op.
//   - single-line imports array: rewrite that line with the glob added.
//   - multi-line imports array: refuse — corrupting the admin's config
//     is the one failure mode this installer must never have.
func (in *Installer) ensureImportsLine(version int) (bool, error) {
	path := in.hostPath(stockConfigPath)
	content, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		minimal := fmt.Sprintf(`# Managed by the setec installer DaemonSet (zeroroot-ai/setec).
# Created because this node had no containerd config file; every setting
# other than the imports below stays containerd's compiled-in default.
version = %d
imports = [%q]
`, version, importsGlob)
		return writeFileIfChanged(path, []byte(minimal), 0o644)
	}
	if err != nil {
		return false, err
	}
	text := string(content)
	if strings.Contains(text, importsGlob) {
		return false, nil
	}

	if m := importsLineRe.FindStringSubmatchIndex(text); m != nil {
		if err := in.backupOnce(stockConfigPath, content); err != nil {
			return false, err
		}
		inner := strings.TrimSpace(text[m[2]:m[3]])
		var rebuilt string
		if inner == "" {
			rebuilt = fmt.Sprintf("imports = [%q]", importsGlob)
		} else {
			rebuilt = fmt.Sprintf("imports = [%s, %q]", inner, importsGlob)
		}
		text = text[:m[0]] + rebuilt + text[m[1]:]
		return writeFileIfChanged(path, []byte(text), 0o644)
	}

	// No single-line imports. If a multi-line imports array exists,
	// refuse rather than guess at TOML structure.
	if regexp.MustCompile(`(?m)^\s*imports\s*=`).MatchString(text) {
		return false, fmt.Errorf(
			"%s has a multi-line imports array the installer cannot safely edit; add %q to it manually",
			stockConfigPath, importsGlob)
	}

	if err := in.backupOnce(stockConfigPath, content); err != nil {
		return false, err
	}
	// Insert after the version line when present (imports is a top-level
	// key and must precede any [table]); otherwise prepend.
	line := fmt.Sprintf("imports = [%q]\n", importsGlob)
	if m := versionLineRe.FindStringIndex(text); m != nil {
		insertAt := strings.Index(text[m[1]:], "\n")
		if insertAt < 0 {
			text += "\n" + line
		} else {
			pos := m[1] + insertAt + 1
			text = text[:pos] + line + text[pos:]
		}
	} else {
		text = line + text
	}
	return writeFileIfChanged(path, []byte(text), 0o644)
}

// --- k3s --------------------------------------------------------------

// ensureK3sTemplate manages the k3s containerd config template. k3s does
// not read /etc/containerd and regenerates its config.toml at every
// start, so the supported customization point is the config template:
// config-v3.toml.tmpl for containerd v2 (config schema 3),
// config.toml.tmpl for older. A template may reference k3s's built-in
// base template — `{{ template "base" . }}` — which renders exactly what
// k3s would have generated, so our template is base + a marker-delimited
// registration block and survives k3s upgrades without freezing the
// dynamic config.
//
// When an existing template is found (admin- or third-party-owned but
// without a kata-fc registration — that case was already classified as
// ownerForeign upstream), the managed block is appended/refreshed
// between markers and everything outside the markers is preserved
// byte-for-byte.
func (in *Installer) ensureK3sTemplate(version int) (bool, error) {
	tmplPath := k3sConfigDir + "/config.toml.tmpl"
	if version >= 3 {
		tmplPath = k3sConfigDir + "/config-v3.toml.tmpl"
	}

	block := beginMarker + "\n" + in.registrationTOML(version) + endMarker + "\n"

	existing, err := os.ReadFile(in.hostPath(tmplPath))
	switch {
	case os.IsNotExist(err):
		content := `# Managed by the setec installer DaemonSet (zeroroot-ai/setec).
# Renders k3s's built-in default containerd config, then registers the
# kata-fc runtime + devmapper snapshotter. Content outside the setec
# markers is yours; the installer preserves it.
{{ template "base" . }}

` + block
		return writeFileIfChanged(in.hostPath(tmplPath), []byte(content), 0o644)
	case err != nil:
		return false, err
	}

	text := string(existing)
	begin := strings.Index(text, beginMarker)
	end := strings.Index(text, endMarker)
	switch {
	case begin >= 0 && end > begin:
		current := text[begin : end+len(endMarker)+1]
		if current == block || current == strings.TrimSuffix(block, "\n") {
			return false, nil
		}
		if err := in.backupOnce(tmplPath, existing); err != nil {
			return false, err
		}
		text = text[:begin] + block + text[end+len(endMarker):]
		text = strings.TrimSuffix(text, "\n") + "\n"
		return writeFileIfChanged(in.hostPath(tmplPath), []byte(text), 0o644)
	case begin >= 0 || end >= 0:
		return false, fmt.Errorf("%s contains a damaged setec marker pair; repair or remove the markers manually", tmplPath)
	default:
		if err := in.backupOnce(tmplPath, existing); err != nil {
			return false, err
		}
		text = strings.TrimSuffix(text, "\n") + "\n\n" + block
		return writeFileIfChanged(in.hostPath(tmplPath), []byte(text), 0o644)
	}
}

// backupOnce snapshots a file we are about to mutate, once — the first
// backup is the admin's pristine original and later runs must not
// overwrite it with our own edits.
func (in *Installer) backupOnce(hostAbsPath string, current []byte) error {
	backup := in.hostPath(hostAbsPath + backupSuffix)
	if _, err := os.Stat(backup); err == nil {
		return nil
	}
	_, err := writeFileIfChanged(backup, current, 0o644)
	return err
}

// restartRuntime restarts the container runtime unit and waits for it to
// report active. On timeout the error tells the operator exactly which
// unit to inspect; the managed drop-in / marker block can be removed and
// the .setec-orig backup restored to roll back by hand.
func (in *Installer) restartRuntime(ctx context.Context, flavor runtimeFlavor) error {
	in.log("restarting %s to pick up the kata-fc registration", flavor.unit)
	if _, err := in.cfg.Runner.Run(ctx, "systemctl", "restart", flavor.unit); err != nil {
		return fmt.Errorf("restarting %s: %w", flavor.unit, err)
	}
	deadline := time.Now().Add(in.cfg.RestartTimeout)
	for {
		out, err := in.cfg.Runner.Run(ctx, "systemctl", "is-active", flavor.unit)
		if err == nil && strings.TrimSpace(string(out)) == "active" {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf(
				"%s did not report active within %s after restart; inspect `journalctl -u %s` on the node (the pristine config was backed up with the %s suffix)",
				flavor.unit, in.cfg.RestartTimeout, flavor.unit, backupSuffix)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// verify asserts the converged state: unit active, pool active, shim on
// PATH. It performs no writes.
func (in *Installer) verify(ctx context.Context, flavor runtimeFlavor) error {
	out, err := in.cfg.Runner.Run(ctx, "systemctl", "is-active", flavor.unit)
	if err != nil || strings.TrimSpace(string(out)) != "active" {
		return fmt.Errorf("%s is not active", flavor.unit)
	}
	if _, err := in.cfg.Runner.Run(ctx, "dmsetup", "info", in.cfg.PoolName); err != nil {
		return fmt.Errorf("thin-pool %s not active: %w", in.cfg.PoolName, err)
	}
	if _, err := os.Stat(in.hostPath(kataShimLink)); err != nil {
		return fmt.Errorf("kata-fc shim missing at %s: %w", kataShimLink, err)
	}
	return nil
}
