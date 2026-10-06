// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
	"github.com/zeroroot-ai/setec/internal/errwrap"
)

// Each pool image of a class is checked before the pool builds a base of
// it: a Job of the disk builder verifies the cosign signature of the image
// against spec.preWarmImageSignature.
const (
	verifyJobPrefix = "setec-verify-"
	// The reasons of the ImageNotVerified condition.
	reasonImageVerified    = "Verified"
	reasonImageNotVerified = "SignatureNotVerified"
	reasonImageNoSigner    = "NoSigner"
)

// verifyState is the result of the signature check of a pool image.
type verifyState int

const (
	verifyPending verifyState = iota
	verifyPassed
	verifyFailed
)

// verifyJobName names the check of one image against one signer for one
// class, so a change of the image or of the signer runs a new check.
func verifyJobName(cls *setecv1alpha1.SandboxClass, image string) string {
	h := sha256.New()
	sig := cls.Spec.PreWarmImageSignature
	for _, part := range []string{cls.Name, image, sig.Issuer, sig.Identity, sig.PublicKey} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return verifyJobPrefix + hex.EncodeToString(h.Sum(nil))[:20]
}

// verifyJob is the Job that checks the signature of a pool image of cls.
func verifyJob(cfg DiskBuilderConfig, name string, cls *setecv1alpha1.SandboxClass, image string) *batchv1.Job {
	sig := cls.Spec.PreWarmImageSignature
	args := []string{"--temp-dir", builderWorkDir}
	var env []corev1.EnvVar
	if sig.PublicKey != "" {
		env = append(env, corev1.EnvVar{Name: "SETEC_VERIFY_PUBLIC_KEY", Value: sig.PublicKey})
	} else {
		args = append(args, "--issuer", sig.Issuer, "--identity", sig.Identity)
	}
	args = append(args, "verify", image)
	return builderJob(cfg, name, "image-verify", args, env, false)
}

// ensureImageVerified starts or reads the signature check of a pool image
// of cls. A class with no signer fails: the pool builds no base from an
// image it cannot check.
func (r *WarmPoolReconciler) ensureImageVerified(
	ctx context.Context, cls *setecv1alpha1.SandboxClass, image string,
) (verifyState, string, error) {
	if cls.Spec.PreWarmImageSignature == nil {
		return verifyFailed, "the class names no signer of its pool image (spec.preWarmImageSignature)", nil
	}
	cfg := r.DiskBuilder
	name := verifyJobName(cls, image)
	reader := cfg.Reader
	if reader == nil {
		reader = r.Client
	}
	job := &batchv1.Job{}
	err := reader.Get(ctx, types.NamespacedName{Namespace: cfg.Namespace, Name: name}, job)
	if apierrors.IsNotFound(err) {
		if err := r.Create(ctx, verifyJob(cfg, name, cls, image)); err != nil && !apierrors.IsAlreadyExists(err) {
			return verifyPending, "", fmt.Errorf("create the image check Job %s: %w", name, err)
		}
		return verifyPending, "", nil
	}
	if err != nil {
		return verifyPending, "", errwrap.Wrap(err, "client.Reader.Get")
	}
	for _, c := range job.Status.Conditions {
		if c.Status != corev1.ConditionTrue {
			continue
		}
		switch c.Type {
		case batchv1.JobComplete:
			return verifyPassed, "", nil
		case batchv1.JobFailed:
			return verifyFailed, fmt.Sprintf("the image check Job %s failed: %s", name, c.Message), nil
		case batchv1.JobSuspended, batchv1.JobFailureTarget, batchv1.JobSuccessCriteriaMet:
		}
	}
	return verifyPending, "", nil
}

// setImageCondition writes the ImageNotVerified condition of cls.
func (r *WarmPoolReconciler) setImageCondition(ctx context.Context, cls *setecv1alpha1.SandboxClass, notVerified bool, reason, msg string) error {
	status := metav1.ConditionFalse
	if notVerified {
		status = metav1.ConditionTrue
	}
	orig := cls.DeepCopy()
	if !meta.SetStatusCondition(&cls.Status.Conditions, metav1.Condition{
		Type: setecv1alpha1.ConditionImageNotVerified, Status: status, Reason: reason, Message: msg,
		ObservedGeneration: cls.Generation,
	}) {
		return nil
	}
	return errwrap.Wrap(r.Status().Patch(ctx, cls, client.MergeFrom(orig)), "client.SubResourceWriter.Patch")
}

// checkImages checks the signature of each pool image and returns the
// state of each. The class gets the condition ImageNotVerified, True with
// the images that failed, or False once each image passed.
func (r *WarmPoolReconciler) checkImages(
	ctx context.Context, cls *setecv1alpha1.SandboxClass, images []setecv1alpha1.SandboxClassWarmPoolImage,
) (map[string]verifyState, error) {
	states := make(map[string]verifyState, len(images))
	var failed []string
	pending := false
	for _, im := range images {
		state, msg, err := r.ensureImageVerified(ctx, cls, im.Image)
		if err != nil {
			return nil, err
		}
		states[im.Image] = state
		switch state {
		case verifyFailed:
			failed = append(failed, msg)
		case verifyPending:
			pending = true
		case verifyPassed:
		}
	}
	switch {
	case len(failed) > 0:
		reason := reasonImageNotVerified
		if cls.Spec.PreWarmImageSignature == nil {
			reason = reasonImageNoSigner
		}
		return states, r.setImageCondition(ctx, cls, true, reason, strings.Join(failed, "; "))
	case !pending && len(images) > 0:
		return states, r.setImageCondition(ctx, cls, false, reasonImageVerified,
			"each pool image has a signature of the named signer")
	}
	return states, nil
}
