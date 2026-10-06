// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package webhook

import (
	"context"
	"fmt"
	"net/netip"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1validation "k8s.io/apimachinery/pkg/apis/meta/v1/validation"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
	"github.com/zeroroot-ai/setec/internal/diskbuilder"
	"github.com/zeroroot-ai/setec/internal/errwrap"
	"github.com/zeroroot-ai/setec/internal/limits"
	"github.com/zeroroot-ai/setec/internal/runtime"
)

// +kubebuilder:webhook:path=/mutate-setec-zeroroot-ai-v1alpha1-sandboxclass,mutating=true,failurePolicy=fail,sideEffects=None,groups=setec.zeroroot.ai,resources=sandboxclasses,verbs=create;update,versions=v1alpha1,name=msandboxclass.setec.zeroroot.ai,admissionReviewVersions=v1
// +kubebuilder:webhook:path=/validate-setec-zeroroot-ai-v1alpha1-sandboxclass,mutating=false,failurePolicy=fail,sideEffects=None,groups=setec.zeroroot.ai,resources=sandboxclasses,verbs=create;update,versions=v1alpha1,name=vsandboxclass.setec.zeroroot.ai,admissionReviewVersions=v1

// SandboxClassWebhook implements both the defaulting and validating admission
// webhooks for v1alpha1.SandboxClass. It is registered once per manager and
// acts as:
//   - a mutating webhook that sets Runtime.Backend to the one backend, the
//     launcher, when the class names none.
//   - a validating webhook that refuses a removed backend with the reason,
//     and checks the coherence of the network, pool, session and resource
//     fields.
//
// It reads no other object, so it holds no client.
type SandboxClassWebhook struct{}

// Compile-time interface assertions. A broken refactor produces a build error
// rather than a runtime admission failure.
var _ admission.Defaulter[*setecv1alpha1.SandboxClass] = (*SandboxClassWebhook)(nil)
var _ admission.Validator[*setecv1alpha1.SandboxClass] = (*SandboxClassWebhook)(nil)

// Default implements admission.Defaulter[*SandboxClass]. A class with no
// Runtime gets the one backend. Calling Default twice is idempotent.
func (w *SandboxClassWebhook) Default(_ context.Context, class *setecv1alpha1.SandboxClass) error {
	if class.Spec.Runtime != nil {
		return nil
	}
	class.Spec.Runtime = &setecv1alpha1.SandboxClassRuntime{Backend: runtime.BackendLauncher}
	return nil
}

// ValidateCreate implements admission.Validator[*SandboxClass] for creates
// (REQ-4.2, REQ-4.3, Error Handling scenario 3).
func (w *SandboxClassWebhook) ValidateCreate(ctx context.Context, class *setecv1alpha1.SandboxClass) (admission.Warnings, error) {
	return w.validate(class)
}

// ValidateUpdate implements admission.Validator[*SandboxClass] for updates.
// The same rules that apply to creation apply to mutation: a class cannot be
// updated to name a removed backend.
func (w *SandboxClassWebhook) ValidateUpdate(ctx context.Context, _, newClass *setecv1alpha1.SandboxClass) (admission.Warnings, error) {
	return w.validate(newClass)
}

// ValidateDelete implements admission.Validator[*SandboxClass]. Deletion is
// always permitted; orphaned Sandboxes are handled by the reconciler.
func (w *SandboxClassWebhook) ValidateDelete(_ context.Context, _ *setecv1alpha1.SandboxClass) (admission.Warnings, error) {
	return nil, nil
}

// validate is the shared create/update path. It aggregates all field errors so
// users see every violation at once rather than playing whack-a-mole.
func (w *SandboxClassWebhook) validate(class *setecv1alpha1.SandboxClass) (admission.Warnings, error) {
	var allErrs field.ErrorList

	// Default-deny egress consistency (docs/design/threat-model.md, setec#66): when a class
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

	// Warm pool fields (docs/design/lifecycles.md, setec#103): a pool needs
	// an image with a digest and the resources that its bases boot with.
	allErrs = append(allErrs, validatePreWarm(class)...)
	allErrs = append(allErrs, validateSessionCheckpoint(class)...)
	allErrs = append(allErrs, validateMaxPauseDuration(class)...)
	allErrs = append(allErrs, validateRequests(class)...)
	allErrs = append(allErrs, validateScratch(class)...)
	allErrs = append(allErrs, validateEgressExemptCIDRs(class)...)
	allErrs = append(allErrs, validateEgressAllowSelectors(class)...)
	// One backend (setec#198): a removed backend name is refused with the
	// reason, never run on another isolation.
	if class.Spec.Runtime != nil {
		if err := runtime.ValidateBackend(class.Spec.Runtime.Backend); err != nil {
			allErrs = append(allErrs, field.Invalid(field.NewPath("spec", "runtime", "backend"),
				class.Spec.Runtime.Backend, err.Error()))
		}
	}

	if len(allErrs) == 0 {
		return nil, nil
	}
	return nil, allErrs.ToAggregate()
}

// validatePreWarm enforces the coherence rules of the warm pool of a
// class (setec#103): a non-zero pool size needs a PreWarmImage with a
// digest, because a base belongs to one digest, and DefaultResources,
// because a base boots with them.
func validatePreWarm(class *setecv1alpha1.SandboxClass) field.ErrorList {
	var errs field.ErrorList
	specPath := field.NewPath("spec")
	if class.Spec.PreWarmPoolSize <= 0 {
		return nil
	}
	switch {
	case class.Spec.PreWarmImage == "":
		errs = append(errs, field.Required(specPath.Child("preWarmImage"),
			fmt.Sprintf("preWarmPoolSize=%d requires preWarmImage: the operator boots each base from the class image",
				class.Spec.PreWarmPoolSize)))
	case !strings.Contains(class.Spec.PreWarmImage, "@sha256:"):
		errs = append(errs, field.Invalid(specPath.Child("preWarmImage"), class.Spec.PreWarmImage,
			"a pool needs an image with a digest: a base belongs to one digest"))
	}
	if class.Spec.DefaultResources == nil {
		errs = append(errs, field.Required(specPath.Child("defaultResources"),
			"a pool boots its bases with the default resources of the class"))
	}
	errs = append(errs, validateImageSignature(class.Spec.PreWarmImageSignature, specPath.Child("preWarmImageSignature"))...)
	return errs
}

// validateImageSignature requires the signer of the pool image: a keyless
// issuer and identity, or a PEM public key, and not both. The operator
// builds no base from an image it cannot verify.
func validateImageSignature(sig *setecv1alpha1.ImageSignature, path *field.Path) field.ErrorList {
	if sig == nil {
		return field.ErrorList{field.Required(path,
			"a pool needs the signer of its image: the operator checks the signature before it builds a base")}
	}
	p := diskbuilder.SignaturePolicy{Issuer: sig.Issuer, Identity: sig.Identity, PublicKey: []byte(sig.PublicKey)}
	if err := p.Validate(); err != nil {
		return field.ErrorList{field.Invalid(path, "", err.Error())}
	}
	return nil
}

// SetupWebhookWithManager registers both the defaulting and validating webhooks
// for SandboxClass with the controller-runtime manager. Invoke from cmd/main.go
// alongside the other webhook registrations.
func (w *SandboxClassWebhook) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return errwrap.Wrap(ctrl.NewWebhookManagedBy(mgr, &setecv1alpha1.SandboxClass{}).
		WithDefaulter(w).
		WithValidator(w).
		Complete(), "builder.WebhookBuilder.Complete")
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

// validateScratch checks the two scratch values of a class (ADR-0146). Each
// must be positive, and the default must fit under the ceiling: a default
// above it would make every Sandbox that omits scratch fail admission.
func validateScratch(class *setecv1alpha1.SandboxClass) field.ErrorList {
	var errs field.ErrorList
	ceiling := limits.ScratchCeiling(class)
	if m := class.Spec.MaxResources; m != nil && m.Scratch != nil && m.Scratch.Sign() <= 0 {
		errs = append(errs, field.Invalid(field.NewPath("spec", "maxResources", "scratch"),
			m.Scratch.String(), "must be positive"))
	}
	if d := class.Spec.DefaultResources; d != nil && d.Scratch != nil {
		p := field.NewPath("spec", "defaultResources", "scratch")
		switch {
		case d.Scratch.Sign() <= 0:
			errs = append(errs, field.Invalid(p, d.Scratch.String(), "must be positive"))
		case d.Scratch.Cmp(ceiling) > 0:
			errs = append(errs, field.Invalid(p, d.Scratch.String(),
				fmt.Sprintf("exceeds the scratch ceiling of the class (%s)", ceiling.String())))
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

// validateEgressExemptCIDRs checks that every spec.egressExemptCIDRs
// entry parses as a prefix. The generator refuses a malformed entry at
// reconcile time, which leaves every Sandbox in the class without a
// policy and Pending; admission is where the author sees it.
func validateEgressExemptCIDRs(class *setecv1alpha1.SandboxClass) field.ErrorList {
	var errs field.ErrorList
	base := field.NewPath("spec", "egressExemptCIDRs")
	for i, cidr := range class.Spec.EgressExemptCIDRs {
		if _, err := netip.ParsePrefix(cidr); err != nil {
			errs = append(errs, field.Invalid(base.Index(i), cidr,
				fmt.Sprintf("must be a CIDR prefix such as 10.96.0.0/12: %v", err)))
		}
	}
	return errs
}

// egressAllowProtocols is the protocol set a NetworkPolicyPort accepts.
var egressAllowProtocols = []string{
	string(corev1.ProtocolTCP), string(corev1.ProtocolUDP), string(corev1.ProtocolSCTP),
}

// validateEgressAllowSelectors checks the shape of every
// spec.egressAllowSelectors entry (setec#76) the way the API server
// checks a NetworkPolicy egress rule: a peer names at least one
// selector, each selector is a valid label selector, and every port is
// a number in range or a valid port name with a known protocol. An
// entry with no port is refused because an allowance never opens every
// port on the selected Pods.
func validateEgressAllowSelectors(class *setecv1alpha1.SandboxClass) field.ErrorList {
	var errs field.ErrorList
	base := field.NewPath("spec", "egressAllowSelectors")
	selOpts := metav1validation.LabelSelectorValidationOptions{}

	for i, a := range class.Spec.EgressAllowSelectors {
		p := base.Index(i)
		if a.NamespaceSelector == nil && a.PodSelector == nil {
			errs = append(errs, field.Required(p,
				"at least one of namespaceSelector or podSelector must be set; a peer with neither selects nothing"))
		}
		if a.NamespaceSelector != nil {
			errs = append(errs, metav1validation.ValidateLabelSelector(
				a.NamespaceSelector, selOpts, p.Child("namespaceSelector"))...)
		}
		if a.PodSelector != nil {
			errs = append(errs, metav1validation.ValidateLabelSelector(
				a.PodSelector, selOpts, p.Child("podSelector"))...)
		}
		if len(a.Ports) == 0 {
			errs = append(errs, field.Required(p.Child("ports"),
				"at least one port is required; an allowance never opens every port"))
		}
		for j, port := range a.Ports {
			pp := p.Child("ports").Index(j)
			if port.Protocol != "" && !slices.Contains(egressAllowProtocols, string(port.Protocol)) {
				errs = append(errs, field.NotSupported(pp.Child("protocol"), string(port.Protocol), egressAllowProtocols))
			}
			switch port.Port.Type {
			case intstr.Int:
				for _, msg := range validation.IsValidPortNum(port.Port.IntValue()) {
					errs = append(errs, field.Invalid(pp.Child("port"), port.Port.IntValue(), msg))
				}
			case intstr.String:
				for _, msg := range validation.IsValidPortName(port.Port.StrVal) {
					errs = append(errs, field.Invalid(pp.Child("port"), port.Port.StrVal, msg))
				}
			}
		}
	}
	return errs
}
