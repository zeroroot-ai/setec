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

package probe

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Handler-check outcomes, published as Details["containerd_handler"] so the
// node's runtime-probe annotation says which of the three cases produced the
// label value.
const (
	handlerConfigured   = "configured"
	handlerAbsent       = "absent"
	handlerUnverifiable = "unverifiable"
)

// containerdConfigSeedDirs lists the directories, relative to Config.FSRoot,
// where the scan STARTS. Every *.toml and *.tmpl file directly inside them is
// read. It is a seed list, not the whole search space: whatever those files
// name in their `imports` array is followed from there (see followImports).
//
// Both containerd flavours the installer supports are covered (see
// internal/installer/containerd.go, which writes into exactly these places):
//
//   - stock containerd: /etc/containerd/config.toml plus the sibling drop-in
//     dirs distributions conventionally use (config.d is what the Setec
//     installer writes; conf.d and config.toml.d appear elsewhere).
//   - k3s: /var/lib/rancher/k3s/agent/etc/containerd/{config.toml,
//     config*.toml.tmpl, config.toml.d/*.toml}. k3s regenerates config.toml
//     from the template, so the template is scanned too — it is the file that
//     survives a k3s restart.
//
// A DROP-IN DIRECTORY IS NOT A CONVENTION, IT IS A POINTER (setec#281). This
// list used to be the entire search space, and it was wrong the moment an
// installer chose a path nobody had enumerated: kata-deploy 3.28 writes
// /opt/kata/containerd/config.d/kata-deploy.toml and registers it by appending
// to `imports` in /etc/containerd/config.toml. containerd resolves that fine;
// a fixed allowlist does not, so every kata-deploy node reported "containerd
// has no runtime handler kata-fc configured" while running kata workloads
// perfectly. Follow `imports` — containerd's own answer to "what else is my
// config" — and the allowlist stops being load-bearing.
//
// The agent only sees a directory when the DaemonSet mounts it read-only
// (charts/setec/templates/runtime-agent-daemonset.yaml). An unmounted seed
// directory is indistinguishable from an absent one and is simply skipped. An
// unmounted IMPORT is different: the config named it, so the agent knows it is
// missing something, and containerdScan reports it as Unreadable rather than
// concluding the handler is absent.
var containerdConfigSeedDirs = []string{
	"etc/containerd",
	"etc/containerd/config.d",
	"etc/containerd/conf.d",
	"etc/containerd/config.toml.d",
	"var/lib/rancher/k3s/agent/etc/containerd",
	"var/lib/rancher/k3s/agent/etc/containerd/config.toml.d",
}

// scanLimits bound the import walk. containerd imposes no depth limit and an
// imported file may import further files, so the walk needs its own stop:
// a cycle (a imports b imports a) is defeated by the visited set, and a
// pathological fan-out by these counts. Both are far above any real config —
// a stock node reads one file, a kata-deploy node two.
const (
	maxImportDepth = 8
	maxScanFiles   = 256
)

// importsArrayRe captures the body of a top-level `imports = [ ... ]` array.
//
// containerd's config puts `imports` at the document root, before any table
// header. A regexp rather than a TOML parse for the same reason the handler
// scan uses one: a partial read cannot be defeated by TOML this probe does not
// understand, and the array's contents are plain quoted strings in every
// version containerd has shipped.
//
// Anchored at a line start with no leading '[' so a key named `imports` inside
// some other table cannot be picked up, and `(?s)` so a multi-line array —
// which is how kata-deploy writes it — is captured whole.
var importsArrayRe = regexp.MustCompile(`(?ms)^\s*imports\s*=\s*\[(.*?)\]`)

// quotedStringRe pulls the individual entries out of an imports array body.
var quotedStringRe = regexp.MustCompile(`"([^"]*)"|'([^']*)'`)

// runtimeHandlerTableRe extracts the handler name from a containerd CRI
// runtime table header. It matches every schema containerd has shipped,
// because only the plugin name differs between them and the plugin name is
// not what is captured:
//
//	[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.kata-qemu]     (v2)
//	[plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes."kata-fc"]  (v3)
//	[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runc.options]  (sub-table)
//
// Anchoring at a line-leading '[' is what keeps prose and comments out: a
// TOML comment starts with '#', so a sentence mentioning containerd.runtimes
// cannot be mistaken for a registration. The bare-key alternative excludes
// '.' so a sub-table header yields the handler ("runc"), not "runc.options".
var runtimeHandlerTableRe = regexp.MustCompile(
	`(?m)^\s*\[+[^\]\n]*?containerd\.runtimes\.(?:"([^"]+)"|'([^']+)'|([A-Za-z0-9_-]+))`)

// containerdScan is the result of reading a node's containerd configuration.
type containerdScan struct {
	// Handlers is the set of CRI runtime handler names containerd is
	// configured to accept.
	Handlers map[string]struct{}

	// Scanned lists the config files actually read, in the order read. An
	// empty Scanned means no containerd configuration was readable at all —
	// the caller must treat that as "cannot verify", which is a different
	// answer from "verified absent".
	Scanned []string

	// Unreadable lists host paths a config file named in its `imports` array
	// that this agent could not read, expressed as the HOST would see them
	// (not re-rooted under FSRoot), because that is what an operator has to
	// mount. A non-empty Unreadable means the scan is INCOMPLETE: a handler
	// missing from Handlers may well be registered in a file behind one of
	// these paths, so "absent" is not a conclusion that can be drawn.
	Unreadable []string
}

// ScanContainerdConfig reads the node's containerd configuration under root.
//
// It starts at containerdConfigSeedDirs and then follows every `imports` entry
// it finds, which is how it stays correct for installers that put their drop-in
// somewhere this package has never heard of (setec#281).
//
// The scan is a read-only regexp over the table headers rather than a full TOML
// parse: a handler exists for CRI precisely when its runtimes.<name> table
// exists, and a partial parse cannot be defeated by TOML the probe does not
// understand. It mirrors the installer's own ownership check
// (internal/installer/containerd.go).
func ScanContainerdConfig(root string) containerdScan {
	scan := containerdScan{Handlers: make(map[string]struct{})}

	// visited is keyed by the LOCAL (FSRoot-prefixed) path, so a file reached
	// both as a seed and as an import is read once and a cycle terminates.
	visited := make(map[string]bool)
	unreadable := make(map[string]bool)

	// queued pairs a file with the depth at which it was discovered.
	type queued struct {
		local string
		depth int
	}
	var pending []queued

	for _, dir := range containerdConfigSeedDirs {
		for _, path := range configFilesIn(filepath.Join(root, dir)) {
			pending = append(pending, queued{local: path})
		}
	}

	for len(pending) > 0 && len(scan.Scanned) < maxScanFiles {
		item := pending[0]
		pending = pending[1:]

		if visited[item.local] {
			continue
		}
		visited[item.local] = true

		data, err := os.ReadFile(item.local) //nolint:gosec // containerd config paths, read-only
		if err != nil {
			continue
		}
		scan.Scanned = append(scan.Scanned, item.local)

		for _, m := range runtimeHandlerTableRe.FindAllSubmatch(data, -1) {
			for _, group := range m[1:] {
				if len(group) > 0 {
					scan.Handlers[string(group)] = struct{}{}
					break
				}
			}
		}

		if item.depth >= maxImportDepth {
			continue
		}
		for _, imp := range parseImports(data) {
			locals, missingHost := resolveImport(root, item.local, imp)
			if missingHost != "" {
				unreadable[missingHost] = true
			}
			for _, l := range locals {
				if !visited[l] {
					pending = append(pending, queued{local: l, depth: item.depth + 1})
				}
			}
		}
	}

	for path := range unreadable {
		scan.Unreadable = append(scan.Unreadable, path)
	}
	sort.Strings(scan.Unreadable)
	return scan
}

// configFilesIn returns the *.toml and *.tmpl files directly inside dir, in a
// stable order. A directory that cannot be read yields nothing.
func configFilesIn(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".toml") && !strings.HasSuffix(name, ".tmpl") {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	paths := make([]string, 0, len(names))
	for _, name := range names {
		paths = append(paths, filepath.Join(dir, name))
	}
	return paths
}

// parseImports returns the raw entries of a config file's `imports` array.
func parseImports(data []byte) []string {
	var out []string
	for _, arr := range importsArrayRe.FindAllSubmatch(data, -1) {
		for _, m := range quotedStringRe.FindAllSubmatch(arr[1], -1) {
			for _, group := range m[1:] {
				if len(group) > 0 {
					if entry := strings.TrimSpace(string(group)); entry != "" {
						out = append(out, entry)
					}
					break
				}
			}
		}
	}
	return out
}

// resolveImport turns one `imports` entry into the local paths to read next.
//
// Two things make this more than a filepath.Join. First, an ABSOLUTE entry is
// a path on the HOST, and this agent reads the host through FSRoot, so it must
// be re-rooted — `/opt/kata/...` on the node is `/host/opt/kata/...` here.
// Second, containerd resolves a RELATIVE entry against the directory of the
// file that declared it, not against the process working directory.
//
// Entries may be globs. A glob that matches nothing is only reported as
// missing when its containing directory is unreadable too: a mounted, genuinely
// empty drop-in directory is a complete answer, whereas a directory this agent
// cannot see means the config points somewhere it has no access to. The
// returned missingHost is expressed in HOST terms, because that is the path an
// operator has to mount.
func resolveImport(root, fromLocal, entry string) (locals []string, missingHost string) {
	var localPattern, hostPattern string
	if filepath.IsAbs(entry) {
		hostPattern = filepath.Clean(entry)
		localPattern = filepath.Join(root, hostPattern)
	} else {
		localPattern = filepath.Join(filepath.Dir(fromLocal), entry)
		// Strip FSRoot back off for the host-facing message. Only meaningful
		// when root is a real prefix; otherwise fall back to the local path.
		hostPattern = localPattern
		if rel, err := filepath.Rel(root, localPattern); err == nil && !strings.HasPrefix(rel, "..") {
			hostPattern = filepath.Join("/", rel)
		}
	}

	matches, err := filepath.Glob(localPattern)
	if err != nil {
		// A malformed pattern is a config problem, not a mount problem.
		return nil, ""
	}
	if len(matches) > 0 {
		return matches, ""
	}

	// Nothing matched. Distinguish "mounted and empty" from "not mounted".
	if _, statErr := os.Stat(filepath.Dir(localPattern)); statErr != nil {
		return nil, hostPattern
	}
	return nil, ""
}

// handlerCheck is the outcome of asking "will containerd on this node accept
// pods with runtime handler X?".
type handlerCheck struct {
	// Configured is true only when a containerd config file was readable
	// AND it registers the handler. It is false both when the handler is
	// verified absent and when nothing could be read — an unverifiable
	// handler is not a usable one.
	Configured bool

	// State is one of handlerConfigured, handlerAbsent, handlerUnverifiable.
	State string

	// Reason explains a false Configured in the CapabilityResult's voice.
	Reason string
}

// checkContainerdHandler reports whether containerd under root registers the
// named CRI runtime handler.
//
// It fails closed. A node whose containerd has no handler cannot run a single
// Sandbox — every RunPodSandbox returns `no runtime for "<handler>" is
// configured` — and a node whose config the agent cannot read has not been
// shown to have one. Publishing a capability label on either is what turned a
// clean "no eligible node" into a containerd failure loop on a live pod
// (setec#243).
func checkContainerdHandler(root, handler string) handlerCheck {
	scan := ScanContainerdConfig(root)

	if len(scan.Scanned) == 0 {
		return handlerCheck{
			State: handlerUnverifiable,
			Reason: fmt.Sprintf(
				"cannot verify that containerd accepts runtime handler %q: no containerd "+
					"configuration is readable on this node. Mount it read-only into the agent "+
					"(Helm: runtimeAgent.mountContainerdConfig, runtimeAgent.mountK3sContainerdConfig)",
				handler),
		}
	}
	if _, ok := scan.Handlers[handler]; !ok {
		// An INCOMPLETE scan cannot conclude "absent". The config named files
		// this agent could not read, and the handler may be registered in one
		// of them — which is exactly what happened on every kata-deploy node
		// before setec#281: /etc/containerd/config.toml imports
		// /opt/kata/containerd/config.d/*.toml, that path was not mounted, and
		// the probe reported a confidently wrong "absent". Say what is missing
		// and name the value that fixes it instead of guessing.
		if len(scan.Unreadable) > 0 {
			return handlerCheck{
				State: handlerUnverifiable,
				Reason: fmt.Sprintf(
					"cannot verify that containerd accepts runtime handler %q: its configuration "+
						"imports %s, which this agent cannot read (%s register %s). Mount the path "+
						"read-only into the agent (Helm: runtimeAgent.extraContainerdConfigDirs)",
					handler, strings.Join(scan.Unreadable, ", "),
					strings.Join(scan.Scanned, ", "), describeHandlers(scan.Handlers)),
			}
		}
		return handlerCheck{
			State: handlerAbsent,
			Reason: fmt.Sprintf(
				"containerd has no runtime handler %q configured (%s register %s); every pod "+
					"sandbox on this node would fail with `no runtime for %q is configured`",
				handler, strings.Join(scan.Scanned, ", "), describeHandlers(scan.Handlers), handler),
		}
	}
	return handlerCheck{Configured: true, State: handlerConfigured}
}

// describeHandlers renders the discovered handler set for a Reason string in
// a stable order.
func describeHandlers(handlers map[string]struct{}) string {
	if len(handlers) == 0 {
		return "no runtime handlers"
	}
	names := make([]string, 0, len(handlers))
	for name := range handlers {
		names = append(names, name)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}
