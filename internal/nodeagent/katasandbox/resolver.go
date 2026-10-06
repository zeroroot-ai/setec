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
// containerd CRI plugin records the sandbox as a container labeled
// with the Pod UID.
package katasandbox

import (
	"context"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"regexp"
	"strings"
	"time"
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
	// FCRoot is the host directory Firecracker sees as FCMount. kata runs
	// Firecracker under the jailer, chrooted into <vm>/root, so a file
	// path handed to the Firecracker API resolves inside that
	// directory. It is "/" for an unjailed Firecracker.
	FCRoot string
	// FCMount is the path at which Firecracker sees FCRoot. Empty means
	// "/". A launcher Pod sees its work volume at /work.
	FCMount string
	// Launcher is true for the Firecracker machine of a launcher Pod. The
	// launcher loads a snapshot itself, so a restore stages the files for
	// it instead of calling the Firecracker API.
	Launcher bool
}

// FCPath returns the path Firecracker sees for hostPath, which must lie
// under FCRoot.
func (p Paths) FCPath(hostPath string) (string, error) {
	root := p.FCRoot
	if root == "" {
		root = "/"
	}
	rel, err := filepath.Rel(root, hostPath)
	if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
		return "", fmt.Errorf("katasandbox: %s is outside the Firecracker root %s", hostPath, root)
	}
	mount := p.FCMount
	if mount == "" {
		mount = "/"
	}
	return filepath.Join(mount, rel), nil
}

// fcRootFrom derives Firecracker's root on the host from the host path
// of its API socket and the address Firecracker bound the socket to,
// which is the path as Firecracker sees it.
func fcRootFrom(hostSocket, boundAs string) (string, error) {
	if boundAs == "" || boundAs == hostSocket {
		return "/", nil
	}
	if !filepath.IsAbs(boundAs) || !strings.HasSuffix(hostSocket, boundAs) {
		return "", fmt.Errorf("katasandbox: socket %s is bound as %q, which is not a suffix of it", hostSocket, boundAs)
	}
	root := strings.TrimSuffix(hostSocket, boundAs)
	if root == "" {
		return "/", nil
	}
	return root, nil
}

// dialTimeout bounds the one connection Resolve opens to read the
// socket's bound address.
const dialTimeout = 5 * time.Second

// firecrackerRoot connects to the API socket once and reads the address
// Firecracker bound it to. getpeername on a Unix socket returns the path
// the server passed to bind, so a jailed Firecracker reports
// "/run/firecracker.socket" for a socket the host sees under its chroot.
func firecrackerRoot(ctx context.Context, hostSocket string) (string, error) {
	d := net.Dialer{Timeout: dialTimeout}
	conn, err := d.DialContext(ctx, "unix", hostSocket)
	if err != nil {
		return "", fmt.Errorf("katasandbox: dial %s: %w", hostSocket, err)
	}
	boundAs := conn.RemoteAddr().String()
	_ = conn.Close()
	return fcRootFrom(hostSocket, boundAs)
}

// Resolver maps a Pod UID to its kata sandbox's Paths.
type Resolver struct {
	Lookup SandboxIDLookup
	// VCRoot is the kata run directory. Empty means DefaultVCRoot.
	VCRoot string
	// RootOf finds Firecracker's root from its API socket. Nil means
	// firecrackerRoot, which dials the socket. Tests replace it.
	RootOf func(ctx context.Context, hostSocket string) (string, error)
}

func (r Resolver) rootOf(ctx context.Context, hostSocket string) (string, error) {
	if r.RootOf != nil {
		return r.RootOf(ctx, hostSocket)
	}
	return firecrackerRoot(ctx, hostSocket)
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
	fcRoot, err := r.rootOf(ctx, matches[0])
	if err != nil {
		return Paths{}, err
	}
	return Paths{APISocket: matches[0], HybridVsock: filepath.Join(vmRoot, "kata.hvsock"), FCRoot: fcRoot}, nil
}
