// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package snapshot

import (
	"context"
	"crypto/rand"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
	"github.com/zeroroot-ai/setec/internal/snapshot/atrest"
)

// Kept snapshots (setec#196). A kept Snapshot is sealed with the key of its
// tenant: one Secret in the namespace of the owner pair. Deleting the
// tenant deletes the namespace, its Snapshots and the key, which
// crypto-erases each kept Snapshot.
const (
	// TenantKEKSecret names the Secret that holds the key of a tenant.
	TenantKEKSecret = "setec-tenant-kek"
	tenantKEKKey    = "kek"
	// KeptBackend is the store of kept Snapshots: the S3-compatible store
	// of session checkpoints, which a node that never held a Snapshot can
	// read.
	KeptBackend = "s3"
	// DefaultKeptTTL is how long a kept Snapshot lives unless pinned.
	DefaultKeptTTL = 30 * 24 * time.Hour
)

// TenantKEK returns the key of the tenant of namespace ns, and makes it on
// the first call.
func (c *Coordinator) TenantKEK(ctx context.Context, ns string) ([]byte, error) {
	secret := &corev1.Secret{}
	err := c.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: TenantKEKSecret}, secret)
	if err == nil {
		if k := secret.Data[tenantKEKKey]; len(k) == atrest.KeySize {
			return k, nil
		}
		return nil, fmt.Errorf("coordinator: the tenant key in %s/%s has the wrong size", ns, TenantKEKSecret)
	}
	if !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("coordinator: read the tenant key: %w", err)
	}
	key := make([]byte, atrest.KeySize)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	secret = &corev1.Secret{
		Namespace: ns, Name: TenantKEKSecret,
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{tenantKEKKey: key},
	}
	if err := c.Client.Create(ctx, secret); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return c.TenantKEK(ctx, ns)
		}
		return nil, fmt.Errorf("coordinator: make the tenant key: %w", err)
	}
	return key, nil
}

// isReviewOf reports whether sb is a review Sandbox that may load the kept
// Snapshot snap: same namespace, so same owner pair.
func isReviewOf(sb *setecv1alpha1.Sandbox, snap *setecv1alpha1.Snapshot) bool {
	return snap.Spec.Kept && sb.Spec.Review && snap.Namespace == sb.Namespace
}
