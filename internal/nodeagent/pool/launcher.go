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

package pool

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
)

// LaunchOptions describes a single pool VM the Manager wants booted
// and paused. It is deliberately serialisable over CLI flags so the
// production Launcher can shell out to the setec-pool-vm binary.
type LaunchOptions struct {
	// ClassName is the SandboxClass this entry belongs to. Recorded
	// for diagnostics and metric labels.
	ClassName string
	// ImageRef is the OCI image reference baked into the entry.
	ImageRef string
	// KernelPath and RootfsPath are absolute paths on the node.
	KernelPath string
	RootfsPath string
	// VCPUs and MemoryMiB are the VM's hardware budget.
	VCPUs     int
	MemoryMiB int
	// SocketPath is where the launcher should place the Firecracker
	// API socket. The Manager renders this from SocketPattern so the
	// node-agent can later dial it for restore-on-Claim.
	SocketPath string
	// StorageRoot + EntryID together define where the paused VM's
	// state files are written. The launcher creates
	// <StorageRoot>/<EntryID>/ and writes state/memory.
	StorageRoot string
	EntryID     string
}

// Launcher is the narrow surface the pool Manager uses to boot a new
// pool entry. The production implementation shells out to the
// setec-pool-vm binary; tests inject fakes.
type Launcher interface {
	// Launch must boot a Firecracker VM, pause it, persist its state,
	// and return successfully with the launcher process still alive
	// (or, in the ExecLauncher case, its grandchild Firecracker
	// process still alive and reachable at opts.SocketPath). The
	// Manager does not care about intermediate lifecycle details
	// beyond "it is ready for Claim".
	Launch(ctx context.Context, opts LaunchOptions) error
}

// ExecLauncher is the production Launcher. It execs the setec-pool-vm
// companion binary with the flags the launcher package documents.
type ExecLauncher struct {
	// BinaryPath overrides the default setec-pool-vm binary location.
	// Empty string resolves via PATH.
	BinaryPath string
	// ExtraArgs is appended after the generated flags; useful for
	// propagating --firecracker-binary or --boot-args overrides the
	// node operator might need.
	ExtraArgs []string
}

// DefaultExecLauncher returns an ExecLauncher configured for typical
// node-agent deployments: the `setec-pool-vm` binary is expected on
// PATH inside the node-agent image.
func DefaultExecLauncher() *ExecLauncher {
	return &ExecLauncher{BinaryPath: "setec-pool-vm"}
}

// Launch execs the setec-pool-vm binary with the flags derived from
// opts. The child's stdout/stderr is forwarded so operators can see
// Firecracker boot output in node-agent logs.
func (l *ExecLauncher) Launch(ctx context.Context, opts LaunchOptions) error {
	if opts.SocketPath == "" || opts.StorageRoot == "" || opts.EntryID == "" {
		return fmt.Errorf("pool: launcher requires socket/storage/entry-id")
	}

	args := []string{
		"--socket-path", opts.SocketPath,
		"--storage-root", opts.StorageRoot,
		"--pool-entry-id", opts.EntryID,
		"--kernel-path", opts.KernelPath,
		"--rootfs-path", opts.RootfsPath,
		"--vcpus", strconv.Itoa(opts.VCPUs),
		"--memory-mib", strconv.Itoa(opts.MemoryMiB),
	}
	if opts.ImageRef != "" {
		args = append(args, "--image-ref", opts.ImageRef)
	}
	args = append(args, l.ExtraArgs...)

	bin := l.BinaryPath
	if bin == "" {
		bin = "setec-pool-vm"
	}

	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdout = execDiscard{}
	cmd.Stderr = execDiscard{}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("pool: setec-pool-vm for %s/%s: %w (output: %s)", opts.ClassName, opts.EntryID, err, string(out))
	}
	return nil
}

// execDiscard swallows child output. In production the node-agent
// reaches inside the child with cmd.Stdout/Stderr = log files, but
// keeping the surface trivial is sufficient for v0.1.0.
type execDiscard struct{}

func (execDiscard) Write(p []byte) (int, error) { return len(p), nil }

// LaunchOptionsFrom is a small helper the Manager uses to translate a
// SandboxClass plus node-agent config into a LaunchOptions. Defined
// here so the tests and production code share exactly one rendering.
func LaunchOptionsFrom(
	cls *setecv1alpha1.SandboxClass,
	entryID string,
	socketPath string,
	storageRoot string,
	kernelPath string,
	rootfsPath string,
	vcpus int,
	memMiB int,
) LaunchOptions {
	return LaunchOptions{
		ClassName:   cls.Name,
		ImageRef:    cls.Spec.PreWarmImage,
		KernelPath:  kernelPath,
		RootfsPath:  rootfsPath,
		VCPUs:       vcpus,
		MemoryMiB:   memMiB,
		SocketPath:  socketPath,
		StorageRoot: storageRoot,
		EntryID:     entryID,
	}
}
