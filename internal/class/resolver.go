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

package class

import (
	"context"
	"errors"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
)

// Sentinel errors returned by Resolver. Callers use errors.Is to classify
// resolution failures into admission-layer messages; the specific error
// type is not load-bearing beyond the exported identity check.
var (
	// ErrClassNotFound is returned when a Sandbox names a SandboxClass
	// that does not exist in the cluster.
	ErrClassNotFound = errors.New("class: named SandboxClass not found")

	// ErrNoDefaultClass is returned when a Sandbox does not name a class
	// and no SandboxClass carries default:true.
	ErrNoDefaultClass = errors.New("class: no default SandboxClass configured")

	// ErrAmbiguousDefault is returned when a Sandbox does not name a
	// class and two or more SandboxClasses carry default:true. The
	// operator refuses to guess; the administrator must resolve the
	// ambiguity.
	ErrAmbiguousDefault = errors.New("class: multiple SandboxClasses marked default:true")
)

// Resolver is the K8s-aware part of the class package. It reads
// SandboxClasses through a controller-runtime client; it never writes.
// Construction deliberately accepts the generic client.Client interface so
// unit tests can inject fake clients without needing an envtest.
type Resolver struct {
	// Client is the Kubernetes client used for SandboxClass reads.
	// Required; Resolve will panic on nil to surface wiring mistakes
	// early rather than returning a misleading NotFound error.
	Client client.Client
}

// NewResolver is the preferred constructor. Using a function rather than a
// struct literal gives us room to evolve the resolver's internals (e.g.
// adding a cache) without touching every caller.
func NewResolver(c client.Client) *Resolver {
	return &Resolver{Client: c}
}

// Resolve returns the effective SandboxClass for sb. Behavior:
//
//   - If sb.Spec.SandboxClassName is set, Resolve issues a cluster-scoped
//     Get for that name. A NotFound error is wrapped as ErrClassNotFound;
//     any other error is propagated unchanged.
//   - If sb.Spec.SandboxClassName is empty, Resolve lists every
//     SandboxClass in the cluster and returns the one whose
//     spec.default is true. Zero matches yields ErrNoDefaultClass, two
//     or more yields ErrAmbiguousDefault.
//
// Resolve never mutates sb and never writes to the cluster.
func (r *Resolver) Resolve(ctx context.Context, sb *setecv1alpha1.Sandbox) (*setecv1alpha1.SandboxClass, error) {
	if r == nil || r.Client == nil {
		// A nil-resolver path indicates a plumbing error higher up;
		// returning a descriptive error rather than panicking keeps
		// the controller alive and surfaces the problem via Events.
		return nil, errors.New("class: Resolver is not initialised")
	}
	if sb == nil {
		return nil, errors.New("class: Resolve called with nil Sandbox")
	}

	if name := sb.Spec.SandboxClassName; name != "" {
		return r.resolveByName(ctx, name)
	}
	return r.resolveDefault(ctx)
}

// resolveByName issues a cluster-scoped Get. Namespace is intentionally
// empty because SandboxClass is cluster-scoped.
func (r *Resolver) resolveByName(ctx context.Context, name string) (*setecv1alpha1.SandboxClass, error) {
	cls := &setecv1alpha1.SandboxClass{}
	if err := r.Client.Get(ctx, types.NamespacedName{Name: name}, cls); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("%w: %q", ErrClassNotFound, name)
		}
		return nil, fmt.Errorf("class: get SandboxClass %q: %w", name, err)
	}
	return cls, nil
}

// resolveDefault lists all SandboxClasses and filters client-side for
// spec.default. A field selector is not used because Kubernetes does not
// support arbitrary field selectors on CRDs; a label would work but would
// require administrators to duplicate the default flag at label level,
// which we avoid to keep the UX single-source-of-truth on spec.default.
func (r *Resolver) resolveDefault(ctx context.Context) (*setecv1alpha1.SandboxClass, error) {
	list := &setecv1alpha1.SandboxClassList{}
	if err := r.Client.List(ctx, list); err != nil {
		return nil, fmt.Errorf("class: list SandboxClasses: %w", err)
	}

	var defaults []setecv1alpha1.SandboxClass
	for i := range list.Items {
		if list.Items[i].Spec.Default {
			defaults = append(defaults, list.Items[i])
		}
	}
	switch len(defaults) {
	case 0:
		return nil, ErrNoDefaultClass
	case 1:
		return &defaults[0], nil
	default:
		names := make([]string, 0, len(defaults))
		for _, d := range defaults {
			names = append(names, d.Name)
		}
		return nil, fmt.Errorf("%w: %v", ErrAmbiguousDefault, names)
	}
}
