// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package runtime

import corev1 "k8s.io/api/core/v1"

// BackendLauncher is the launcher runtime (docs/design/runtime.md): one
// Firecracker machine in a launcher Pod that runs on the default container
// runtime. The Pod asks for setec.zeroroot.ai/kvm, so the scheduler places
// it on a node whose device plugin offers /dev/kvm, and no RuntimeClass and
// no node label are involved.
const BackendLauncher = "launcher"

// LauncherDispatcher is the Dispatcher of BackendLauncher. The controller
// builds a launcher Pod (podspec.BuildLauncher) for it instead of a
// RuntimeClass Pod.
type LauncherDispatcher struct{}

// NewLauncherDispatcher returns the launcher Dispatcher.
func NewLauncherDispatcher() *LauncherDispatcher { return &LauncherDispatcher{} }

// Name implements Dispatcher.
func (*LauncherDispatcher) Name() string { return BackendLauncher }

// RuntimeClassName implements Dispatcher: the launcher Pod runs on the
// default runtime of the node.
func (*LauncherDispatcher) RuntimeClassName() string { return "" }

// NodeAffinity implements Dispatcher. The device resources of the Pod do
// the placement.
func (*LauncherDispatcher) NodeAffinity() *corev1.NodeAffinity { return nil }

// Overhead implements Dispatcher. The machine lives inside the Pod limits.
func (*LauncherDispatcher) Overhead() corev1.ResourceList { return nil }

// MutatePod implements Dispatcher. The launcher Pod is complete as built.
func (*LauncherDispatcher) MutatePod(*corev1.Pod, map[string]string) error { return nil }
