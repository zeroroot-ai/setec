// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

// Package launchersandbox finds the Firecracker files of a launcher Pod on
// the node from the Pod UID. The launcher keeps them in the work volume of
// the Pod, an emptyDir that the kubelet keeps at
//
//	<pods dir>/<pod UID>/volumes/kubernetes.io~empty-dir/work
//
// and that the launcher container sees at /work.
package launchersandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/zeroroot-ai/setec/internal/podspec"
)

// ErrNotFound means that this node has no launcher machine for the Pod.
var ErrNotFound = errors.New("launchersandbox: no launcher machine for this pod on this node")

// Paths are the Firecracker files of the machine of a launcher Pod.
type Paths struct {
	// APISocket is the Firecracker API socket on the host.
	APISocket string
	// HybridVsock is the vsock socket of the machine on the host.
	HybridVsock string
	// FCRoot is the work volume of the Pod on the host. FCMount is where
	// the launcher, and so Firecracker, sees it.
	FCRoot  string
	FCMount string
}

// FCPath maps a host path under FCRoot to the path that Firecracker sees.
func (p Paths) FCPath(hostPath string) (string, error) {
	rel, err := filepath.Rel(p.FCRoot, hostPath)
	if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
		return "", fmt.Errorf("launchersandbox: %s is outside the work volume %s", hostPath, p.FCRoot)
	}
	return filepath.Join(p.FCMount, rel), nil
}

// DefaultPodsDir is the Pod directory of the kubelet on the host.
const DefaultPodsDir = "/var/lib/kubelet/pods"

var podUIDPattern = regexp.MustCompile(`^[0-9a-fA-F-]{1,64}$`)

// Resolver maps a Pod UID to the Firecracker files of its launcher.
type Resolver struct {
	// PodsDir is the Pod directory of the kubelet. Empty means
	// DefaultPodsDir.
	PodsDir string
}

// WorkDir returns the host path of the work volume of the Pod.
func (r Resolver) WorkDir(podUID string) (string, error) {
	if !podUIDPattern.MatchString(podUID) {
		return "", fmt.Errorf("launchersandbox: %q is not a pod UID", podUID)
	}
	dir := r.PodsDir
	if dir == "" {
		dir = DefaultPodsDir
	}
	return filepath.Join(dir, podUID, "volumes", "kubernetes.io~empty-dir", podspec.LauncherWorkVolume), nil
}

// Resolve returns the Paths of the launcher machine of the Pod, or
// ErrNotFound when the Pod has no launcher machine on this
// node.
func (r Resolver) Resolve(_ context.Context, podUID string) (Paths, error) {
	work, err := r.WorkDir(podUID)
	if err != nil {
		return Paths{}, err
	}
	vm := filepath.Join(work, filepath.Base(podspec.LauncherVMDir))
	if _, err := os.Stat(vm); err != nil {
		if os.IsNotExist(err) {
			return Paths{}, ErrNotFound
		}
		return Paths{}, fmt.Errorf("launchersandbox: %w", err)
	}
	return Paths{
		APISocket:   filepath.Join(vm, podspec.LauncherAPISocket),
		HybridVsock: filepath.Join(vm, podspec.LauncherVsockSocket),
		FCRoot:      work,
		FCMount:     podspec.LauncherWorkMountPath,
	}, nil
}

// HostPath returns the host path of podPath, a path in the work volume as
// the launcher Pod sees it.
func HostPath(p Paths, podPath string) string {
	rel := strings.TrimPrefix(podPath, podspec.LauncherWorkMountPath)
	return filepath.Join(p.FCRoot, rel)
}
