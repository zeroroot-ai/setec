// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

// Package launcher runs one Firecracker machine inside a launcher Pod
// (docs/design/runtime.md). It joins the machine to the Pod network
// interface, attaches its disks, sends its console to stdout, and ends with
// the exit code of the workload.
//
// "Boot" and "load a snapshot" are one call: Spec.Source names a kernel or a
// snapshot, and Run starts the machine from it.
package launcher

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/zeroroot-ai/setec/internal/guestagent"
)

// Spec is what the operator writes into the launcher Pod.
type Spec struct {
	// VCPU and MemoryMiB size the machine. The operator copies them from
	// the limits of the Sandbox, so the machine never exceeds its Pod.
	VCPU      int `json:"vcpu"`
	MemoryMiB int `json:"memoryMiB"`

	// ImageDisk is the read-only disk of the image digest (vda), and
	// DiskSignature its signature. Both come from the image volume of the
	// Pod (setec-disk-builder). The launcher checks the signature before
	// the machine starts.
	ImageDisk     string `json:"imageDisk"`
	DiskSignature string `json:"diskSignature,omitempty"`
	// ImageRef is the image with its digest. The signature must name it.
	ImageRef string `json:"imageRef,omitempty"`
	// DiskKeys are the base64 ed25519 public keys that may sign the disk.
	DiskKeys []string `json:"diskKeys,omitempty"`
	// WritableDisk is the writable layer of this Sandbox (vdb). The
	// launcher makes it as a sparse file of WritableBytes when it does not
	// exist.
	WritableDisk  string `json:"writableDisk"`
	WritableBytes int64  `json:"writableBytes"`
	// WorkspaceDevice is the block device of a session workspace (vdc).
	// Empty for an ephemeral Sandbox.
	WorkspaceDevice string `json:"workspaceDevice,omitempty"`

	// Source is the boot or the snapshot that the machine starts from.
	Source Source `json:"source"`

	// WorkDir holds the API socket, the vsock socket and the config.
	WorkDir string `json:"workDir"`

	// Workload is the process that the guest agent starts after a boot:
	// the entry point, user, directory and environment of the image, with
	// the command and environment of the Sandbox applied by the operator.
	Workload *guestagent.Process `json:"workload,omitempty"`
}

// Source is exactly one of a boot and a snapshot.
type Source struct {
	Boot     *BootSource     `json:"boot,omitempty"`
	Snapshot *SnapshotSource `json:"snapshot,omitempty"`
}

// BootSource starts the machine from a kernel and an initrd.
type BootSource struct {
	Kernel   string `json:"kernel"`
	Initrd   string `json:"initrd,omitempty"`
	BootArgs string `json:"bootArgs,omitempty"`
}

// SnapshotSource loads a machine state and its memory file.
type SnapshotSource struct {
	State  string `json:"state"`
	Memory string `json:"memory"`
}

// SpecEnv is the environment variable through which the operator passes
// the spec (podspec.LauncherSpecEnv).
const SpecEnv = "SETEC_LAUNCHER_SPEC"

// ReadSpec reads and checks the spec: from SpecEnv when it is set, else
// from the file at path.
func ReadSpec(path string) (*Spec, error) {
	raw := []byte(os.Getenv(SpecEnv))
	if len(raw) == 0 {
		var err error
		raw, err = os.ReadFile(path) //nolint:gosec // the path is a flag of the launcher
		if err != nil {
			return nil, fmt.Errorf("launcher: read spec: %w", err)
		}
	}
	s := &Spec{}
	if err := json.Unmarshal(raw, s); err != nil {
		return nil, fmt.Errorf("launcher: parse spec: %w", err)
	}
	return s, s.Validate()
}

// Validate reports the first problem of s.
func (s *Spec) Validate() error {
	switch {
	case s.VCPU < 1 || s.VCPU > 32:
		return fmt.Errorf("launcher: vcpu must be 1 to 32, got %d", s.VCPU)
	case s.MemoryMiB < 128:
		return fmt.Errorf("launcher: memory must be at least 128 MiB, got %d", s.MemoryMiB)
	case !filepath.IsAbs(s.ImageDisk), !filepath.IsAbs(s.WritableDisk), !filepath.IsAbs(s.WorkDir):
		return errors.New("launcher: imageDisk, writableDisk and workDir must be absolute paths")
	case s.WritableBytes <= 0:
		return errors.New("launcher: writableBytes must be positive")
	case s.WorkspaceDevice != "" && !filepath.IsAbs(s.WorkspaceDevice):
		return errors.New("launcher: workspaceDevice must be an absolute path")
	case (s.Source.Boot == nil) == (s.Source.Snapshot == nil):
		return errors.New("launcher: the source must be exactly one of boot and snapshot")
	case s.Source.Boot != nil && !filepath.IsAbs(s.Source.Boot.Kernel):
		return errors.New("launcher: the boot kernel must be an absolute path")
	case s.Source.Snapshot != nil && (!filepath.IsAbs(s.Source.Snapshot.State) || !filepath.IsAbs(s.Source.Snapshot.Memory)):
		return errors.New("launcher: the snapshot state and memory must be absolute paths")
	case s.Source.Boot != nil && s.Workload == nil:
		return errors.New("launcher: a boot needs a workload; an empty argv runs the image entry point")
	}
	return nil
}
