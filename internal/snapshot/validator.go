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

// Package snapshot groups the Phase 3 snapshot-orchestration helpers:
// a pure Validator that checks Sandbox <-> Snapshot <-> SandboxClass
// compatibility, and a Coordinator that sequences operator-side
// create/restore/pause/resume work across the node-agent.
package snapshot

import (
	"fmt"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
)

// ConstraintViolation describes a single reason a Sandbox cannot
// restore from a given Snapshot. Field is the dotted JSON path of the
// offending Sandbox field so admission webhooks can surface it as a
// metav1.StatusCause.
type ConstraintViolation struct {
	Field   string
	Message string
}

// String renders the violation as "<field>: <message>" — matching the
// existing class.ConstraintViolation convention so controller and
// webhook error output stay consistent.
func (v ConstraintViolation) String() string {
	return v.Field + ": " + v.Message
}

// Validate reports every way a Sandbox is incompatible with the given
// Snapshot under the given SandboxClass. A nil Sandbox or nil Snapshot
// yields a single violation; a nil class is treated as "no class
// cross-check" (the resolver layer owns class-presence).
//
// The Validator is a pure function: no Kubernetes client, no context,
// no hidden state. Both the admission webhook and the reconciler call
// into the same implementation so rejection reasons are identical.
func Validate(sb *setecv1alpha1.Sandbox, snap *setecv1alpha1.Snapshot, class *setecv1alpha1.SandboxClass) []ConstraintViolation {
	if sb == nil {
		return []ConstraintViolation{{Field: "", Message: "sandbox is nil"}}
	}
	if snap == nil {
		return []ConstraintViolation{{Field: "spec.snapshotRef.name", Message: "snapshot is nil"}}
	}

	var out []ConstraintViolation

	// Namespace match. Cross-namespace snapshot references are a hard
	// security boundary: snapshot state contains memory which may
	// hold secrets from the source tenant. The comparison uses the
	// object-level Namespace rather than any field in the spec so the
	// caller cannot forge a match by editing the Snapshot.
	if sb.Namespace != snap.Namespace {
		out = append(out, ConstraintViolation{
			Field: "spec.snapshotRef.name",
			Message: fmt.Sprintf(
				"Snapshot %q is in namespace %q but Sandbox is in namespace %q; cross-namespace restore is not permitted",
				snap.Name, snap.Namespace, sb.Namespace,
			),
		})
	}

	// SandboxClass match. The Snapshot records the class its source
	// Sandbox used; the restore target's class MUST match, otherwise
	// the VMM/runtime configuration baked into the snapshot may be
	// incompatible with the pod spec the operator is about to
	// render.
	//
	// Resolution of the effective SandboxClass name is the
	// controller's job; this function compares names as-given.
	if class != nil && snap.Spec.SandboxClass != class.Name {
		out = append(out, ConstraintViolation{
			Field: "spec.sandboxClassName",
			Message: fmt.Sprintf(
				"Snapshot %q was captured under SandboxClass %q but the resolved class is %q",
				snap.Name, snap.Spec.SandboxClass, class.Name,
			),
		})
	}

	// Image match. When the user leaves Sandbox.spec.image empty and
	// relies solely on snapshotRef, the snapshot's image is used
	// verbatim (no conflict). When the user sets spec.image explicitly
	// it must match the snapshot's original image — restoring onto a
	// different image would combine memory state from image A with a
	// container filesystem from image B, which is a correctness
	// minefield.
	if sb.Spec.Image != "" && sb.Spec.Image != snap.Spec.ImageRef {
		out = append(out, ConstraintViolation{
			Field: "spec.image",
			Message: fmt.Sprintf(
				"Sandbox requests image %q but Snapshot %q was captured from image %q",
				sb.Spec.Image, snap.Name, snap.Spec.ImageRef,
			),
		})
	}

	// VMM match. The Snapshot is bound to a specific VMM; the
	// restore target's class must agree. We treat the class's VMM as
	// authoritative when a class is supplied.
	if class != nil && snap.Spec.VMM != "" && snap.Spec.VMM != class.Spec.VMM { //nolint:staticcheck // back-compat: VMM retained until v2
		out = append(out, ConstraintViolation{
			Field: "spec.sandboxClassName",
			Message: fmt.Sprintf(
				"Snapshot %q was captured on VMM %q but the resolved class uses VMM %q",
				snap.Name, snap.Spec.VMM, class.Spec.VMM, //nolint:staticcheck // back-compat: VMM retained until v2
			),
		})
	}

	return out
}
