// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

// Package katasandbox finds a kata-fc Pod's Firecracker files on the
// node from the Pod's UID (setec#19).
//
// The kata Go runtime (setec pins kata-go-static, kata.env) keeps a
// sandbox's Firecracker API socket at
//
//	/run/vc/<hypervisor binary name>/<id>/root/run/firecracker.socket
//
// and its hybrid vsock at /run/vc/<hypervisor>/<id>/root/kata.hvsock,
// where <id> is the first 32 characters of the CRI sandbox id
// (kata-containers src/runtime/virtcontainers/fc.go, setPaths and
// truncateID). The CRI sandbox id is not the Pod UID, and the Pod
// object does not carry it, so only the node can resolve it: the
// containerd CRI plugin records the sandbox as a container labelled
// with the Pod UID.
package katasandbox

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
)

// DefaultVCRoot is the kata Go runtime's run directory on the host.
const DefaultVCRoot = "/run/vc"

// kataIDLen is the length kata truncates a sandbox id to.
const kataIDLen = 32

// ErrNotFound means the Pod has no running kata sandbox on this node.
var ErrNotFound = errors.New("katasandbox: no kata sandbox for this pod on this node")

// podUIDPattern is the shape of a Kubernetes object UID. The UID is
// placed in a containerd filter, so anything else is refused.
var podUIDPattern = regexp.MustCompile(`^[0-9a-fA-F-]{1,64}$`)

// SandboxIDLookup returns the CRI sandbox id of the Pod with the given
// UID on this node, or ErrNotFound.
type SandboxIDLookup interface {
	SandboxID(ctx context.Context, podUID string) (string, error)
}

// Paths are one kata sandbox's Firecracker files.
type Paths struct {
	// APISocket is the Firecracker API socket.
	APISocket string
	// HybridVsock is the Firecracker hybrid vsock the guest agent
	// listens behind.
	HybridVsock string
}

// Resolver maps a Pod UID to its kata sandbox's Paths.
type Resolver struct {
	Lookup SandboxIDLookup
	// VCRoot is the kata run directory. Empty means DefaultVCRoot.
	VCRoot string
}

// Resolve returns the Paths of the kata sandbox that runs the Pod with
// the given UID.
func (r Resolver) Resolve(ctx context.Context, podUID string) (Paths, error) {
	if !podUIDPattern.MatchString(podUID) {
		return Paths{}, fmt.Errorf("katasandbox: %q is not a pod UID", podUID)
	}
	if r.Lookup == nil {
		return Paths{}, errors.New("katasandbox: no sandbox lookup configured")
	}
	id, err := r.Lookup.SandboxID(ctx, podUID)
	if err != nil {
		return Paths{}, err
	}
	if len(id) > kataIDLen {
		id = id[:kataIDLen]
	}
	root := r.VCRoot
	if root == "" {
		root = DefaultVCRoot
	}
	// The hypervisor directory is the basename of the configured
	// Firecracker binary, which this package does not need to know.
	matches, err := filepath.Glob(filepath.Join(root, "*", id, "root", "run", "firecracker.socket"))
	if err != nil {
		return Paths{}, fmt.Errorf("katasandbox: glob: %w", err)
	}
	switch len(matches) {
	case 0:
		return Paths{}, fmt.Errorf("%w: pod %s, sandbox %s has no firecracker.socket under %s",
			ErrNotFound, podUID, id, root)
	case 1:
	default:
		return Paths{}, fmt.Errorf("katasandbox: pod %s, sandbox %s has %d firecracker sockets: %v",
			podUID, id, len(matches), matches)
	}
	vmRoot := filepath.Dir(filepath.Dir(matches[0]))
	return Paths{APISocket: matches[0], HybridVsock: filepath.Join(vmRoot, "kata.hvsock")}, nil
}
