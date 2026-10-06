// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

// Package podspec contains the pure translator that turns a Sandbox custom
// resource into the launcher Pod that the controller creates. The
// translator is side-effect free, so each mapping rule is verified with
// unit tests and no API server.
package podspec

import "errors"

const (
	// PodNameSuffix is appended to the Sandbox name to derive the Pod name
	// (e.g. Sandbox "foo" → Pod "foo-vm").
	PodNameSuffix = "-vm"

	// SandboxLabelKey is the label applied to the owned Pod whose value is
	// the owning Sandbox's name. Callers (e.g. the controller) use this label
	// for owner-ref indexing and to filter events.
	SandboxLabelKey = "setec.zeroroot.ai/sandbox"

	// sandboxKind is the literal kind used in the generated OwnerReference.
	sandboxKind = "Sandbox"

	// WorkspaceVolumeName is the Pod volume of the durable workspace PVC
	// of a session (docs/design/lifecycles.md, docs/design/storage.md).
	WorkspaceVolumeName = "workspace"

	// WorkspacePVCSuffix is appended to the Sandbox name to derive the
	// workspace PVC name (e.g. Sandbox "foo" → PVC "foo-workspace").
	WorkspacePVCSuffix = "-workspace"
)

// Errors returned by BuildLauncher for structural problems the OpenAPI
// schema cannot express (e.g. a caller hand-constructs a Sandbox in Go and
// skips the API server's validation entirely).
var (
	// ErrNilSandbox is returned when BuildLauncher gets a nil Sandbox.
	ErrNilSandbox = errors.New("podspec: sandbox is nil")

	// ErrMissingName is returned when Sandbox.metadata.name is empty.
	ErrMissingName = errors.New("podspec: sandbox.metadata.name is required")

	// ErrInvalidVCPU is returned when Sandbox.spec.resources.vcpu is less
	// than 1.
	ErrInvalidVCPU = errors.New("podspec: sandbox.spec.resources.vcpu must be >= 1")

	// ErrInvalidMemory is returned when Sandbox.spec.resources.memory is
	// zero or negative.
	ErrInvalidMemory = errors.New("podspec: sandbox.spec.resources.memory must be > 0")
)

// WorkspacePVCName derives the deterministic name of the workspace PVC
// owned by the named session Sandbox. Centralized so the controller
// (which creates and deletes the claim) and the builder (which mounts
// it) can never disagree.
func WorkspacePVCName(sandboxName string) string {
	return sandboxName + WorkspacePVCSuffix
}
