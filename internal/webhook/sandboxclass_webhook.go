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

package webhook

import (
	"context"
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
	"github.com/zeroroot-ai/setec/internal/runtime"
)

// defaultAllowDevLabel is the namespace label key that gates dev-only runtimes
// such as runc. Operators may override it via SandboxClassWebhook.AllowDevLabel.
const defaultAllowDevLabel = "setec.zeroroot.ai/allow-dev-runtimes"

// devGateNamespace is the cluster-level namespace consulted for the dev-only
// runtime gate. SandboxClass is cluster-scoped and therefore has no namespace of
// its own; a well-known namespace carries the cluster operator's intent. Using
// "default" is conventional and avoids introducing a custom cluster-scoped
// sentinel resource.
const devGateNamespace = "default"

// +kubebuilder:webhook:path=/mutate-setec-zeroroot-ai-v1alpha1-sandboxclass,mutating=true,failurePolicy=fail,sideEffects=None,groups=setec.zeroroot.ai,resources=sandboxclasses,verbs=create;update,versions=v1alpha1,name=msandboxclass.setec.zeroroot.ai,admissionReviewVersions=v1
// +kubebuilder:webhook:path=/validate-setec-zeroroot-ai-v1alpha1-sandboxclass,mutating=false,failurePolicy=fail,sideEffects=None,groups=setec.zeroroot.ai,resources=sandboxclasses,verbs=create;update,versions=v1alpha1,name=vsandboxclass.setec.zeroroot.ai,admissionReviewVersions=v1

// SandboxClassWebhook implements both the defaulting and validating admission
// webhooks for v1alpha1.SandboxClass. It is registered once per manager and
// acts as:
//   - a mutating webhook that fills Runtime.Backend from the legacy VMM field
//     when Runtime is nil (defaulting, REQ-6.1).
//   - a validating webhook that enforces backend enablement (REQ-4.2) and the
//     dev-only namespace gate for runc (REQ-4.3).
//
// RuntimeCfg is a snapshot taken at operator startup. The webhook does not
// live-reload it; a rolling restart is required to pick up Helm value changes.
//
// The construction site in cmd/main.go MUST NOT pass a nil RuntimeCfg. If
// RuntimeConfig loading fails, cmd/main.go should call os.Exit before
// constructing this struct. Passing nil RuntimeCfg causes a nil-pointer panic
// on the first admission call, which surfaces the mis-wiring early rather than
// silently admitting everything.
//
// The dev-only gate for runc (REQ-4.3) is enforced by fetching a well-known
// namespace (devGateNamespace, "default") and checking for the AllowDevLabel.
// Cluster operators signal cluster-wide dev-runtime consent by labelling that
// namespace; a SandboxClass requesting runc without the label present is
// rejected.
type SandboxClassWebhook struct {
	// Client is a controller-runtime reader used to fetch the gate namespace for
	// dev-only backend checks. Required.
	Client client.Client

	// RuntimeCfg is the operator-wide runtime configuration loaded at startup.
	// Must not be nil; the webhook panics on the first admission call if nil.
	RuntimeCfg *runtime.RuntimeConfig

	// AllowDevLabel is the namespace label key checked when a dev-only backend
	// (e.g. runc) is requested. Defaults to defaultAllowDevLabel when empty.
	AllowDevLabel string
}

// allowDevLabel returns the effective label key, falling back to the package
// constant when the field is empty.
func (w *SandboxClassWebhook) allowDevLabel() string {
	if w.AllowDevLabel != "" {
		return w.AllowDevLabel
	}
	return defaultAllowDevLabel
}

// Compile-time interface assertions. A broken refactor produces a build error
// rather than a runtime admission failure.
var _ admission.Defaulter[*setecv1alpha1.SandboxClass] = (*SandboxClassWebhook)(nil)
var _ admission.Validator[*setecv1alpha1.SandboxClass] = (*SandboxClassWebhook)(nil)

// Default implements admission.Defaulter[*SandboxClass]. It fills
// Spec.Runtime.Backend from the legacy Spec.VMM field when Runtime is nil, so
// existing SandboxClass manifests without a Runtime block continue to behave
// correctly after an operator upgrade. Calling Default twice on the same
// object is idempotent — the second call detects Runtime != nil and returns
// without modification (REQ-6.1, Error Handling scenario 7).
func (w *SandboxClassWebhook) Default(_ context.Context, class *setecv1alpha1.SandboxClass) error {
	// Idempotency guard: if Runtime is already set, do not overwrite it.
	if class.Spec.Runtime != nil {
		return nil
	}

	backend := ""
	switch class.Spec.VMM { //nolint:staticcheck // back-compat: VMM retained until v2
	case setecv1alpha1.VMMFirecracker:
		backend = runtime.BackendKataFC
	case setecv1alpha1.VMMQEMU:
		backend = runtime.BackendKataQEMU
	default:
		// VMM is also unset or an unrecognised value — fall back to the
		// cluster-default backend from Helm values.
		backend = w.RuntimeCfg.Defaults.Runtime.Backend
	}

	class.Spec.Runtime = &setecv1alpha1.SandboxClassRuntime{Backend: backend}
	return nil
}

// ValidateCreate implements admission.Validator[*SandboxClass] for creates
// (REQ-4.2, REQ-4.3, Error Handling scenario 3).
func (w *SandboxClassWebhook) ValidateCreate(ctx context.Context, class *setecv1alpha1.SandboxClass) (admission.Warnings, error) {
	return w.validate(ctx, class)
}

// ValidateUpdate implements admission.Validator[*SandboxClass] for updates.
// The same rules that apply to creation apply to mutation: a class cannot be
// updated to reference a disabled or ungated backend.
func (w *SandboxClassWebhook) ValidateUpdate(ctx context.Context, _, newClass *setecv1alpha1.SandboxClass) (admission.Warnings, error) {
	return w.validate(ctx, newClass)
}

// ValidateDelete implements admission.Validator[*SandboxClass]. Deletion is
// always permitted; orphaned Sandboxes are handled by the reconciler.
func (w *SandboxClassWebhook) ValidateDelete(_ context.Context, _ *setecv1alpha1.SandboxClass) (admission.Warnings, error) {
	return nil, nil
}

// validate is the shared create/update path. It aggregates all field errors so
// users see every violation at once rather than playing whack-a-mole.
func (w *SandboxClassWebhook) validate(ctx context.Context, class *setecv1alpha1.SandboxClass) (admission.Warnings, error) {
	var allErrs field.ErrorList

	// Default-deny egress consistency (ADR-0052, setec#66): when a class
	// both restricts the allowed network modes and declares a default mode,
	// the default it would silently apply to a Sandbox MUST itself be an
	// allowed mode. Otherwise the operator would synthesise a posture the
	// class forbids tenants from requesting explicitly. Evaluated before the
	// Runtime nil-check so the consistency rule holds for every SandboxClass.
	if class.Spec.DefaultNetworkMode != "" &&
		len(class.Spec.AllowedNetworkModes) > 0 &&
		!slices.Contains(class.Spec.AllowedNetworkModes, class.Spec.DefaultNetworkMode) {
		allErrs = append(allErrs, field.Invalid(
			field.NewPath("spec", "defaultNetworkMode"),
			class.Spec.DefaultNetworkMode,
			fmt.Sprintf("defaultNetworkMode %q is not in allowedNetworkModes %v",
				class.Spec.DefaultNetworkMode, class.Spec.AllowedNetworkModes),
		))
	}

	// Pre-warm pool knobs (ADR-0004, setec#188). The three fields are one
	// declarative surface: a pool needs an image to bake, and a TTL of zero
	// or less would recycle entries in a hot loop.
	allErrs = append(allErrs, validatePreWarm(class)...)
	allErrs = append(allErrs, validateSessionCheckpoint(class)...)
	allErrs = append(allErrs, validateMaxPauseDuration(class)...)
	allErrs = append(allErrs, validateRequests(class)...)

	// Runtime may be nil when a SandboxClass without a Runtime block is applied
	// before the defaulting webhook fires (e.g. --dry-run, kubectl apply with
	// webhooks bypassed). Treat it as "no runtime constraint to validate" —
	// but still surface any network-default error accumulated above.
	if class.Spec.Runtime == nil {
		if len(allErrs) == 0 {
			return nil, nil
		}
		return nil, allErrs.ToAggregate()
	}

	rtPath := field.NewPath("spec", "runtime")

	// Rule 1: primary backend must be in the enabled set.
	backend := class.Spec.Runtime.Backend
	if backend != "" {
		if ferr := w.validateBackendEnabled(backend, rtPath.Child("backend")); ferr != nil {
			allErrs = append(allErrs, ferr)
		}
	}

	// Rule 2: every fallback entry must be a known AND enabled backend.
	for i, fb := range class.Spec.Runtime.Fallback {
		fbPath := rtPath.Child("fallback").Index(i)
		if ferr := w.validateBackendKnownAndEnabled(fb, fbPath); ferr != nil {
			allErrs = append(allErrs, ferr)
		}
	}

	// Rule 3: dev-only gate. If any referenced backend is marked devOnly,
	// the cluster-level gate namespace must carry the allow label.
	devBackends := []struct {
		name string
		path *field.Path
	}{}
	if backend != "" && w.isDevOnly(backend) {
		devBackends = append(devBackends, struct {
			name string
			path *field.Path
		}{backend, rtPath.Child("backend")})
	}
	for i, fb := range class.Spec.Runtime.Fallback {
		if w.isDevOnly(fb) {
			devBackends = append(devBackends, struct {
				name string
				path *field.Path
			}{fb, rtPath.Child("fallback").Index(i)})
		}
	}

	if len(devBackends) > 0 {
		// Fetch the gate namespace once — all devOnly backends share the same
		// cluster-level gate label check.
		gateLabels, gateErr := w.fetchGateNamespaceLabels(ctx)
		if gateErr != nil {
			// Fail closed: if we cannot read the namespace, reject the request.
			allErrs = append(allErrs, field.InternalError(rtPath,
				fmt.Errorf("webhook: fetching dev-runtime gate namespace %q: %w", devGateNamespace, gateErr)))
		} else {
			for _, db := range devBackends {
				if gateLabels[w.allowDevLabel()] != "true" {
					allErrs = append(allErrs, field.Forbidden(db.path,
						fmt.Sprintf("backend %q is dev-only; to use it add label %s=true to the %q namespace, "+
							"or set runtimes.%s.devOnly=false in Helm values to remove the gate",
							db.name, w.allowDevLabel(), devGateNamespace, db.name)))
				}
			}
		}
	}

	if len(allErrs) == 0 {
		return nil, nil
	}
	return nil, allErrs.ToAggregate()
}

// validatePreWarm enforces the coherence rules of the declarative
// pre-warm pool surface (PreWarmPoolSize / PreWarmImage / PreWarmTTL,
// ADR-0004):
//
//   - a non-zero pool size requires a PreWarmImage — the node-agent
//     bakes pool entries from the class image and has nothing to boot
//     otherwise;
//   - PreWarmTTL, when set, must be positive;
//   - an active pool requires the class's effective backend to be
//     kata-fc — the pool restore path drives the Kata VM's Firecracker
//     socket, which no other backend exposes. The backend rule is only
//     evaluated when Runtime is populated (the defaulting webhook runs
//     first in the admission chain, so it always is on the normal path).
func validatePreWarm(class *setecv1alpha1.SandboxClass) field.ErrorList {
	var errs field.ErrorList
	specPath := field.NewPath("spec")

	poolActive := class.Spec.PreWarmPoolSize > 0
	if poolActive && class.Spec.PreWarmImage == "" {
		errs = append(errs, field.Required(
			specPath.Child("preWarmImage"),
			fmt.Sprintf("preWarmPoolSize=%d requires preWarmImage: the node-agent builds pool entries from the class image",
				class.Spec.PreWarmPoolSize)))
	}
	if class.Spec.PreWarmTTL != nil && class.Spec.PreWarmTTL.Duration <= 0 {
		errs = append(errs, field.Invalid(
			specPath.Child("preWarmTTL"),
			class.Spec.PreWarmTTL.Duration.String(),
			"preWarmTTL must be a positive duration"))
	}
	if poolActive && class.Spec.Runtime != nil &&
		class.Spec.Runtime.Backend != "" &&
		class.Spec.Runtime.Backend != runtime.BackendKataFC {
		errs = append(errs, field.Invalid(
			specPath.Child("runtime", "backend"),
			class.Spec.Runtime.Backend,
			fmt.Sprintf("pre-warm pools require the %q backend: pool restore drives the Kata VM's Firecracker socket",
				runtime.BackendKataFC)))
	}
	return errs
}

// validateBackendEnabled returns a field.Error when backend is not present in
// the config or not enabled. Unknown-but-disabled and known-but-disabled both
// produce the same "not enabled" message — the actionable remediation is the
// same (enable via Helm).
func (w *SandboxClassWebhook) validateBackendEnabled(backend string, fldPath *field.Path) *field.Error {
	bc, ok := w.RuntimeCfg.Runtimes[backend]
	if !ok || !bc.Enabled {
		return field.Invalid(fldPath, backend,
			fmt.Sprintf("%q is not enabled in this cluster; enable via Helm value runtimes.%s.enabled=true",
				backend, backend))
	}
	return nil
}

// validateBackendKnownAndEnabled returns a field.Error for fallback entries that
// are either unknown (not in AllKnownBackends) or not enabled. Unknown backends
// get a distinct message referencing AllKnownBackends so the user knows exactly
// what values are accepted.
func (w *SandboxClassWebhook) validateBackendKnownAndEnabled(backend string, fldPath *field.Path) *field.Error {
	known := slices.Contains(runtime.AllKnownBackends, backend)
	if !known {
		return field.Invalid(fldPath, backend,
			fmt.Sprintf("%q is not a recognised backend; must be one of %v", backend, runtime.AllKnownBackends))
	}
	bc, ok := w.RuntimeCfg.Runtimes[backend]
	if !ok || !bc.Enabled {
		return field.Invalid(fldPath, backend,
			fmt.Sprintf("%q is not enabled in this cluster; enable via Helm value runtimes.%s.enabled=true",
				backend, backend))
	}
	return nil
}

// isDevOnly returns true when backend is configured with DevOnly=true.
// Unknown backends return false (no restriction imposed).
//
// The predicate is RuntimeConfig.IsDevOnly, shared with the startup check
// that keeps a devOnly backend out of the cluster defaults
// (GHSA-q7hq-f8hm-wmjr). Two copies of "what counts as devOnly" is exactly
// how the two paths drifted apart in the first place.
func (w *SandboxClassWebhook) isDevOnly(backend string) bool {
	return w.RuntimeCfg.IsDevOnly(backend)
}

// fetchGateNamespaceLabels fetches the label map of devGateNamespace.
func (w *SandboxClassWebhook) fetchGateNamespaceLabels(ctx context.Context) (map[string]string, error) {
	ns := &corev1.Namespace{}
	if err := w.Client.Get(ctx, client.ObjectKey{Name: devGateNamespace}, ns); err != nil {
		return nil, err
	}
	return ns.Labels, nil
}

// SetupWebhookWithManager registers both the defaulting and validating webhooks
// for SandboxClass with the controller-runtime manager. Invoke from cmd/main.go
// alongside the other webhook registrations.
func (w *SandboxClassWebhook) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &setecv1alpha1.SandboxClass{}).
		WithDefaulter(w).
		WithValidator(w).
		Complete()
}

// validateRequests checks the class's scheduler reservation
// (spec.requests). Each set value must be positive, and when the class
// also states a ceiling (spec.maxResources) the reservation must fit
// under it: a request no Sandbox in the class may ever reach its limit
// for is a misconfiguration, not a reservation.
func validateRequests(class *setecv1alpha1.SandboxClass) field.ErrorList {
	var errs field.ErrorList
	req := class.Spec.Requests
	if req == nil {
		return errs
	}
	base := field.NewPath("spec", "requests")
	maxRes := class.Spec.MaxResources

	if req.CPU != nil {
		p := base.Child("cpu")
		switch {
		case req.CPU.Sign() <= 0:
			errs = append(errs, field.Invalid(p, req.CPU.String(), "must be positive"))
		case maxRes != nil && req.CPU.Cmp(*resource.NewQuantity(int64(maxRes.VCPU), resource.DecimalSI)) > 0:
			errs = append(errs, field.Invalid(p, req.CPU.String(),
				fmt.Sprintf("exceeds spec.maxResources.vcpu (%d)", maxRes.VCPU)))
		}
	}
	if req.Memory != nil {
		p := base.Child("memory")
		switch {
		case req.Memory.Sign() <= 0:
			errs = append(errs, field.Invalid(p, req.Memory.String(), "must be positive"))
		case maxRes != nil && req.Memory.Cmp(maxRes.Memory) > 0:
			errs = append(errs, field.Invalid(p, req.Memory.String(),
				fmt.Sprintf("exceeds spec.maxResources.memory (%s)", maxRes.Memory.String())))
		}
	}
	return errs
}

// validateMaxPauseDuration enforces that spec.maxPauseDuration, when
// set, is a positive duration (setec#202). Zero or negative would
// read as "fail every pause instantly", which is never what an
// administrator means — omitting the field is how pauses are left
// unbounded. The reconciler additionally treats a non-positive value
// as unbounded, so a class admitted before this rule existed fails
// open rather than instantly.
func validateMaxPauseDuration(class *setecv1alpha1.SandboxClass) field.ErrorList {
	var errs field.ErrorList
	if d := class.Spec.MaxPauseDuration; d != nil && d.Duration <= 0 {
		errs = append(errs, field.Invalid(
			field.NewPath("spec", "maxPauseDuration"),
			d.Duration.String(),
			"must be a positive duration; omit maxPauseDuration to leave pauses unbounded"))
	}
	return errs
}

// validateSessionCheckpoint enforces the coherence rules of the
// session-checkpoint surface (spec.sessionCheckpoint, setec#194):
// the interval, when set, must be positive, and the backend — already
// enum-restricted by the CRD schema — must be "s3" or empty.
func validateSessionCheckpoint(class *setecv1alpha1.SandboxClass) field.ErrorList {
	var allErrs field.ErrorList
	sc := class.Spec.SessionCheckpoint
	if sc == nil {
		return allErrs
	}
	path := field.NewPath("spec", "sessionCheckpoint")
	if sc.Interval != nil && sc.Interval.Duration <= 0 {
		allErrs = append(allErrs, field.Invalid(
			path.Child("interval"), sc.Interval.Duration.String(),
			"must be positive; omit interval to disable periodic checkpoints"))
	}
	if sc.Backend != "" && sc.Backend != "s3" {
		allErrs = append(allErrs, field.NotSupported(
			path.Child("backend"), sc.Backend, []string{"s3"}))
	}
	return allErrs
}
