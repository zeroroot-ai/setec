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

// Package webhook owns the validating admission webhook that enforces
// SandboxClass constraints at kubectl-apply time. The webhook reuses the
// pure internal/class.Validator so admission and reconcile agree on what
// "valid" means — there is exactly one source of truth for constraint
// checks.
package webhook

import (
	"context"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
	"github.com/zeroroot-ai/setec/internal/class"
	"github.com/zeroroot-ai/setec/internal/snapshot"
	"github.com/zeroroot-ai/setec/internal/tenancy"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

// shimNamespace bridges the NamespaceGetter label-map return to the
// corev1.Namespace shape tenancy.FromNamespace wants. Kept private
// because its only purpose is to avoid requiring the getter to return a
// full corev1.Namespace object.
type shimNamespace struct {
	name   string
	labels map[string]string
}

// toCoreNamespace materialises the minimal corev1.Namespace needed by
// tenancy.FromNamespace. All other Namespace fields are irrelevant to
// tenancy identity extraction.
func (s *shimNamespace) toCoreNamespace() *corev1.Namespace {
	return &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: s.name, Labels: s.labels},
	}
}

// +kubebuilder:webhook:path=/validate-setec-zeroroot-ai-v1alpha1-sandbox,mutating=false,failurePolicy=fail,sideEffects=None,groups=setec.zeroroot.ai,resources=sandboxes,verbs=create;update,versions=v1alpha1,name=vsandbox.setec.zeroroot.ai,admissionReviewVersions=v1

// SandboxValidator is the controller-runtime CustomValidator for
// v1alpha1.Sandbox. It rejects Sandboxes that either fail SandboxClass
// constraint checks (resource ceilings, network modes) or, when
// multi-tenancy is enabled, lack a namespace tenant label.
type SandboxValidator struct {
	// Resolver maps a Sandbox to its effective SandboxClass. Required.
	Resolver *class.Resolver

	// MultiTenancyEnabled mirrors the controller flag; when true the
	// webhook rejects Sandboxes in namespaces without a tenant label.
	MultiTenancyEnabled bool

	// TenantLabelKey is the label key checked on the namespace. Only
	// consulted when MultiTenancyEnabled.
	TenantLabelKey string

	// NamespaceGetter is the hook for namespace lookup used by the
	// multi-tenancy enforcement path. Tests inject a stub; the
	// operator wires ClientNamespaceGetter backed by the manager's
	// client. Required when MultiTenancyEnabled is true — the
	// validator fails closed rather than silently skipping the tenant
	// check if the getter is nil.
	NamespaceGetter NamespaceGetter

	// Client is a controller-runtime reader used by Phase 3 admission
	// rules to resolve Snapshot references. When nil the snapshotRef
	// checks are skipped (defense in depth: the controller itself
	// still validates before pod creation).
	Client client.Reader
}

// NamespaceGetter abstracts the dependency on a Kubernetes client for
// namespace reads so tests can inject a fake without pulling in the full
// controller-runtime manager.
type NamespaceGetter interface {
	// GetNamespaceLabels returns the label map for the named namespace,
	// or a NotFound error if the namespace does not exist.
	GetNamespaceLabels(ctx context.Context, name string) (map[string]string, error)
}

// ClientNamespaceGetter is the production NamespaceGetter. It reads
// the namespace through the controller-runtime manager's cached
// client, which is what cmd/main.go wires at startup when the
// validating webhook is enabled.
type ClientNamespaceGetter struct {
	Client client.Reader
}

// GetNamespaceLabels returns the Namespace's label map. A NotFound
// error bubbles up unchanged so the validator can produce a clear
// "namespace not found" remediation message.
func (g *ClientNamespaceGetter) GetNamespaceLabels(ctx context.Context, name string) (map[string]string, error) {
	if g == nil || g.Client == nil {
		return nil, fmt.Errorf("ClientNamespaceGetter: client is nil")
	}
	ns := &corev1.Namespace{}
	if err := g.Client.Get(ctx, client.ObjectKey{Name: name}, ns); err != nil {
		return nil, err
	}
	return ns.Labels, nil
}

// Ensure interface satisfaction at compile time so a broken refactor
// produces a build error, not a runtime admission failure.
var _ admission.Validator[*setecv1alpha1.Sandbox] = (*SandboxValidator)(nil)

// ValidateCreate runs at Sandbox creation. Returns an aggregated error
// enumerating every violation; the admission server surfaces it to the
// user as the rejection message.
func (v *SandboxValidator) ValidateCreate(ctx context.Context, obj *setecv1alpha1.Sandbox) (admission.Warnings, error) {
	return v.validate(ctx, obj)
}

// ValidateUpdate reuses the create logic: any Sandbox mutation that leaves
// the spec violating class constraints is just as bad as creation.
func (v *SandboxValidator) ValidateUpdate(ctx context.Context, _, newObj *setecv1alpha1.Sandbox) (admission.Warnings, error) {
	return v.validate(ctx, newObj)
}

// ValidateDelete is a no-op. Deletion is always permitted; garbage
// collection of owned resources is handled by the controller's
// OwnerReference plumbing.
func (v *SandboxValidator) ValidateDelete(_ context.Context, _ *setecv1alpha1.Sandbox) (admission.Warnings, error) {
	return nil, nil
}

// validate is the shared create/update path. It collects every applicable
// error and returns a single aggregate so users see the whole list at
// once rather than playing whack-a-mole against one error per apply.
func (v *SandboxValidator) validate(ctx context.Context, sb *setecv1alpha1.Sandbox) (admission.Warnings, error) {
	var errs []error

	// (1) Tenant-label enforcement when multi-tenancy is enabled.
	// Fail closed: a nil NamespaceGetter means mis-wired production
	// and the webhook refuses the Sandbox rather than silently
	// skipping the check.
	if v.MultiTenancyEnabled && v.TenantLabelKey != "" {
		if v.NamespaceGetter == nil {
			return nil, fmt.Errorf(
				"webhook: multi-tenancy enabled but NamespaceGetter not configured; refusing Sandbox to fail closed")
		}
		if err := v.checkTenantLabel(ctx, sb); err != nil {
			errs = append(errs, err)
		}
	}

	// (2) SandboxClass resolution + constraint validation.
	cls, err := v.Resolver.Resolve(ctx, sb)
	switch {
	case errors.Is(err, class.ErrClassNotFound):
		errs = append(errs, fmt.Errorf(
			"SandboxClass %q not found", sb.Spec.SandboxClassName))
	case errors.Is(err, class.ErrNoDefaultClass):
		// No default class is only fatal when multi-tenancy is
		// enabled or the Sandbox omitted a class name explicitly.
		// Phase 1 back-compat: a Sandbox with no class name in a
		// single-tenant cluster without any SandboxClass must still
		// be admitted.
		if v.MultiTenancyEnabled || sb.Spec.SandboxClassName != "" {
			errs = append(errs, fmt.Errorf(
				"no default SandboxClass configured and Sandbox did not specify sandboxClassName"))
		}
	case errors.Is(err, class.ErrAmbiguousDefault):
		errs = append(errs, fmt.Errorf(
			"multiple SandboxClasses marked default:true; administrator must resolve ambiguity"))
	case err != nil:
		// Unexpected error (e.g., API server down). Return directly
		// so the admission controller can apply failurePolicy.
		return nil, fmt.Errorf("webhook: resolve SandboxClass: %w", err)
	default:
		// Class resolved cleanly: run the pure validator.
		for _, vio := range class.Validate(sb, cls) {
			errs = append(errs, errors.New(vio.String()))
		}
	}

	// (3) Phase 3: snapshotRef admission. Reject Sandboxes whose
	// snapshot reference resolves to a different namespace, does not
	// exist, or is incompatible with the resolved class.
	if sb.Spec.SnapshotRef != nil && sb.Spec.SnapshotRef.Name != "" && v.Client != nil {
		snap := &setecv1alpha1.Snapshot{}
		getErr := v.Client.Get(ctx, client.ObjectKey{
			Namespace: sb.Namespace,
			Name:      sb.Spec.SnapshotRef.Name,
		}, snap)
		switch {
		case apierrors.IsNotFound(getErr):
			errs = append(errs, fmt.Errorf(
				"snapshot %q not found in namespace %q",
				sb.Spec.SnapshotRef.Name, sb.Namespace))
		case getErr != nil:
			return nil, fmt.Errorf("webhook: resolve Snapshot: %w", getErr)
		default:
			for _, vio := range snapshot.Validate(sb, snap, cls) {
				errs = append(errs, errors.New(vio.String()))
			}
		}
	}

	if len(errs) == 0 {
		return nil, nil
	}
	return nil, utilerrors.NewAggregate(errs)
}

// checkTenantLabel reads the Sandbox's namespace and confirms the
// configured tenant label is present and valid.
func (v *SandboxValidator) checkTenantLabel(ctx context.Context, sb *setecv1alpha1.Sandbox) error {
	labels, err := v.NamespaceGetter.GetNamespaceLabels(ctx, sb.Namespace)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// Namespace disappearing mid-admission is a rare
			// race; surface a clear message.
			return fmt.Errorf("namespace %q not found", sb.Namespace)
		}
		return fmt.Errorf("webhook: get namespace %q: %w", sb.Namespace, err)
	}
	// Construct a minimal namespace shim so we can reuse
	// tenancy.FromNamespace without depending on a Get returning the
	// whole object.
	ns := &shimNamespace{name: sb.Namespace, labels: labels}
	if _, err := tenancy.FromNamespace(ns.toCoreNamespace(), v.TenantLabelKey); err != nil {
		return fmt.Errorf("tenant label %q required on namespace %q; remediation: apply the label or disable multi-tenancy",
			v.TenantLabelKey, sb.Namespace)
	}
	return nil
}

// SetupWebhookWithManager registers the Sandbox validating webhook with
// the controller-runtime manager. Callers invoke it from cmd/main.go.
func (v *SandboxValidator) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &setecv1alpha1.Sandbox{}).
		WithValidator(v).
		Complete()
}

// assert the webhook type exists as referenced by docs and tests.
var _ = webhook.Admission{}
