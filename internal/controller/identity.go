// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package controller

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
	"github.com/zeroroot-ai/setec/internal/podspec"
	"github.com/zeroroot-ai/setec/internal/sandboxid"
	"github.com/zeroroot-ai/setec/internal/tenancy"
)

// sandboxID is the <namespace>/<name>/<uid> of a Sandbox, as the frontend
// returns it.
func sandboxID(sb *setecv1alpha1.Sandbox) string {
	return sb.Namespace + "/" + sb.Name + "/" + string(sb.UID)
}

// ensureIdentity gives a launcher Sandbox its identity (setec#235): a new
// ed25519 key in the Secret <name>-identity, which only the launcher
// container mounts, and the public key and the generation in the status.
// Each Sandbox, so each fork, gets its own key. A Secret of an earlier
// Sandbox of the same name is replaced.
func (r *SandboxReconciler) ensureIdentity(ctx context.Context, sb *setecv1alpha1.Sandbox) (*podspec.LauncherIdentity, error) {
	key := types.NamespacedName{Namespace: sb.Namespace, Name: podspec.IdentitySecretName(sb.Name)}
	sec := &corev1.Secret{}
	err := r.Get(ctx, key, sec)
	if err == nil && !metav1.IsControlledBy(sec, sb) {
		if derr := r.Delete(ctx, sec); derr != nil && !apierrors.IsNotFound(derr) {
			return nil, fmt.Errorf("replace the identity Secret of an earlier Sandbox: %w", derr)
		}
		err = apierrors.NewNotFound(corev1.Resource("secrets"), key.Name)
	}
	if apierrors.IsNotFound(err) {
		if sec, err = r.createIdentitySecret(ctx, sb, key); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, fmt.Errorf("get the identity Secret: %w", err)
	}
	priv, err := sandboxid.KeyFromSeed(string(sec.Data[podspec.IdentitySecretKey]))
	if err != nil {
		return nil, err
	}
	pub := base64.StdEncoding.EncodeToString(priv.Public().(ed25519.PublicKey))
	if id := sb.Status.Identity; id == nil || id.PublicKey != pub {
		original := sb.DeepCopy()
		gen := int64(1)
		if id != nil && id.Generation > gen {
			gen = id.Generation
		}
		sb.Status.Identity = &setecv1alpha1.SandboxIdentityStatus{PublicKey: pub, Generation: gen}
		if err := r.Status().Patch(ctx, sb, client.MergeFrom(original)); err != nil {
			return nil, fmt.Errorf("record the identity: %w", err)
		}
	}
	ns := &corev1.Namespace{}
	if err := r.Get(ctx, types.NamespacedName{Name: sb.Namespace}, ns); err != nil {
		return nil, fmt.Errorf("get the namespace of the Sandbox: %w", err)
	}
	return &podspec.LauncherIdentity{
		SandboxID:  sandboxID(sb),
		Client:     ns.Labels[tenancy.ClientLabelKey],
		Tenant:     ns.Labels[tenancy.TenantLabelKey],
		Generation: sb.Status.Identity.Generation,
	}, nil
}

func (r *SandboxReconciler) createIdentitySecret(
	ctx context.Context, sb *setecv1alpha1.Sandbox, key types.NamespacedName,
) (*corev1.Secret, error) {
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		return nil, fmt.Errorf("make an identity key: %w", err)
	}
	sec := &corev1.Secret{
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{podspec.IdentitySecretKey: []byte(base64.StdEncoding.EncodeToString(seed))},
	}
	sec.Name, sec.Namespace = key.Name, key.Namespace
	sec.Labels = map[string]string{podspec.SandboxLabelKey: sb.Name}
	if err := controllerutil.SetControllerReference(sb, sec, r.Scheme); err != nil {
		return nil, fmt.Errorf("own the identity Secret: %w", err)
	}
	if err := r.Create(ctx, sec); err != nil {
		return nil, fmt.Errorf("create the identity Secret: %w", err)
	}
	return sec, nil
}
