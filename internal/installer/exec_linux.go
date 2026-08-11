//go:build linux

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
	"os/exec"
	"strings"
	"syscall"
)

// NewHostRunner returns the production Runner: every command is executed
// chrooted into hostRoot (the hostPath mount of the node's /), so it runs
// the HOST's binaries against the HOST's filesystem.
//
// Why chroot instead of nsenter: the DaemonSet runs with hostPID and
// mounts / with mountPropagation HostToContainer, so hostRoot is a full
// recursive view of the host filesystem including /proc, /run and /sys.
// Chrooting a child into it makes systemctl work exactly as it does on
// the node (systemd's running-in-chroot detection compares / with
// /proc/1/root, and with hostPID those are the same tree), without
// needing an nsenter binary in a distroless image. exec.Cmd supports
// this natively via SysProcAttr.Chroot — no fork/exec gymnastics.
type hostRunner struct {
	root string
}

// NewHostRunner constructs a Runner that chroots into root for every
// command.
func NewHostRunner(root string) Runner {
	return &hostRunner{root: root}
}

func (r *hostRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	path, err := lookPathIn(r.root, name)
	if err != nil {
		return nil, err
	}
	// exec.CommandContext would LookPath in the INSTALLER's mount view;
	// build the Cmd by hand so Path is chroot-relative.
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Path = path
	cmd.Env = []string{"PATH=" + strings.Join(hostPATH, ":")}
	cmd.SysProcAttr = &syscall.SysProcAttr{Chroot: r.root}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("%s %s: %w (output: %s)", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return out, nil
}
