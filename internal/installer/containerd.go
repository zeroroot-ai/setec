// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package installer

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/zeroroot-ai/setec/internal/runtimeagent/probe"
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

// Runtime flavour names. Defined here rather than in the tests that first
// needed them: the production switches below are what repeat the literals.
const (
	flavorContainerd = "containerd"
	flavorK3s        = "k3s"
)

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
			name:       flavorK3s,
			unit:       unit,
			configPath: k3sConfigPath,
			configDir:  k3sConfigDir,
		}, nil
	}
	if _, err := os.Stat(cfg.HostRoot + stockConfigDir); err == nil {
		return runtimeFlavor{
			name:       flavorContainerd,
			unit:       "containerd.service",
			configPath: stockConfigPath,
			configDir:  stockConfigDir,
		}, nil
	}
	// No /etc/containerd at all: still a valid stock-containerd node
	// (containerd runs on pure defaults) as long as the unit exists.
	if unitPresent(cfg.HostRoot, "containerd.service") {
		return runtimeFlavor{
			name:       flavorContainerd,
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

// kataFCRuntimeTableRe matches a kata-fc runtime registration in any
// containerd config schema (v2 grpc.v1.cri or v3 cri.v1.runtime table
// names, quoted or bare key).
var kataFCRuntimeTableRe = regexp.MustCompile(`containerd\.runtimes\.("kata-fc"|kata-fc)\b`)

// devmapperTableRe matches the devmapper snapshotter plugin table.
var devmapperTableRe = regexp.MustCompile(`plugins\.["']io\.containerd\.snapshotter\.v1\.devmapper["']`)

// devmapperSnapshotter is the snapshotter name the kata-fc handler asks
// containerd for. Firecracker needs a block device per container rootfs,
// so kata-deploy's default for the fc shim is this, with the note
// "requires pre-configuration on the user side".
const devmapperSnapshotter = "devmapper"

// convergeMode selects how much of the node this installer owns.
type convergeMode int

const (
	// modeFull: the installer owns the whole kata-fc registration: kata
	// payload, thin-pool, devmapper snapshotter and the runtime handler.
	modeFull convergeMode = iota
	// modeDevmapper: another owner (kata-deploy, a baked image, an
	// administrator) registered the kata-fc handler and pointed it at the
	// devmapper snapshotter, and nothing configured that snapshotter. The
	// installer supplies the thin-pool and the snapshotter table only, and
	// never touches the handler or the kata payload (setec#9).
	modeDevmapper
)

// ownership is what the node's effective containerd configuration says
// about who registered what. Both halves are read through the same
// import-following scan the runtime-agent uses (setec#281), so a drop-in
// in a directory nobody enumerated (kata-deploy 3.28 writes
// /opt/kata/containerd/config.d/kata-deploy.toml) is still seen.
type ownership struct {
	// handlerSelf: the kata-fc handler is in this installer's managed
	// drop-in or template block.
	handlerSelf bool
	// handlerForeign: the kata-fc handler is registered, and not by us.
	handlerForeign bool
	// handlerWantsDevmapper: the kata-fc handler's own table names the
	// devmapper snapshotter.
	handlerWantsDevmapper bool
	// devmapperSelf: the devmapper snapshotter table is in our managed
	// content.
	devmapperSelf bool
	// devmapperForeign: the devmapper snapshotter table exists, and not in
	// our managed content.
	devmapperForeign bool
}

// nodeOwnership reads the node's containerd configuration and reports
// what this installer owns and what somebody else does.
func nodeOwnership(cfg Config, flavor runtimeFlavor) ownership {
	managed := managedContent(cfg, flavor)
	scan := probe.ScanContainerdConfig(cfg.HostRoot)

	var own ownership
	own.handlerSelf = kataFCRuntimeTableRe.MatchString(managed)
	if _, ok := scan.Handlers["kata-fc"]; ok && !own.handlerSelf {
		own.handlerForeign = true
	}
	own.handlerWantsDevmapper = scan.HandlerSnapshotters["kata-fc"] == devmapperSnapshotter
	own.devmapperSelf = devmapperTableRe.MatchString(managed)
	if _, ok := scan.Snapshotters[devmapperSnapshotter]; ok && !own.devmapperSelf {
		own.devmapperForeign = true
	}
	return own
}

// managedContent returns the containerd configuration this installer
// wrote: the stock drop-in, or the marker-delimited block of the k3s
// template. Empty when nothing of ours is on the node.
func managedContent(cfg Config, flavor runtimeFlavor) string {
	if flavor.name == flavorK3s {
		for _, tmpl := range k3sTemplateCandidates() {
			content, err := os.ReadFile(cfg.HostRoot + tmpl)
			if err != nil {
				continue
			}
			text := string(content)
			begin := strings.Index(text, beginMarker)
			end := strings.Index(text, endMarker)
			if begin >= 0 && end > begin {
				return text[begin:end]
			}
		}
		return ""
	}
	content, err := os.ReadFile(cfg.HostRoot + stockDropinPath)
	if err != nil {
		return ""
	}
	return string(content)
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
// the installer writes (2 for the containerd 1.x schema, 3 and later for
// containerd 2.x).
//
// The version line of the node's own config wins. containerd refuses to
// start when a drop-in declares a higher version than the root config
// ("drop-in config version 4 higher than root config version 2"), and a
// containerd 2.x binary still runs a version-2 root config by migrating it.
// So on a node that upgraded containerd and kept its config (the kind node
// image is one), the binary's default schema is the wrong answer: the
// installer took that node's containerd down for good (setec#22).
//
// Only when the config has no version line, or does not exist (the
// installer then creates it), does the host's containerd binary decide,
// through its default config. k3s embeds containerd and has no binary on
// PATH, so its flavor reads the rendered config alone. The last fallback
// is 2.
var versionLineRe = regexp.MustCompile(`(?m)^\s*version\s*=\s*(\d+)`)

func (in *Installer) detectConfigVersion(ctx context.Context, flavor runtimeFlavor) int {
	if content, err := os.ReadFile(in.hostPath(flavor.configPath)); err == nil {
		if m := versionLineRe.FindSubmatch(content); m != nil {
			if v, err := strconv.Atoi(string(m[1])); err == nil {
				return v
			}
		}
	}
	if flavor.name == "containerd" {
		if out, err := in.cfg.Runner.Run(ctx, "containerd", "config", "default"); err == nil {
			if m := versionLineRe.FindSubmatch(out); m != nil {
				if v, err := strconv.Atoi(string(m[1])); err == nil {
					return v
				}
			}
		}
	}
	return 2
}

// runtimeTableName returns the CRI runtime table prefix for the config
// schema version.
// gvisorRuntimeTableName returns the CRI runtime table prefix for the runsc
// handler, on the same schema split as kata-fc.
//
// The 2.x path is not cosmetic. containerd 2.x SILENTLY IGNORES a runtime
// registered under the 1.x `io.containerd.grpc.v1.cri` table: the stanza is
// present, containerd starts clean, and kubelet then fails the pod with
// `no runtime for "runsc" is configured`. Proven on kind-vanilla running
// containerd v2.1.1.
func gvisorRuntimeTableName(version int) string {
	if version >= 3 {
		return `plugins."io.containerd.cri.v1.runtime".containerd.runtimes.runsc`
	}
	return `plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runsc`
}

func runtimeTableName(version int) string {
	if version >= 3 {
		return `plugins."io.containerd.cri.v1.runtime".containerd.runtimes.kata-fc`
	}
	return `plugins."io.containerd.grpc.v1.cri".containerd.runtimes.kata-fc`
}

// keepUnpackedLayersTOML turns discard_unpacked_layers off. The kata-fc
// handler unpacks into the devmapper snapshotter, every other handler into
// the node's default one, and an image already unpacked for one of them has
// to be unpacked again for the other. With discard_unpacked_layers = true
// (the kind node image sets it) the second unpack finds no layer to read and
// fails with "failed to get reader from content store ... not found"
// (setec#22).
//
// Stock containerd only. The drop-in is a file of its own, and containerd
// merges it over the root config. k3s renders one config from a template
// whose base already declares the images table, and a second declaration of
// a table in one TOML file does not parse.
func keepUnpackedLayersTOML(version int) string {
	return fmt.Sprintf(`
# Keep compressed layers after unpacking, so an image unpacked for one
# snapshotter can be unpacked again for the other.
[%s]
  discard_unpacked_layers = false
`, imagesTableName(version))
}

// imagesTableName returns the CRI table that holds discard_unpacked_layers
// for the config schema version.
func imagesTableName(version int) string {
	if version >= 3 {
		return `plugins."io.containerd.cri.v1.images"`
	}
	return `plugins."io.containerd.grpc.v1.cri".containerd`
}

// registrationTOML renders what this installer registers with containerd
// for the given schema version: the devmapper snapshotter always, and the
// kata-fc runtime handler in modeFull. It is the packer AMI's drop-in,
// kept in one place for both flavors.
func (in *Installer) registrationTOML(version int, mode convergeMode) string {
	var b strings.Builder
	fmt.Fprintf(&b, `# Firecracker needs a block device per container rootfs (no overlayfs);
# the devmapper snapshotter carves thin volumes out of the pool that
# setec-thinpool.service builds at boot.
[plugins."io.containerd.snapshotter.v1.devmapper"]
  root_path = "%s"
  pool_name = "%s"
  base_image_size = "%s"
  discard_blocks = true
`, in.cfg.DevmapperRoot, in.cfg.PoolName, in.cfg.BaseImageSize)
	if mode == modeDevmapper {
		b.WriteString(`
# The kata-fc runtime handler on this node is registered by another owner
# (kata-deploy, a baked image, an administrator) and asks for the devmapper
# snapshotter above. This installer supplies the snapshotter only.
`)
		return b.String()
	}
	table := runtimeTableName(version)
	fmt.Fprintf(&b, `
[%s]
  runtime_type = "io.containerd.kata-fc.v2"
  privileged_without_host_devices = true
  pod_annotations = ["io.katacontainers.*"]
  snapshotter = "devmapper"
  [%s.options]
    ConfigPath = "%s"
`, table, table, kataFCConf)

	// gvisor. No snapshotter override: runsc runs on the node's default
	// snapshotter (overlayfs on a stock node), unlike Firecracker which needs a
	// block device per container rootfs. No pod_annotations either, because
	// runsc consumes none.
	gvisorTable := gvisorRuntimeTableName(version)
	fmt.Fprintf(&b, `
[%s]
  runtime_type = "io.containerd.runsc.v1"
`, gvisorTable)
	return b.String()
}

// ensureContainerdConfig registers kata-fc + devmapper with the node's
// containerd, flavor-appropriately. Returns whether the effective config
// changed (i.e. whether the runtime needs a restart).
func (in *Installer) ensureContainerdConfig(ctx context.Context, flavor runtimeFlavor, mode convergeMode) (bool, error) {
	version := in.detectConfigVersion(ctx, flavor)
	switch flavor.name {
	case flavorK3s:
		return in.ensureK3sTemplate(version, mode)
	default:
		return in.ensureStockDropin(version, mode)
	}
}

// --- stock containerd -------------------------------------------------

// ensureStockDropin writes the registration as a drop-in under
// /etc/containerd/config.d and makes sure the main config imports that
// directory. The drop-in mechanism is containerd's supported extension
// point; the only mutation of the admin's own config.toml is the
// one-line imports entry, and the original is backed up first.
func (in *Installer) ensureStockDropin(version int, mode convergeMode) (bool, error) {
	changed := false

	dropin := fmt.Sprintf("# Managed by the setec installer DaemonSet (zeroroot-ai/setec) — DO NOT EDIT.\nversion = %d\n\n%s%s",
		version, in.registrationTOML(version, mode), keepUnpackedLayersTOML(version))
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
// When an existing template is found (admin- or third-party-owned; a
// kata-fc registration of its own puts the run in modeDevmapper upstream),
// the managed block is appended/refreshed between markers and everything
// outside the markers is preserved byte-for-byte.
func (in *Installer) ensureK3sTemplate(version int, mode convergeMode) (bool, error) {
	tmplPath := k3sConfigDir + "/config.toml.tmpl"
	if version >= 3 {
		tmplPath = k3sConfigDir + "/config-v3.toml.tmpl"
	}

	block := beginMarker + "\n" + in.registrationTOML(version, mode) + endMarker + "\n"

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

// verify asserts the converged state: unit active, pool active, and in
// modeFull the shim on PATH. It performs no writes.
func (in *Installer) verify(ctx context.Context, flavor runtimeFlavor, mode convergeMode) error {
	out, err := in.cfg.Runner.Run(ctx, "systemctl", "is-active", flavor.unit)
	if err != nil || strings.TrimSpace(string(out)) != "active" {
		return fmt.Errorf("%s is not active", flavor.unit)
	}
	if _, err := in.cfg.Runner.Run(ctx, "dmsetup", "info", in.cfg.PoolName); err != nil {
		return fmt.Errorf("thin-pool %s not active: %w", in.cfg.PoolName, err)
	}
	if err := in.verifyDevmapperPlugin(ctx, flavor); err != nil {
		return err
	}
	if mode == modeDevmapper {
		// The shim belongs to the handler's owner.
		return nil
	}
	// Lstat the link, then Stat its target THROUGH hostPath (setec#220).
	//
	// kataShimLink is a symlink whose target is a host-absolute path
	// (kataShimBin, /opt/kata/bin/...). os.Stat follows it, and an absolute
	// target resolves against the real filesystem — escaping the configured
	// HostRoot that every other path here goes through. On a real node
	// HostRoot is "/" and the two agree, so the bug is invisible in
	// production; under a fake root it asks whether THIS MACHINE has kata
	// installed, which is a question about the test runner rather than about
	// the tree the installer just wrote. That is why the installer tests
	// passed on a workstation with /opt/kata left over and failed on every
	// clean CI runner.
	if _, err := os.Lstat(in.hostPath(kataShimLink)); err != nil {
		return fmt.Errorf("kata-fc shim link missing at %s: %w", kataShimLink, err)
	}
	if _, err := os.Stat(in.hostPath(kataShimBin)); err != nil {
		return fmt.Errorf("kata-fc shim target missing at %s: %w", kataShimBin, err)
	}
	return nil
}

// devmapperPluginType and devmapperPluginID name the containerd snapshotter
// the kata-fc handler asks for, as `ctr plugins ls` prints them.
const (
	devmapperPluginType = "io.containerd.snapshotter.v1"
	devmapperPluginID   = "devmapper"
)

// verifyDevmapperPlugin asks the running containerd whether it loaded the
// devmapper snapshotter. A pool and a drop-in on disk prove nothing when the
// binary was built without the plugin: the kind node image ships such a
// build, so the installer reported "converged" while every kata-fc Pod died
// with "inspection service could not find snapshotter devmapper plugin"
// (setec#22). A plugin that is present but failed to initialise is the same
// failure, one step later.
//
// When the host has no ctr to ask (k3s without its multicall binary, a
// minimal image), the check is skipped out loud rather than guessed.
func (in *Installer) verifyDevmapperPlugin(ctx context.Context, flavor runtimeFlavor) error {
	var name string
	var args []string
	switch {
	case flavor.name == flavorK3s && hostHas(in.cfg.HostRoot, "k3s"):
		name, args = "k3s", []string{"ctr", "plugins", "ls"}
	case hostHas(in.cfg.HostRoot, "ctr"):
		name, args = "ctr", []string{"plugins", "ls"}
	default:
		in.log("no ctr on the host, so the installer cannot confirm that %s loaded the devmapper snapshotter", flavor.name)
		return nil
	}
	out, err := in.cfg.Runner.Run(ctx, name, args...)
	if err != nil {
		return fmt.Errorf("listing %s plugins to confirm the devmapper snapshotter: %w", flavor.name, err)
	}
	status, found := devmapperPluginStatus(out)
	if !found {
		return fmt.Errorf(
			"%s on this node has no devmapper snapshotter plugin: the binary was built without it "+
				"(the kind node image is one), so no kata-fc Pod can unpack its image here. "+
				"Install a containerd build that includes devmapper", flavor.name)
	}
	if status != "ok" {
		return fmt.Errorf("%s did not load the devmapper snapshotter (plugin status %q), inspect `journalctl -u %s` on the node",
			flavor.name, status, flavor.unit)
	}
	return nil
}

// devmapperPluginStatus finds the devmapper snapshotter row in `ctr plugins
// ls` output (TYPE ID PLATFORMS STATUS) and returns its status.
func devmapperPluginStatus(out []byte) (string, bool) {
	for line := range strings.SplitSeq(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) >= 3 && f[0] == devmapperPluginType && f[1] == devmapperPluginID {
			return f[len(f)-1], true
		}
	}
	return "", false
}

// hostHas reports whether name resolves on the host's PATH.
func hostHas(root, name string) bool {
	_, err := lookPathIn(root, name)
	return err == nil
}

// ensureConfigDirTraversable makes the containerd config directory readable and
// traversable, so a non-root reader can stat the files inside it.
//
// This exists because of a failure that looked like a probe bug and was a
// permissions bug. The installer created /etc/containerd at mode 0644 — readable
// but with no execute bit, so a non-root process could not traverse into it. The
// runtime agent then reported `setec.zeroroot.ai/runtime.gvisor=false` with
// reason "no containerd configuration is readable on this node", on a node whose
// containerd was correctly configured. Nothing failed loudly; the node simply
// advertised itself as incapable and no Sandbox scheduled (setec#89).
//
// 0755 and not 0775: the directory holds the node's runtime configuration, so
// group-write is more than any reader needs.
func (in *Installer) ensureConfigDirTraversable(flavor runtimeFlavor) (bool, error) {
	dir := in.hostPath(flavor.configDir)
	info, err := os.Stat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			// Nothing to fix: a flavor whose config dir does not exist has no
			// config for the agent to read either, and the registration step
			// creates it with the right mode.
			return false, nil
		}
		return false, err
	}
	if info.Mode().Perm() == 0o755 {
		return false, nil
	}
	in.log("making %s traversable (was %#o) so the non-root runtime agent can read the containerd config", flavor.configDir, info.Mode().Perm())
	if err := os.Chmod(dir, 0o755); err != nil {
		return false, err
	}
	return true, nil
}
