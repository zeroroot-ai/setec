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

// Package gate is the single decision point for the ADR-0005
// snapshot-isolation invariant gate: a snapshot restore or checkpoint
// resume is served OUTSIDE dev-mode only when every one of the five
// isolation invariants carries a positive per-restore verification.
//
// The gate builds no verification machinery of its own — the checks
// live where the state lives (the node-agent's fail-closed reseed and
// uniquification, the pool manager's provenance verification, the
// sealed-DEK at-rest encryption, the coordinator's one-shot claim
// semantics). What this package owns is the DECISION: it takes the
// per-restore evidence those mechanisms report, treats every absent or
// negative signal as a violation (fail closed — absence of evidence is
// never evidence of absence), and answers exactly one question: may
// this restore be handed to a caller?
//
// Dev-mode opt-out mirrors the existing dev-only-runtime gate (the
// runc admission gate): the cluster operator labels the well-known
// gate namespace with setec.zeroroot.ai/allow-dev-runtimes=true AND
// annotates the specific SandboxClass with
// setec.zeroroot.ai/allow-unverified-restores="true". Both signals are
// required; either alone changes nothing. The opt-out is loud: the
// class controller stamps an UnverifiedRestoresAllowed condition on
// the class and the coordinator emits a Warning event on every restore
// that was served despite failed verifications.
package gate

import (
	"context"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
)

// Invariant names one of the five ADR-0005 isolation invariants. The
// values are bounded, human-meaningful strings used verbatim in
// Events, conditions, and metric labels.
type Invariant string

const (
	// InvariantCleanBase (ADR-0005 #1): the restored state is the
	// class image booted to guest-agent-ready — no tenant data, no
	// secrets, no prior input. Per-restore evidence: the verified
	// template-provenance record (a class-image boot carries no tenant
	// data by construction) or, for an intra-session resume, the
	// artifact's binding to the very sandbox that produced it.
	InvariantCleanBase Invariant = "clean-base"

	// InvariantUniquified (ADR-0005 #2): the restored guest verifiably
	// received a fresh CSPRNG reseed AND adopted a fresh identity
	// (machine-id, boot-id, hostname, CNI-assigned Pod IP, unique
	// vsock CID). Per-restore evidence: the node-agent's
	// entropy_reseeded and uniquified confirmations (setec#72,
	// setec#189).
	InvariantUniquified Invariant = "per-restore-uniquification"

	// InvariantSingleSession (ADR-0005 #3): the restored state serves
	// exactly one session — never reused across sessions or tenants.
	// Per-restore evidence: pool entries are consumed on claim (one
	// claim, then destroyed); a snapshot/checkpoint resume must target
	// the sandbox that produced the artifact.
	InvariantSingleSession Invariant = "one-session-then-destroy"

	// InvariantProvenance (ADR-0005 #4): the restored template was
	// built only from a trusted class image, never from a used
	// sandbox. Per-restore evidence: the node-agent's
	// provenance_verified confirmation (record read, class-image-boot
	// source, cryptographically bound into the sealed DEK, setec#190).
	InvariantProvenance Invariant = "template-provenance"

	// InvariantEncryptedAtRest (ADR-0005 #5): the restored state was
	// only ever persisted through the sealed-DEK encrypted path.
	// Per-restore evidence: the node-agent's encrypted_at_rest
	// confirmation (setec#190).
	InvariantEncryptedAtRest Invariant = "encrypted-at-rest"
)

// AllowUnverifiedRestoresAnnotation is the SandboxClass annotation
// half of the dev-mode opt-out. Its value must be exactly "true".
const AllowUnverifiedRestoresAnnotation = "setec.zeroroot.ai/allow-unverified-restores"

// DefaultAllowDevLabel is the namespace label key that carries the
// cluster operator's dev-mode consent — the SAME label the dev-only
// runtime (runc) admission gate consults, so one cluster-level switch
// governs every dev-only relaxation.
const DefaultAllowDevLabel = "setec.zeroroot.ai/allow-dev-runtimes"

// DefaultGateNamespace is the well-known namespace whose labels carry
// cluster-level dev-mode consent, mirroring the admission webhook's
// devGateNamespace.
const DefaultGateNamespace = "default"

// Evidence carries the per-restore verification signals for one
// restore/resume attempt. Every field is an affirmative confirmation
// reported by the mechanism that enforced it; the zero value is a
// fully-violating restore. Callers assemble it from the node-agent
// response plus the operator-side facts they are positioned to check.
type Evidence struct {
	// CleanBase confirms invariant 1 (see InvariantCleanBase).
	CleanBase bool

	// EntropyReseeded and IdentityUniquified together confirm
	// invariant 2; both must hold.
	EntropyReseeded    bool
	IdentityUniquified bool

	// SingleSession confirms invariant 3.
	SingleSession bool

	// ProvenanceVerified confirms invariant 4.
	ProvenanceVerified bool

	// EncryptedAtRest confirms invariant 5.
	EncryptedAtRest bool
}

// Evaluate returns the invariants the evidence fails to confirm, in
// stable order. An empty result means all five invariants are
// verified for this restore.
func Evaluate(ev Evidence) []Invariant {
	var out []Invariant
	if !ev.CleanBase {
		out = append(out, InvariantCleanBase)
	}
	if !ev.EntropyReseeded || !ev.IdentityUniquified {
		out = append(out, InvariantUniquified)
	}
	if !ev.SingleSession {
		out = append(out, InvariantSingleSession)
	}
	if !ev.ProvenanceVerified {
		out = append(out, InvariantProvenance)
	}
	if !ev.EncryptedAtRest {
		out = append(out, InvariantEncryptedAtRest)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Decision is the gate's answer for one restore attempt.
type Decision struct {
	// Allowed reports whether the restore may be handed to a caller.
	Allowed bool

	// Violations lists the invariants that failed verification —
	// populated even when Allowed is true under the dev opt-out, so
	// the caller can be loud about what it is serving.
	Violations []Invariant

	// DevOptOut reports that Allowed is true ONLY because the
	// dev-mode opt-out (class annotation + gate-namespace label) is
	// active.
	DevOptOut bool
}

// String renders the violation list for Events and errors.
func (d Decision) String() string {
	if len(d.Violations) == 0 {
		return "all ADR-0005 invariants verified"
	}
	parts := make([]string, len(d.Violations))
	for i, v := range d.Violations {
		parts[i] = string(v)
	}
	return "unverified ADR-0005 invariants: " + strings.Join(parts, ", ")
}

// Gate resolves the dev-mode opt-out for the invariant gate. A nil
// *Gate is valid and means "no opt-out is ever granted" — the gate
// enforces unconditionally, which is the fail-closed default for any
// caller that did not wire dev-mode support.
type Gate struct {
	// Reader fetches the gate namespace. Required for the opt-out to
	// ever be granted; nil fails closed.
	Reader client.Reader

	// AllowDevLabel overrides the namespace label key. Empty uses
	// DefaultAllowDevLabel.
	AllowDevLabel string

	// GateNamespace overrides the namespace consulted for the label.
	// Empty uses DefaultGateNamespace.
	GateNamespace string
}

func (g *Gate) allowDevLabel() string {
	if g != nil && g.AllowDevLabel != "" {
		return g.AllowDevLabel
	}
	return DefaultAllowDevLabel
}

func (g *Gate) gateNamespace() string {
	if g != nil && g.GateNamespace != "" {
		return g.GateNamespace
	}
	return DefaultGateNamespace
}

// ClassOptsOut reports whether the SandboxClass carries the
// unverified-restores annotation. This is only HALF the opt-out — the
// cluster-level dev label must also be present (DevModeActive).
func ClassOptsOut(cls *setecv1alpha1.SandboxClass) bool {
	if cls == nil {
		return false
	}
	return cls.Annotations[AllowUnverifiedRestoresAnnotation] == "true"
}

// DevModeActive reports whether the cluster-level dev consent label is
// present on the gate namespace. Any read error fails closed (false,
// with the error returned for logging).
func (g *Gate) DevModeActive(ctx context.Context) (bool, error) {
	if g == nil || g.Reader == nil {
		return false, nil
	}
	ns := &corev1.Namespace{}
	if err := g.Reader.Get(ctx, types.NamespacedName{Name: g.gateNamespace()}, ns); err != nil {
		return false, fmt.Errorf("gate: fetch dev-gate namespace %q (failing closed): %w", g.gateNamespace(), err)
	}
	return ns.Labels[g.allowDevLabel()] == "true", nil
}

// Decide is the single decision point: it evaluates the evidence and,
// when verification fails, consults the dev-mode opt-out. Outside
// dev-mode a single unverified invariant refuses the restore. The
// opt-out never hides violations — they are reported either way.
func (g *Gate) Decide(ctx context.Context, cls *setecv1alpha1.SandboxClass, ev Evidence) (Decision, error) {
	violations := Evaluate(ev)
	if len(violations) == 0 {
		return Decision{Allowed: true}, nil
	}
	if !ClassOptsOut(cls) {
		return Decision{Allowed: false, Violations: violations}, nil
	}
	dev, err := g.DevModeActive(ctx)
	if err != nil || !dev {
		// Annotation without the cluster-level label (or an unreadable
		// gate namespace) changes nothing: fail closed.
		return Decision{Allowed: false, Violations: violations}, err
	}
	return Decision{Allowed: true, Violations: violations, DevOptOut: true}, nil
}
