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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
)

// newScheme builds a runtime.Scheme with just the v1alpha1 types registered.
// Tests do not need the core scheme because the resolver only touches
// SandboxClass resources.
func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	utilruntime.Must(setecv1alpha1.AddToScheme(scheme))
	return scheme
}

// newFakeClient builds a fake controller-runtime client seeded with the
// given SandboxClasses. It exists so individual table-driven cases stay
// focused on inputs/outputs rather than fake-client boilerplate.
func newFakeClient(t *testing.T, classes ...*setecv1alpha1.SandboxClass) client.Client {
	t.Helper()
	objs := make([]client.Object, 0, len(classes))
	for _, c := range classes {
		objs = append(objs, c)
	}
	return fake.NewClientBuilder().
		WithScheme(newScheme(t)).
		WithObjects(objs...).
		Build()
}

func mkClass(name string, isDefault bool) *setecv1alpha1.SandboxClass {
	return &setecv1alpha1.SandboxClass{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: setecv1alpha1.SandboxClassSpec{
			VMM:     setecv1alpha1.VMMFirecracker,
			Default: isDefault,
		},
	}
}

func mkSandbox(className string) *setecv1alpha1.Sandbox {
	return &setecv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sb", Namespace: "ns"},
		Spec: setecv1alpha1.SandboxSpec{
			SandboxClassName: className,
			Image:            "alpine:3.19",
			Command:          []string{"true"},
			Resources:        setecv1alpha1.Resources{VCPU: 1, Memory: qty("512Mi")},
		},
	}
}

func TestResolver_Resolve(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		seed       []*setecv1alpha1.SandboxClass
		sb         *setecv1alpha1.Sandbox
		wantName   string
		wantErrIs  error
		wantErrNil bool
	}{
		{
			name:       "named class exists",
			seed:       []*setecv1alpha1.SandboxClass{mkClass("standard", false)},
			sb:         mkSandbox("standard"),
			wantName:   "standard",
			wantErrNil: true,
		},
		{
			name:      "named class not found",
			seed:      []*setecv1alpha1.SandboxClass{mkClass("other", false)},
			sb:        mkSandbox("standard"),
			wantErrIs: ErrClassNotFound,
		},
		{
			name: "no name, single default",
			seed: []*setecv1alpha1.SandboxClass{
				mkClass("other", false),
				mkClass("standard", true),
			},
			sb:         mkSandbox(""),
			wantName:   "standard",
			wantErrNil: true,
		},
		{
			name:      "no name, zero defaults",
			seed:      []*setecv1alpha1.SandboxClass{mkClass("other", false)},
			sb:        mkSandbox(""),
			wantErrIs: ErrNoDefaultClass,
		},
		{
			name: "no name, multiple defaults",
			seed: []*setecv1alpha1.SandboxClass{
				mkClass("a", true),
				mkClass("b", true),
			},
			sb:        mkSandbox(""),
			wantErrIs: ErrAmbiguousDefault,
		},
		{
			name:      "no name, cluster empty",
			seed:      nil,
			sb:        mkSandbox(""),
			wantErrIs: ErrNoDefaultClass,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := newFakeClient(t, tc.seed...)
			r := NewResolver(c)
			got, err := r.Resolve(context.Background(), tc.sb)

			if tc.wantErrNil {
				if err != nil {
					t.Fatalf("Resolve() unexpected error: %v", err)
				}
				if got == nil {
					t.Fatalf("Resolve() returned nil class without error")
				}
				if got.Name != tc.wantName {
					t.Fatalf("Resolve() name = %q, want %q", got.Name, tc.wantName)
				}
				return
			}

			if err == nil {
				t.Fatalf("Resolve() succeeded, want error %v", tc.wantErrIs)
			}
			if !errors.Is(err, tc.wantErrIs) {
				t.Fatalf("Resolve() error = %v, want errors.Is(%v)", err, tc.wantErrIs)
			}
		})
	}
}

// TestResolver_NilInputs covers the defensive paths so a wiring mistake
// produces a clear error rather than a panic.
func TestResolver_NilInputs(t *testing.T) {
	t.Parallel()

	t.Run("nil receiver", func(t *testing.T) {
		var r *Resolver
		_, err := r.Resolve(context.Background(), mkSandbox(""))
		if err == nil {
			t.Fatalf("expected error on nil receiver")
		}
	})
	t.Run("nil client", func(t *testing.T) {
		r := &Resolver{}
		_, err := r.Resolve(context.Background(), mkSandbox(""))
		if err == nil {
			t.Fatalf("expected error on nil client")
		}
	})
	t.Run("nil sandbox", func(t *testing.T) {
		c := newFakeClient(t)
		r := NewResolver(c)
		_, err := r.Resolve(context.Background(), nil)
		if err == nil {
			t.Fatalf("expected error on nil sandbox")
		}
	})
}
