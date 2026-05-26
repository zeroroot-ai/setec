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
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
	"github.com/zeroroot-ai/setec/internal/class"
)

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	utilruntime.Must(setecv1alpha1.AddToScheme(s))
	return s
}

func newFakeClient(t *testing.T, classes ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(classes...).Build()
}

// stubNamespaceGetter is a minimal in-memory implementation of
// NamespaceGetter used by tests that exercise the multi-tenancy branch.
type stubNamespaceGetter struct {
	byName map[string]map[string]string
	err    error
}

func (s *stubNamespaceGetter) GetNamespaceLabels(_ context.Context, name string) (map[string]string, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.byName[name], nil
}

// mkClass builds a SandboxClass with the given name, max resources, and
// allowed modes. Helpers keep table cases focused on inputs.
func mkClass(name string, isDefault bool, maxMem string, modes ...setecv1alpha1.NetworkMode) *setecv1alpha1.SandboxClass {
	return &setecv1alpha1.SandboxClass{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: setecv1alpha1.SandboxClassSpec{
			VMM:     setecv1alpha1.VMMFirecracker, //nolint:staticcheck // back-compat: VMM retained until v2
			Default: isDefault,
			MaxResources: &setecv1alpha1.Resources{
				VCPU:   4,
				Memory: resource.MustParse(maxMem),
			},
			AllowedNetworkModes: modes,
		},
	}
}

// mkSandbox constructs a Sandbox with knobs for the fields tests vary.
func mkSandbox(className string, vcpu int32, mem string, mode setecv1alpha1.NetworkMode) *setecv1alpha1.Sandbox {
	var netSpec *setecv1alpha1.Network
	if mode != "" {
		netSpec = &setecv1alpha1.Network{Mode: mode}
	}
	return &setecv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sb", Namespace: "team-a"},
		Spec: setecv1alpha1.SandboxSpec{
			SandboxClassName: className,
			Image:            "alpine:3.19",
			Command:          []string{"sh"},
			Resources: setecv1alpha1.Resources{
				VCPU:   vcpu,
				Memory: resource.MustParse(mem),
			},
			Network: netSpec,
		},
	}
}

func TestValidateCreate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		seed        []client.Object
		sb          *setecv1alpha1.Sandbox
		multitenant bool
		labelKey    string
		nsGetter    NamespaceGetter
		wantErr     bool
		wantMsg     string // substring match
	}{
		{
			name: "valid sandbox within class constraints",
			seed: []client.Object{
				mkClass("standard", false, "8Gi",
					setecv1alpha1.NetworkModeNone, setecv1alpha1.NetworkModeEgressAllowList),
			},
			sb: mkSandbox("standard", 2, "2Gi", setecv1alpha1.NetworkModeNone),
		},
		{
			name: "vcpu exceeds class max",
			seed: []client.Object{
				mkClass("standard", false, "8Gi"),
			},
			sb:      mkSandbox("standard", 8, "2Gi", ""),
			wantErr: true,
			wantMsg: "vcpu",
		},
		{
			name: "memory exceeds class max",
			seed: []client.Object{
				mkClass("standard", false, "8Gi"),
			},
			sb:      mkSandbox("standard", 2, "16Gi", ""),
			wantErr: true,
			wantMsg: "memory",
		},
		{
			name: "referenced class not found",
			seed: []client.Object{
				mkClass("standard", false, "8Gi"),
			},
			sb:      mkSandbox("missing", 1, "512Mi", ""),
			wantErr: true,
			wantMsg: "not found",
		},
		{
			name:    "no class name, no default in cluster (multitenant on)",
			seed:    nil,
			sb:      mkSandbox("", 1, "512Mi", ""),
			wantErr: true,
			// Multi-tenancy off → no-default is acceptable (Phase 1 compat).
			// Flip multitenant to require a class.
			multitenant: true,
			labelKey:    "setec.zeroroot.ai/tenant",
			nsGetter: &stubNamespaceGetter{byName: map[string]map[string]string{
				"team-a": {"setec.zeroroot.ai/tenant": "team-a"},
			}},
			wantMsg: "no default SandboxClass",
		},
		{
			name: "no class name, no default (Phase 1 back-compat)",
			seed: nil,
			sb:   mkSandbox("", 1, "512Mi", ""),
			// multitenant off, no default — the webhook admits and
			// the reconciler handles back-compat.
		},
		{
			name: "network mode not in allowed list",
			seed: []client.Object{
				mkClass("standard", false, "8Gi",
					setecv1alpha1.NetworkModeNone),
			},
			sb:      mkSandbox("standard", 2, "2Gi", setecv1alpha1.NetworkModeFull),
			wantErr: true,
			wantMsg: "network.mode",
		},
		{
			name: "multi-tenancy enabled: namespace missing tenant label",
			seed: []client.Object{
				mkClass("standard", false, "8Gi"),
			},
			sb:          mkSandbox("standard", 2, "2Gi", ""),
			multitenant: true,
			labelKey:    "setec.zeroroot.ai/tenant",
			nsGetter: &stubNamespaceGetter{
				byName: map[string]map[string]string{
					"team-a": {},
				},
			},
			wantErr: true,
			wantMsg: "tenant label",
		},
		{
			name: "multi-tenancy enabled: namespace has valid tenant label",
			seed: []client.Object{
				mkClass("standard", false, "8Gi"),
			},
			sb:          mkSandbox("standard", 2, "2Gi", ""),
			multitenant: true,
			labelKey:    "setec.zeroroot.ai/tenant",
			nsGetter: &stubNamespaceGetter{
				byName: map[string]map[string]string{
					"team-a": {"setec.zeroroot.ai/tenant": "team-a"},
				},
			},
		},
		{
			name: "ambiguous default: two classes marked default",
			seed: []client.Object{
				mkClass("a", true, "8Gi"),
				mkClass("b", true, "8Gi"),
			},
			sb:      mkSandbox("", 2, "2Gi", ""),
			wantErr: true,
			wantMsg: "ambiguity",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := newFakeClient(t, tc.seed...)
			v := &SandboxValidator{
				Resolver:            class.NewResolver(c),
				MultiTenancyEnabled: tc.multitenant,
				TenantLabelKey:      tc.labelKey,
				NamespaceGetter:     tc.nsGetter,
			}
			_, err := v.ValidateCreate(context.Background(), tc.sb)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tc.wantMsg)
				}
				if tc.wantMsg != "" && !strings.Contains(err.Error(), tc.wantMsg) {
					t.Fatalf("err %q does not contain %q", err.Error(), tc.wantMsg)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestValidateUpdate(t *testing.T) {
	t.Parallel()

	seed := []client.Object{mkClass("standard", false, "8Gi")}
	c := newFakeClient(t, seed...)
	v := &SandboxValidator{Resolver: class.NewResolver(c)}

	oldSB := mkSandbox("standard", 2, "2Gi", "")
	newSB := mkSandbox("standard", 8, "2Gi", "")

	_, err := v.ValidateUpdate(context.Background(), oldSB, newSB)
	if err == nil {
		t.Fatalf("expected vcpu-ceiling error on update, got nil")
	}
}

func TestValidateDelete(t *testing.T) {
	t.Parallel()
	v := &SandboxValidator{Resolver: class.NewResolver(newFakeClient(t))}
	warnings, err := v.ValidateDelete(context.Background(), mkSandbox("", 1, "1Gi", ""))
	if err != nil {
		t.Fatalf("ValidateDelete() err = %v, want nil", err)
	}
	if warnings != nil {
		t.Fatalf("ValidateDelete() warnings = %v, want nil", warnings)
	}
}

// mkSandboxWithSnapshotRef returns a Sandbox that references a Snapshot
// via spec.snapshotRef.name. Used by the Phase 3 admission tests.
func mkSandboxWithSnapshotRef(refName string) *setecv1alpha1.Sandbox {
	sb := mkSandbox("standard", 2, "2Gi", "")
	sb.Spec.Image = "img:v1"
	sb.Spec.SnapshotRef = &setecv1alpha1.SandboxSnapshotRef{Name: refName}
	return sb
}

// mkSnapshot returns a ready Snapshot CR for admission tests.
func mkSnapshot(ns, name, classObj, image string, vmm setecv1alpha1.VMM) *setecv1alpha1.Snapshot {
	return &setecv1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: setecv1alpha1.SnapshotSpec{
			SandboxClass: classObj, ImageRef: image, VMM: vmm,
			StorageBackend: "local-disk", StorageRef: name,
			Node: "node-a",
		},
	}
}

func TestValidateCreate_Phase3_SnapshotRef(t *testing.T) {
	t.Parallel()

	t.Run("missing snapshot rejected", func(t *testing.T) {
		t.Parallel()
		seed := []client.Object{mkClass("standard", false, "8Gi")}
		c := newFakeClient(t, seed...)
		v := &SandboxValidator{Resolver: class.NewResolver(c), Client: c}
		sb := mkSandboxWithSnapshotRef("ghost")
		_, err := v.ValidateCreate(context.Background(), sb)
		if err == nil || !strings.Contains(err.Error(), "not found") {
			t.Fatalf("expected not-found error, got %v", err)
		}
	})

	t.Run("cross-namespace rejected via Validator", func(t *testing.T) {
		t.Parallel()
		cls := mkClass("standard", false, "8Gi")
		snap := mkSnapshot("team-b", "snap-1", "standard", "img:v1", setecv1alpha1.VMMFirecracker)
		c := newFakeClient(t, cls, snap)
		v := &SandboxValidator{Resolver: class.NewResolver(c), Client: c}
		sb := mkSandboxWithSnapshotRef("snap-1")
		_, err := v.ValidateCreate(context.Background(), sb)
		// When the Snapshot is in a different namespace, Get returns
		// NotFound (fake client is namespaced). That's effectively
		// equivalent to the cross-namespace rejection.
		if err == nil {
			t.Fatalf("expected error")
		}
	})

	t.Run("incompatible class rejected", func(t *testing.T) {
		t.Parallel()
		cls := mkClass("standard", false, "8Gi")
		snap := mkSnapshot("team-a", "snap-1", "fast", "img:v1", setecv1alpha1.VMMFirecracker)
		c := newFakeClient(t, cls, snap)
		v := &SandboxValidator{Resolver: class.NewResolver(c), Client: c}
		sb := mkSandboxWithSnapshotRef("snap-1")
		_, err := v.ValidateCreate(context.Background(), sb)
		if err == nil || !strings.Contains(err.Error(), "SandboxClass") {
			t.Fatalf("expected class-mismatch error, got %v", err)
		}
	})

	t.Run("compatible snapshot accepted", func(t *testing.T) {
		t.Parallel()
		cls := mkClass("standard", false, "8Gi")
		snap := mkSnapshot("team-a", "snap-1", "standard", "img:v1", setecv1alpha1.VMMFirecracker)
		c := newFakeClient(t, cls, snap)
		v := &SandboxValidator{Resolver: class.NewResolver(c), Client: c}
		sb := mkSandboxWithSnapshotRef("snap-1")
		_, err := v.ValidateCreate(context.Background(), sb)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}

func TestValidateCreate_NamespaceGetterError(t *testing.T) {
	t.Parallel()

	seed := []client.Object{mkClass("standard", false, "8Gi")}
	c := newFakeClient(t, seed...)
	v := &SandboxValidator{
		Resolver:            class.NewResolver(c),
		MultiTenancyEnabled: true,
		TenantLabelKey:      "setec.zeroroot.ai/tenant",
		NamespaceGetter: &stubNamespaceGetter{
			err: errors.New("api server unavailable"),
		},
	}
	sb := mkSandbox("standard", 2, "2Gi", "")
	_, err := v.ValidateCreate(context.Background(), sb)
	if err == nil {
		t.Fatalf("expected error from namespace getter")
	}
}

// TestValidateCreate_FailClosedWhenMultitenancyUnwired asserts that a
// SandboxValidator with MultiTenancyEnabled=true but no NamespaceGetter
// refuses Sandboxes instead of silently skipping the tenant-label
// check. This is the production mis-wiring guard: a chart deployed
// with the tenant flag but without plumbing the namespace getter
// must not silently allow every Sandbox through.
func TestValidateCreate_FailClosedWhenMultitenancyUnwired(t *testing.T) {
	v := &SandboxValidator{
		Resolver:            class.NewResolver(newFakeClient(t, mkClass("standard", true, "4Gi"))),
		MultiTenancyEnabled: true,
		TenantLabelKey:      "setec.zeroroot.ai/tenant",
		// NamespaceGetter deliberately unset.
	}
	sb := mkSandbox("standard", 1, "1Gi", "")
	_, err := v.ValidateCreate(context.Background(), sb)
	if err == nil {
		t.Fatal("expected fail-closed error, got nil")
	}
	if !strings.Contains(err.Error(), "NamespaceGetter not configured") {
		t.Fatalf("err = %v, want 'NamespaceGetter not configured'", err)
	}
}

// TestClientNamespaceGetter_HappyPath builds the production
// ClientNamespaceGetter against a fake controller-runtime client and
// asserts it returns the Namespace's label map.
func TestClientNamespaceGetter_HappyPath(t *testing.T) {
	s := runtime.NewScheme()
	utilruntime.Must(setecv1alpha1.AddToScheme(s))
	utilruntime.Must(corev1.AddToScheme(s))
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "team-a",
			Labels: map[string]string{"setec.zeroroot.ai/tenant": "team-a"},
		},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(ns).Build()
	g := &ClientNamespaceGetter{Client: c}

	labels, err := g.GetNamespaceLabels(context.Background(), "team-a")
	if err != nil {
		t.Fatalf("GetNamespaceLabels: %v", err)
	}
	if labels["setec.zeroroot.ai/tenant"] != "team-a" {
		t.Fatalf("label = %q, want team-a", labels["setec.zeroroot.ai/tenant"])
	}
}

// TestClientNamespaceGetter_Missing returns a NotFound error so the
// validator can render the right "namespace %q not found" message.
func TestClientNamespaceGetter_Missing(t *testing.T) {
	s := runtime.NewScheme()
	utilruntime.Must(corev1.AddToScheme(s))
	c := fake.NewClientBuilder().WithScheme(s).Build()
	g := &ClientNamespaceGetter{Client: c}

	_, err := g.GetNamespaceLabels(context.Background(), "nope")
	if err == nil {
		t.Fatal("expected NotFound, got nil")
	}
	if !apierrors.IsNotFound(err) {
		t.Fatalf("err = %v, want IsNotFound", err)
	}
}

// TestClientNamespaceGetter_NilReceiver guards against the
// accidentally-nil case.
func TestClientNamespaceGetter_NilReceiver(t *testing.T) {
	var g *ClientNamespaceGetter
	if _, err := g.GetNamespaceLabels(context.Background(), "any"); err == nil {
		t.Fatal("expected error, got nil")
	}
}
