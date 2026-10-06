// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

// Package limits holds the disk limits of a Sandbox (ADR-0146): the scratch
// size and the ephemeral-storage limit of its Pod.
package limits

import (
	"k8s.io/apimachinery/pkg/api/resource"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
)

// The disk limits of a Sandbox (ADR-0146). A SandboxClass changes the
// scratch values; the headroom is fixed.
var (
	// DefaultScratch is the scratch size limit when neither the Sandbox nor
	// its class names one. It is also the scratch ceiling of a class that
	// sets no maxResources.scratch.
	DefaultScratch = resource.MustParse("10Gi")

	// EphemeralHeadroom is what the Pod may write outside the scratch
	// volume (container logs, the writable container layer). The Pod's
	// ephemeral-storage limit is the scratch size plus this value.
	EphemeralHeadroom = resource.MustParse("1Gi")
)

// ScratchCeiling is the largest scratch size a Sandbox of cls may ask for:
// maxResources.scratch, else DefaultScratch. A nil class gets
// DefaultScratch.
func ScratchCeiling(cls *setecv1alpha1.SandboxClass) resource.Quantity {
	if cls != nil && cls.Spec.MaxResources != nil && cls.Spec.MaxResources.Scratch != nil {
		return cls.Spec.MaxResources.Scratch.DeepCopy()
	}
	return DefaultScratch.DeepCopy()
}

// EffectiveScratch is the scratch size limit of sb: its own value, else the
// class default (defaultResources.scratch), else DefaultScratch.
func EffectiveScratch(sb *setecv1alpha1.Sandbox, cls *setecv1alpha1.SandboxClass) resource.Quantity {
	if sb != nil && sb.Spec.Resources.Scratch != nil {
		return sb.Spec.Resources.Scratch.DeepCopy()
	}
	if cls != nil && cls.Spec.DefaultResources != nil && cls.Spec.DefaultResources.Scratch != nil {
		return cls.Spec.DefaultResources.Scratch.DeepCopy()
	}
	return DefaultScratch.DeepCopy()
}

// EphemeralLimit is the ephemeral-storage limit of a Pod whose scratch size
// limit is scratch.
func EphemeralLimit(scratch resource.Quantity) resource.Quantity {
	out := scratch.DeepCopy()
	out.Add(EphemeralHeadroom)
	return out
}
