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

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
)

// SandboxClassReconciler watches SandboxClass resources. Phase 2 does not
// derive any status or mutate any child resources — the resolver reads
// classes fresh on every Sandbox reconcile. The controller exists so the
// cluster's SandboxClass watch is wired (and so future phases can start
// populating status.observedGeneration, image-prefetch tracking, etc.
// without restructuring the manager wiring).
type SandboxClassReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// Reconcile is a no-op beyond logging. Phase 2 does not require any
// state to converge on a SandboxClass; admins own the lifecycle.
func (r *SandboxClassReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log.FromContext(ctx).V(1).Info("observed SandboxClass event", "class", req.Name)
	return ctrl.Result{}, nil
}

// SetupWithManager registers the reconciler with the given manager. It
// watches cluster-scoped SandboxClass resources only.
func (r *SandboxClassReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&setecv1alpha1.SandboxClass{}).
		Complete(r)
}
