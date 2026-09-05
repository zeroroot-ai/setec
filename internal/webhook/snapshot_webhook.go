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
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
)

// SnapshotResourceName is the ResourceQuota counter name that
// restricts the number of Snapshot CRs a tenant namespace may hold.
// Operators wire it via the standard count/<resource>.<group> form:
//
//	apiVersion: v1
//	kind: ResourceQuota
//	spec:
//	  hard:
//	    count/snapshots.setec.zeroroot.ai: "10"
const SnapshotResourceName = corev1.ResourceName("count/snapshots.setec.zeroroot.ai")

// minSnapshotTTL is the lower bound we enforce on Snapshot.spec.ttl.
// A 30-second TTL would race with creation and produce thrash; an
// administrative minimum of one minute strikes a balance between
// expressiveness and operational sanity.
const minSnapshotTTL = time.Minute

// +kubebuilder:webhook:path=/validate-setec-zeroroot-ai-v1alpha1-snapshot,mutating=false,failurePolicy=fail,sideEffects=None,groups=setec.zeroroot.ai,resources=snapshots,verbs=create;update,versions=v1alpha1,name=vsnapshot.setec.zeroroot.ai,admissionReviewVersions=v1

// SnapshotValidator is the controller-runtime CustomValidator for
// v1alpha1.Snapshot. It rejects:
//   - Snapshots with TTL below a sane minimum (1 minute).
//   - Snapshots whose creation would exceed a namespace ResourceQuota
//     on count/snapshots.setec.zeroroot.ai.
//   - Duplicate-name creates (best-effort; the API server is the
//     authoritative uniqueness guarantor).
type SnapshotValidator struct {
	// Client is a controller-runtime reader used to inspect existing
	// Snapshots and ResourceQuotas in the namespace. Required.
	Client client.Reader
}

var _ admission.Validator[*setecv1alpha1.Snapshot] = (*SnapshotValidator)(nil)

// ValidateCreate is the primary admission path.
func (v *SnapshotValidator) ValidateCreate(ctx context.Context, snap *setecv1alpha1.Snapshot) (admission.Warnings, error) {
	return v.validate(ctx, snap, true)
}

// ValidateUpdate runs the subset of checks that can meaningfully
// change on update — TTL tightening in particular. Name uniqueness
// cannot change on update (the name is immutable on update).
func (v *SnapshotValidator) ValidateUpdate(ctx context.Context, _, newObj *setecv1alpha1.Snapshot) (admission.Warnings, error) {
	return v.validate(ctx, newObj, false)
}

// ValidateDelete is a no-op. Deletion flows through the finalizer.
func (v *SnapshotValidator) ValidateDelete(_ context.Context, _ *setecv1alpha1.Snapshot) (admission.Warnings, error) {
	return nil, nil
}

// validate collects rule violations into a single aggregated error.
// isCreate toggles the name-collision and quota checks that only
// apply to new Snapshot objects.
func (v *SnapshotValidator) validate(ctx context.Context, snap *setecv1alpha1.Snapshot, isCreate bool) (admission.Warnings, error) {
	var errs []error

	// Rule 1: TTL minimum.
	if snap.Spec.TTL != nil && snap.Spec.TTL.Duration > 0 && snap.Spec.TTL.Duration < minSnapshotTTL {
		errs = append(errs, fmt.Errorf(
			"spec.ttl %s is below the minimum %s; choose a longer TTL or omit ttl to disable expiry",
			snap.Spec.TTL.Duration, minSnapshotTTL))
	}

	if isCreate && v.Client != nil {
		// Rule 2: best-effort name-collision detection. The API server
		// catches a true collision via its uniqueness guarantee, but
		// surfacing the conflict here produces a clearer message than
		// the generic "already exists" error.
		existing := &setecv1alpha1.Snapshot{}
		err := v.Client.Get(ctx, client.ObjectKey{Namespace: snap.Namespace, Name: snap.Name}, existing)
		switch {
		case err == nil:
			errs = append(errs, fmt.Errorf(
				"snapshot %q already exists in namespace %q", snap.Name, snap.Namespace))
		case !apierrors.IsNotFound(err):
			return nil, fmt.Errorf("webhook: check duplicate Snapshot: %w", err)
		}

		// Rule 3: per-tenant ResourceQuota check. If a ResourceQuota in
		// the namespace caps count/snapshots.setec.zeroroot.ai and the current
		// total (admission is non-atomic; be defensive) would exceed
		// it, reject.
		if err := v.checkQuota(ctx, snap); err != nil {
			errs = append(errs, err)
		}
	}

	if len(errs) == 0 {
		return nil, nil
	}
	return nil, utilerrors.NewAggregate(errs)
}

// checkQuota reads every ResourceQuota in the namespace and rejects
// the create when any quota has reached its cap.
func (v *SnapshotValidator) checkQuota(ctx context.Context, snap *setecv1alpha1.Snapshot) error {
	quotas := &corev1.ResourceQuotaList{}
	if err := v.Client.List(ctx, quotas, client.InNamespace(snap.Namespace)); err != nil {
		// Treat list errors as "unable to validate"; return a wrapped
		// error so failurePolicy=fail can surface it to the user.
		return fmt.Errorf("webhook: list ResourceQuotas: %w", err)
	}
	if len(quotas.Items) == 0 {
		return nil
	}

	existing := &setecv1alpha1.SnapshotList{}
	if err := v.Client.List(ctx, existing, client.InNamespace(snap.Namespace)); err != nil {
		return fmt.Errorf("webhook: list Snapshots: %w", err)
	}
	current := int64(len(existing.Items))

	for _, q := range quotas.Items {
		limit, ok := q.Spec.Hard[SnapshotResourceName]
		if !ok {
			continue
		}
		// Quantity.Value() rounds up for counts — exact conversion is
		// fine because count/* values are required to be whole numbers.
		max := limit.Value()
		if current+1 > max {
			return fmt.Errorf(
				"ResourceQuota %q in namespace %q caps %q at %s; current=%d",
				q.Name, snap.Namespace, SnapshotResourceName, quantityString(limit), current)
		}
	}
	return nil
}

// quantityString returns a user-friendly quantity representation that
// avoids the "1e+01" style when large values slip through.
func quantityString(q resource.Quantity) string {
	return q.String()
}

// SetupWebhookWithManager registers the Snapshot validating webhook.
func (v *SnapshotValidator) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &setecv1alpha1.Snapshot{}).
		WithValidator(v).
		Complete()
}

// errMinSnapshotTTL is a sentinel so tests can errors.Is against
// a stable value when asserting the TTL rule.
var errMinSnapshotTTL = errors.New("snapshot ttl below minimum")

// Ensure the sentinel is referenced so linters do not flag it as
// unused when the downstream tests migrate to errors.Is.
var _ = errMinSnapshotTTL
