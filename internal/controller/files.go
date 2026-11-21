// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
	"github.com/zeroroot-ai/setec/internal/podspec"
	"github.com/zeroroot-ai/setec/internal/sandboxfiles"
)

// ensureFiles keeps the content of spec.files of a Sandbox in the Secret
// <name>-files (docs/design/files.md), with a manifest of the paths and
// modes. Only the launcher container mounts it. A Sandbox with no files gets
// no Secret. The files of a Sandbox never change, so a Secret that it owns is
// not written again. A Secret of an earlier Sandbox of the same name is
// replaced.
func (r *SandboxReconciler) ensureFiles(ctx context.Context, sb *setecv1alpha1.Sandbox) error {
	if len(sb.Spec.Files) == 0 {
		return nil
	}
	data, err := sandboxfiles.SecretData(sb.Spec.Files)
	if err != nil {
		return fmt.Errorf("make the files Secret data: %w", err)
	}
	key := types.NamespacedName{Namespace: sb.Namespace, Name: podspec.FilesSecretName(sb.Name)}
	sec := &corev1.Secret{}
	err = r.Get(ctx, key, sec)
	if err == nil && !metav1.IsControlledBy(sec, sb) {
		if derr := r.Delete(ctx, sec); derr != nil && !apierrors.IsNotFound(derr) {
			return fmt.Errorf("replace the files Secret of an earlier Sandbox: %w", derr)
		}
		err = apierrors.NewNotFound(corev1.Resource("secrets"), key.Name)
	}
	switch {
	case err == nil:
		// spec.files is immutable, so the Secret that the Sandbox owns holds its content.
		return nil
	case !apierrors.IsNotFound(err):
		return fmt.Errorf("get the files Secret: %w", err)
	}
	sec = &corev1.Secret{Type: corev1.SecretTypeOpaque, Data: data}
	sec.Name, sec.Namespace = key.Name, key.Namespace
	sec.Labels = map[string]string{podspec.SandboxLabelKey: sb.Name}
	if err := controllerutil.SetControllerReference(sb, sec, r.Scheme); err != nil {
		return fmt.Errorf("own the files Secret: %w", err)
	}
	if err := r.Create(ctx, sec); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create the files Secret: %w", err)
	}
	return nil
}
