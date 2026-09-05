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

package controller

import (
	"context"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
	"github.com/zeroroot-ai/setec/internal/snapshot/gate"
)

// ConditionUnverifiedRestoresAllowed is the SandboxClass condition
// type that makes the ADR-0005 dev-mode opt-out loud: it is True only
// while the class annotation AND the cluster-level dev gate label are
// both present, i.e. while the invariant gate may serve unverified
// restores for this class.
const ConditionUnverifiedRestoresAllowed = "UnverifiedRestoresAllowed"

// Condition reasons for ConditionUnverifiedRestoresAllowed.
const (
	// ReasonDevModeOptOut: both opt-out halves are present; the gate
	// may serve unverified restores for this class. DEV ONLY.
	ReasonDevModeOptOut = "DevModeOptOut"
	// ReasonDevGateNamespaceUnlabelled: the class asks for the opt-out
	// but the cluster-level dev label is absent, so the gate still
	// enforces. The annotation is inert — surfaced so the mismatch is
	// visible instead of silently ignored.
	ReasonDevGateNamespaceUnlabelled = "DevGateNamespaceUnlabelled"
	// ReasonEnforced: no opt-out requested; the ADR-0005 invariant
	// gate enforces all five invariant verifications per restore.
	ReasonEnforced = "Enforced"
)

// SandboxClassReconciler watches SandboxClass resources and keeps the
// ADR-0005 dev-mode opt-out condition (UnverifiedRestoresAllowed)
// truthful on every class. Class resolution itself stays read-fresh in
// the resolver; this controller only owns the loud status surface.
type SandboxClassReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// Gate resolves the cluster-level half of the dev-mode opt-out
	// (the dev label on the gate namespace). Nil means the opt-out can
	// never be active — the condition is stamped accordingly.
	Gate *gate.Gate
}

// +kubebuilder:rbac:groups=setec.zeroroot.ai,resources=sandboxclasses/status,verbs=get;update;patch

// Reconcile derives the UnverifiedRestoresAllowed condition from the
// class annotation and the cluster-level dev gate label, and patches
// the class status when the observed truth changed. An active opt-out
// is additionally logged at high visibility on every reconcile of the
// class — dev-mode is an auditable exception, never a quiet default.
func (r *SandboxClassReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	cls := &setecv1alpha1.SandboxClass{}
	if err := r.Get(ctx, req.NamespacedName, cls); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	cond := metav1.Condition{
		Type:               ConditionUnverifiedRestoresAllowed,
		Status:             metav1.ConditionFalse,
		Reason:             ReasonEnforced,
		Message:            "ADR-0005 invariant gate enforced: every restore/resume requires all five invariant verifications",
		ObservedGeneration: cls.Generation,
	}
	if gate.ClassOptsOut(cls) {
		dev, err := r.Gate.DevModeActive(ctx)
		switch {
		case err != nil:
			// Unreadable gate namespace fails closed — the gate will
			// not honour the opt-out either. Surface why.
			cond.Reason = ReasonDevGateNamespaceUnlabelled
			cond.Message = "opt-out annotation present but the dev gate namespace could not be read (failing closed): " + err.Error()
		case !dev:
			cond.Reason = ReasonDevGateNamespaceUnlabelled
			cond.Message = "opt-out annotation present but cluster-level dev consent is absent; the invariant gate still enforces (label the gate namespace with " +
				gate.DefaultAllowDevLabel + "=true to activate the DEV-ONLY opt-out)"
		default:
			cond.Status = metav1.ConditionTrue
			cond.Reason = ReasonDevModeOptOut
			cond.Message = "DEV-MODE: the ADR-0005 invariant gate may serve UNVERIFIED restores for this class (" +
				gate.AllowUnverifiedRestoresAnnotation + "=\"true\" + cluster dev label). Never use in production."
			logger.Info("SECURITY: ADR-0005 invariant-gate dev-mode opt-out ACTIVE — unverified snapshot restores may be served",
				"class", cls.Name,
				"annotation", gate.AllowUnverifiedRestoresAnnotation,
				"devLabel", gate.DefaultAllowDevLabel)
		}
	}

	orig := cls.DeepCopy()
	if !meta.SetStatusCondition(&cls.Status.Conditions, cond) {
		return ctrl.Result{}, nil
	}
	if err := r.Status().Patch(ctx, cls, client.MergeFrom(orig)); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// SetupWithManager registers the reconciler with the given manager. It
// watches cluster-scoped SandboxClass resources only.
func (r *SandboxClassReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&setecv1alpha1.SandboxClass{}).
		Complete(r)
}
