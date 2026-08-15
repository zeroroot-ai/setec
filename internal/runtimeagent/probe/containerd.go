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

// containerdConfigDirs lists the directories, relative to Config.FSRoot, that
// may hold the node's containerd CRI configuration. Every *.toml and *.tmpl
// file directly inside them is scanned.
//
// Both containerd flavours the installer supports are covered (see
// internal/installer/containerd.go, which writes into exactly these places):
//
//   - stock containerd: /etc/containerd/config.toml plus the drop-in dirs it
//     imports (config.d is what the Setec installer and kata-deploy write;
//     conf.d and config.toml.d appear in other distributions).
//   - k3s: /var/lib/rancher/k3s/agent/etc/containerd/{config.toml,
//     config*.toml.tmpl, config.toml.d/*.toml}. k3s regenerates config.toml
//     from the template, so the template is scanned too — it is the file that
//     survives a k3s restart.
//
// The agent only sees a directory when the DaemonSet mounts it read-only
// (charts/setec/templates/runtime-agent-daemonset.yaml). An unmounted
// directory is indistinguishable from an absent one and is simply skipped;
// what matters to the caller is whether ANY file was readable.
var containerdConfigDirs = []string{
	"etc/containerd",
	"etc/containerd/config.d",
	"etc/containerd/conf.d",
	"etc/containerd/config.toml.d",
	"var/lib/rancher/k3s/agent/etc/containerd",
	"var/lib/rancher/k3s/agent/etc/containerd/config.toml.d",
}

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

// ConfiguredRuntimeHandlers scans the node's containerd configuration under
// root and returns the set of CRI runtime handler names containerd is
// configured to accept, together with the config files that were actually
// read.
//
// An empty scanned slice means no containerd configuration was readable at
// all — the caller must treat that as "cannot verify", which is a different
// answer from "verified absent".
//
// The scan is a read-only regexp over the table headers rather than a full
// TOML parse: a handler exists for CRI precisely when its runtimes.<name>
// table exists, and a partial parse cannot be defeated by TOML the probe does
// not understand. It mirrors the installer's own ownership check
// (internal/installer/containerd.go).
func ConfiguredRuntimeHandlers(root string) (handlers map[string]struct{}, scanned []string) {
	handlers = make(map[string]struct{})

	for _, dir := range containerdConfigDirs {
		full := filepath.Join(root, dir)
		entries, err := os.ReadDir(full)
		if err != nil {
			continue
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
		for _, name := range names {
			path := filepath.Join(full, name)
			data, err := os.ReadFile(path) //nolint:gosec // fixed config dirs, read-only
			if err != nil {
				continue
			}
			scanned = append(scanned, path)
			for _, m := range runtimeHandlerTableRe.FindAllSubmatch(data, -1) {
				for _, group := range m[1:] {
					if len(group) > 0 {
						handlers[string(group)] = struct{}{}
						break
					}
				}
			}
		}
	}
	return handlers, scanned
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
	handlers, scanned := ConfiguredRuntimeHandlers(root)

	if len(scanned) == 0 {
		return handlerCheck{
			State: handlerUnverifiable,
			Reason: fmt.Sprintf(
				"cannot verify that containerd accepts runtime handler %q: no containerd "+
					"configuration is readable on this node. Mount it read-only into the agent "+
					"(Helm: runtimeAgent.mountContainerdConfig, runtimeAgent.mountK3sContainerdConfig)",
				handler),
		}
	}
	if _, ok := handlers[handler]; !ok {
		return handlerCheck{
			State: handlerAbsent,
			Reason: fmt.Sprintf(
				"containerd has no runtime handler %q configured (%s register %s); every pod "+
					"sandbox on this node would fail with `no runtime for %q is configured`",
				handler, strings.Join(scanned, ", "), describeHandlers(handlers), handler),
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
