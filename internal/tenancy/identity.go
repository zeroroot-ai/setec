// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

// Package tenancy owns the tenant of a Sandbox. A TenantID comes from a
// Kubernetes namespace label or from the tenant field of a frontend request.
// It never comes from a certificate: a certificate names a workload, not a
// tenant (ADR-0142). A Pair joins the enrolled client cluster of the caller
// and the tenant, and it selects the namespace of the Sandbox. Every value is
// validated to be safe as a DNS-1123 label.
//
// This package has no controller-runtime or client-go imports: the
// controller and frontend both compose it but all I/O stays outside.
package tenancy

import (
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation"
)

// TenantID is an opaque identifier for a tenant. Callers MUST NOT depend on
// any particular encoding; the type exists to prevent accidental mixing of
// tenant IDs with arbitrary strings.
type TenantID string

// String renders the TenantID as a plain string. Use sparingly — prefer
// passing TenantID values by type so the compiler enforces tenant boundaries.
func (t TenantID) String() string { return string(t) }

// Sentinel errors. Callers classify failures via errors.Is to drive
// admission-layer messages or gRPC status codes (PERMISSION_DENIED vs.
// UNAUTHENTICATED vs. INVALID_ARGUMENT).
var (
	// ErrTenantLabelMissing is returned when a namespace has no value for
	// the configured tenant-label key, or the value is empty.
	ErrTenantLabelMissing = errors.New("tenancy: namespace is missing tenant label")

	// ErrTenantInvalid is returned when the extracted tenant value does
	// not conform to DNS-1123 label syntax, which means it cannot safely
	// be used as a Kubernetes label value.
	ErrTenantInvalid = errors.New("tenancy: tenant identity is not a valid DNS label")
)

// FromNamespace extracts the tenant identity from a Kubernetes namespace
// by reading the given label key. Returns ErrTenantLabelMissing if the
// label is absent or empty, and ErrTenantInvalid if the value is not a
// valid DNS-1123 label.
//
// The function never returns sensitive data in its error messages —
// namespace names and label keys are the only fields echoed back, which is
// already public cluster metadata.
func FromNamespace(ns *corev1.Namespace, labelKey string) (TenantID, error) {
	if ns == nil {
		return "", fmt.Errorf("%w: namespace is nil", ErrTenantLabelMissing)
	}
	if labelKey == "" {
		return "", fmt.Errorf("%w: label key is empty", ErrTenantLabelMissing)
	}
	value, ok := ns.Labels[labelKey]
	if !ok || value == "" {
		return "", fmt.Errorf("%w: namespace %q has no %q label",
			ErrTenantLabelMissing, ns.Name, labelKey)
	}
	if errs := validation.IsDNS1123Label(value); len(errs) != 0 {
		return "", fmt.Errorf("%w: namespace %q label %q value fails validation",
			ErrTenantInvalid, ns.Name, labelKey)
	}
	return TenantID(value), nil
}

// Label keys that record the owner of a Sandbox namespace and of a Sandbox.
const (
	// ClientLabelKey names the enrolled client cluster.
	ClientLabelKey = "setec.zeroroot.ai/client"
	// TenantLabelKey names the tenant inside that client cluster. The
	// operator reads the same key on a namespace (--tenant-label-key).
	TenantLabelKey = "setec.zeroroot.ai/tenant"
)

// Pair is the owner of a Sandbox: the enrolled client cluster and the tenant
// that the client named in the request. Two pairs never share a namespace.
type Pair struct {
	Client string
	Tenant TenantID
}

// NewPair validates both parts as DNS-1123 labels. An empty part is refused.
func NewPair(client, tenant string) (Pair, error) {
	for _, part := range []struct{ name, value string }{{"client", client}, {"tenant", tenant}} {
		if part.value == "" {
			return Pair{}, fmt.Errorf("%w: the %s is empty", ErrTenantInvalid, part.name)
		}
		if errs := validation.IsDNS1123Label(part.value); len(errs) != 0 {
			return Pair{}, fmt.Errorf("%w: the %s %q is not a DNS label", ErrTenantInvalid, part.name, part.value)
		}
	}
	return Pair{Client: client, Tenant: TenantID(tenant)}, nil
}

// String renders the pair as client/tenant, for messages.
func (p Pair) String() string { return p.Client + "/" + string(p.Tenant) }

// IsOwnerOf reports whether labels record this pair as the owner.
func (p Pair) IsOwnerOf(labels map[string]string) bool {
	return labels[ClientLabelKey] == p.Client && labels[TenantLabelKey] == string(p.Tenant)
}

// Labels returns the two labels that record the pair on an object.
func (p Pair) Labels() map[string]string {
	return map[string]string{ClientLabelKey: p.Client, TenantLabelKey: string(p.Tenant)}
}
