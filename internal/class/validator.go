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

// Package class provides two capabilities the rest of the operator composes:
// a Resolver that binds a Sandbox to its effective SandboxClass (either
// explicitly named or the cluster default), and a Validator that checks a
// Sandbox spec against the constraints published by its class.
//
// The Validator is deliberately pure — no Kubernetes client, no context —
// so it can run identically in the validating admission webhook and in the
// controller's reconcile loop without duplicating logic.
package class

import (
	"fmt"
	"slices"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
)

// ConstraintViolation describes a single reason a Sandbox spec is rejected
// against a SandboxClass. Field is the dotted JSON path of the offending
// field (e.g. "spec.resources.vcpu") so callers can build clear user-facing
// error messages and admission webhooks can expose it as a
// metav1.StatusCause.Field value.
type ConstraintViolation struct {
	// Field is the JSON-path of the violating Sandbox field.
	Field string
	// Message is a short human-readable explanation suitable for display
	// in `kubectl apply` rejection output.
	Message string
}

// String renders the violation in the shape used throughout the webhook and
// controller surface ("<field>: <message>"). Centralizing it here keeps the
// two surfaces consistent.
func (v ConstraintViolation) String() string {
	return v.Field + ": " + v.Message
}

// Validate reports every way in which sb violates class's published
// constraints. It is a pure function: no Kubernetes API calls, no context,
// no hidden state. An empty return slice means the Sandbox is accepted.
//
// Nil inputs are treated conservatively: a nil Sandbox yields a single
// violation ("sandbox is nil"); a nil class yields no violations (the
// resolver layer owns the "no class available" outcome, not the validator).
func Validate(sb *setecv1alpha1.Sandbox, cls *setecv1alpha1.SandboxClass) []ConstraintViolation {
	if sb == nil {
		return []ConstraintViolation{{Field: "", Message: "sandbox is nil"}}
	}
	if cls == nil {
		// The validator's contract is "given this class, is this
		// Sandbox permitted". Absence of a class is a resolver-layer
		// concern expressed via ErrClassNotFound / ErrNoDefaultClass.
		return nil
	}

	var out []ConstraintViolation

	// Resource ceilings. MaxResources is optional; when nil the class
	// imposes no class-level ceiling (the namespace ResourceQuota still
	// applies). VCPU and Memory are checked independently so callers see
	// every offending field in a single pass.
	if cls.Spec.MaxResources != nil {
		if sb.Spec.Resources.VCPU > cls.Spec.MaxResources.VCPU {
			out = append(out, ConstraintViolation{
				Field: "spec.resources.vcpu",
				Message: fmt.Sprintf(
					"Sandbox requests %d vcpu but SandboxClass %q allows maximum %d vcpu",
					sb.Spec.Resources.VCPU, cls.Name, cls.Spec.MaxResources.VCPU,
				),
			})
		}
		// resource.Quantity.Cmp returns -1, 0, or 1 for a<b, a==b, a>b.
		if sb.Spec.Resources.Memory.Cmp(cls.Spec.MaxResources.Memory) > 0 {
			out = append(out, ConstraintViolation{
				Field: "spec.resources.memory",
				Message: fmt.Sprintf(
					"Sandbox requests %s memory but SandboxClass %q allows maximum %s",
					sb.Spec.Resources.Memory.String(), cls.Name,
					cls.Spec.MaxResources.Memory.String(),
				),
			})
		}
	}

	// Network mode enforcement. An empty AllowedNetworkModes list means
	// the class imposes no restriction (back-compat for Phase 1 users).
	if len(cls.Spec.AllowedNetworkModes) > 0 && sb.Spec.Network != nil {
		if !containsNetworkMode(cls.Spec.AllowedNetworkModes, sb.Spec.Network.Mode) {
			out = append(out, ConstraintViolation{
				Field: "spec.network.mode",
				Message: fmt.Sprintf(
					"Sandbox requests network.mode=%q but SandboxClass %q only allows modes %v",
					sb.Spec.Network.Mode, cls.Name, cls.Spec.AllowedNetworkModes,
				),
			})
		}
	}

	return out
}

// containsNetworkMode is a tiny helper that avoids bringing in
// golang.org/x/exp/slices for a one-line search.
func containsNetworkMode(modes []setecv1alpha1.NetworkMode, want setecv1alpha1.NetworkMode) bool {
	return slices.Contains(modes, want)
}
