// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package frontend

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"sync"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/zeroroot-ai/setec/internal/tenancy"
)

const (
	// SandboxNamespaceLabel marks a namespace that holds Sandboxes only.
	// The host guard and the scope policy of the frontend bind to it.
	SandboxNamespaceLabel = "setec.zeroroot.ai/sandbox-namespace"

	// PairNamespacePrefix starts the name of each namespace that the
	// frontend makes for a pair.
	PairNamespacePrefix = "sbx-"
)

// PairNamespaceName is the one namespace name of a pair: the prefix and the
// first 20 hex digits of the SHA-256 of "client/tenant". The name is
// fixed by the pair, so a pair never gets two namespaces, and a race of two
// frontends ends on one object.
func PairNamespaceName(p tenancy.Pair) string {
	sum := sha256.Sum256([]byte(p.Client + "/" + string(p.Tenant)))
	return PairNamespacePrefix + hex.EncodeToString(sum[:])[:20]
}

// RoleGrant is one RoleBinding that each pair namespace gets: a ClusterRole
// bound to one ServiceAccount inside that namespace only.
type RoleGrant struct {
	ClusterRole    string
	ServiceAccount types.NamespacedName
}

// NamespaceProvisioner is the PairResolver of the frontend
// (docs/design/isolation.md). On the first call of a pair it makes the
// namespace of the pair, with the pair labels and the sandbox namespace
// label, and the RoleBindings that let the operator write Pods and the
// frontend enter them there. The operator writes the default-deny policy
// of the namespace before its first Pod.
//
// A namespace with the name of the pair and different labels is refused,
// never adopted.
type NamespaceProvisioner struct {
	Client client.Client
	Grants []RoleGrant

	mu    sync.Mutex
	ready map[string]struct{}
}

// NamespaceFor returns the namespace of p, and makes it on the first call.
func (n *NamespaceProvisioner) NamespaceFor(ctx context.Context, p tenancy.Pair) (string, error) {
	name := PairNamespaceName(p)
	n.mu.Lock()
	_, done := n.ready[name]
	n.mu.Unlock()
	if done {
		return name, nil
	}

	want := maps.Clone(p.Labels())
	want[SandboxNamespaceLabel] = "true"
	ns := &corev1.Namespace{}
	err := n.Client.Get(ctx, types.NamespacedName{Name: name}, ns)
	if apierrors.IsNotFound(err) {
		ns = &corev1.Namespace{Name: name, Labels: want}
		err = n.Client.Create(ctx, ns)
		if apierrors.IsAlreadyExists(err) {
			err = n.Client.Get(ctx, types.NamespacedName{Name: name}, ns)
		}
	}
	if err != nil {
		return "", fmt.Errorf("namespace %s of pair %s: %w", name, p, err)
	}
	if !p.IsOwnerOf(ns.Labels) || ns.Labels[SandboxNamespaceLabel] != "true" {
		return "", fmt.Errorf("namespace %s exists with labels %v; it is not the namespace of pair %s",
			name, ns.Labels, p.String())
	}
	if ns.DeletionTimestamp != nil {
		return "", fmt.Errorf("namespace %s of pair %s is being deleted", name, p)
	}
	for _, g := range n.Grants {
		if err := n.ensureGrant(ctx, name, g); err != nil {
			return "", err
		}
	}

	n.mu.Lock()
	if n.ready == nil {
		n.ready = map[string]struct{}{}
	}
	n.ready[name] = struct{}{}
	n.mu.Unlock()
	return name, nil
}

func (n *NamespaceProvisioner) ensureGrant(ctx context.Context, ns string, g RoleGrant) error {
	if g.ClusterRole == "" || g.ServiceAccount.Name == "" || g.ServiceAccount.Namespace == "" {
		return errors.New("a role grant of the namespace provisioner is incomplete")
	}
	rb := &rbacv1.RoleBinding{
		Name:      g.ClusterRole,
		Namespace: ns,
		RoleRef:   rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: g.ClusterRole},
		Subjects: []rbacv1.Subject{{
			Kind: rbacv1.ServiceAccountKind, Name: g.ServiceAccount.Name, Namespace: g.ServiceAccount.Namespace,
		}},
	}
	if err := n.Client.Create(ctx, rb); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("role binding %s in %s: %w", g.ClusterRole, ns, err)
	}
	return nil
}
