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
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
	setecruntime "github.com/zeroroot-ai/setec/internal/runtime"
)

// schemeWithCore returns a scheme that includes both the setec CRDs and the
// core/v1 types needed by the dev-only gate namespace fetch.
func schemeWithCore(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	utilruntime.Must(setecv1alpha1.AddToScheme(s))
	utilruntime.Must(corev1.AddToScheme(s))
	return s
}

// fakeClientWithNS builds a fake controller-runtime client pre-seeded with
// optional objects. The core/v1 scheme is always included so namespace objects
// are resolvable.
func fakeClientWithNS(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(schemeWithCore(t)).
		WithObjects(objs...).
		Build()
}

// gateNamespaceLabelled returns a corev1.Namespace whose name is devGateNamespace
// and whose labels carry the allow-dev-runtimes gate.
func gateNamespaceLabelled(label string) *corev1.Namespace {
	return &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   devGateNamespace,
			Labels: map[string]string{label: "true"},
		},
	}
}

// gateNamespaceUnlabelled returns a corev1.Namespace whose name is
// devGateNamespace but without the gate label.
func gateNamespaceUnlabelled() *corev1.Namespace {
	return &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: devGateNamespace},
	}
}

// baseConfig returns a RuntimeConfig that has all four backends enabled, none
// devOnly, and a default of kata-fc. Individual tests override fields as needed.
func baseConfig() *setecruntime.RuntimeConfig {
	return &setecruntime.RuntimeConfig{
		Runtimes: map[string]setecruntime.BackendConfig{
			setecruntime.BackendKataFC:   {Enabled: true, RuntimeClassName: "kata-fc"},
			setecruntime.BackendKataQEMU: {Enabled: true, RuntimeClassName: "kata-qemu"},
			setecruntime.BackendGVisor:   {Enabled: true, RuntimeClassName: "gvisor"},
			setecruntime.BackendRunc:     {Enabled: true, RuntimeClassName: "runc"},
		},
		Defaults: setecruntime.DefaultsConfig{
			Runtime: setecruntime.RuntimeDefaults{Backend: setecruntime.BackendKataFC},
		},
	}
}

// webhookWith returns a SandboxClassWebhook wired to the provided client and
// config. AllowDevLabel defaults to defaultAllowDevLabel.
func webhookWith(c client.Client, cfg *setecruntime.RuntimeConfig) *SandboxClassWebhook {
	return &SandboxClassWebhook{
		Client:     c,
		RuntimeCfg: cfg,
	}
}

// mkSandboxClass constructs a minimal SandboxClass for use in tests.
func mkSandboxClass(name string, vmm setecv1alpha1.VMM, rt *setecv1alpha1.SandboxClassRuntime) *setecv1alpha1.SandboxClass {
	return &setecv1alpha1.SandboxClass{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: setecv1alpha1.SandboxClassSpec{
			VMM:     vmm,
			Runtime: rt,
		},
	}
}

// mkRuntime is a shorthand for building a SandboxClassRuntime with an optional
// fallback list.
func mkRuntime(backend string, fallback ...string) *setecv1alpha1.SandboxClassRuntime {
	return &setecv1alpha1.SandboxClassRuntime{
		Backend:  backend,
		Fallback: fallback,
	}
}

// -------------------------------------------------------------------------
// Defaulting tests
// -------------------------------------------------------------------------

func TestSandboxClassWebhook_Default(t *testing.T) {
	t.Parallel()

	cfg := baseConfig()

	tests := []struct {
		name        string
		class       *setecv1alpha1.SandboxClass
		wantBackend string
	}{
		{
			name:        "no Runtime, VMM=Firecracker → kata-fc",
			class:       mkSandboxClass("fc", setecv1alpha1.VMMFirecracker, nil),
			wantBackend: setecruntime.BackendKataFC,
		},
		{
			name:        "no Runtime, VMM=QEMU → kata-qemu",
			class:       mkSandboxClass("qemu", setecv1alpha1.VMMQEMU, nil),
			wantBackend: setecruntime.BackendKataQEMU,
		},
		{
			name:        "no Runtime, no VMM → cluster default (kata-fc)",
			class:       mkSandboxClass("def", "", nil),
			wantBackend: setecruntime.BackendKataFC,
		},
		{
			name:        "Runtime already set → idempotent, not overwritten",
			class:       mkSandboxClass("idem", setecv1alpha1.VMMFirecracker, mkRuntime(setecruntime.BackendKataQEMU)),
			wantBackend: setecruntime.BackendKataQEMU, // unchanged
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := webhookWith(fakeClientWithNS(t), cfg)
			if err := w.Default(context.Background(), tc.class); err != nil {
				t.Fatalf("Default() error = %v", err)
			}
			if tc.class.Spec.Runtime == nil {
				t.Fatal("Default() left Runtime nil")
			}
			if tc.class.Spec.Runtime.Backend != tc.wantBackend {
				t.Fatalf("Runtime.Backend = %q, want %q",
					tc.class.Spec.Runtime.Backend, tc.wantBackend)
			}
		})
	}
}

// TestSandboxClassWebhook_Default_Idempotent verifies that calling Default
// a second time on the same object does not overwrite what was set by the first
// call. This satisfies the "idempotent" restriction in the task spec.
func TestSandboxClassWebhook_Default_Idempotent(t *testing.T) {
	t.Parallel()
	cfg := baseConfig()
	w := webhookWith(fakeClientWithNS(t), cfg)

	class := mkSandboxClass("x", setecv1alpha1.VMMFirecracker, nil)

	if err := w.Default(context.Background(), class); err != nil {
		t.Fatalf("first Default() error = %v", err)
	}
	wantBackend := class.Spec.Runtime.Backend

	if err := w.Default(context.Background(), class); err != nil {
		t.Fatalf("second Default() error = %v", err)
	}
	if class.Spec.Runtime.Backend != wantBackend {
		t.Fatalf("second Default() changed Backend: got %q, want %q",
			class.Spec.Runtime.Backend, wantBackend)
	}
}

// -------------------------------------------------------------------------
// Validation tests
// -------------------------------------------------------------------------

func TestSandboxClassWebhook_ValidateCreate(t *testing.T) {
	t.Parallel()

	allowLabel := defaultAllowDevLabel

	tests := []struct {
		name    string
		cfg     *setecruntime.RuntimeConfig
		nsObjs  []client.Object // namespace objects to seed in the fake client
		class   *setecv1alpha1.SandboxClass
		wantErr bool
		wantMsg string // substring of the error message; checked only when wantErr=true
	}{
		// --- enabled backend, no devOnly ---
		{
			name:  "backend=kata-fc enabled → accept",
			cfg:   baseConfig(),
			class: mkSandboxClass("fc", "", mkRuntime(setecruntime.BackendKataFC)),
		},
		{
			name:  "backend=gvisor enabled → accept",
			cfg:   baseConfig(),
			class: mkSandboxClass("gv", "", mkRuntime(setecruntime.BackendGVisor)),
		},
		// --- disabled backend ---
		{
			name: "backend=gvisor when disabled → reject",
			cfg: func() *setecruntime.RuntimeConfig {
				c := baseConfig()
				c.Runtimes[setecruntime.BackendGVisor] = setecruntime.BackendConfig{Enabled: false}
				return c
			}(),
			class:   mkSandboxClass("gv", "", mkRuntime(setecruntime.BackendGVisor)),
			wantErr: true,
			wantMsg: "not enabled",
		},
		// --- fallback validations ---
		{
			name: "fallback contains disabled backend → reject",
			cfg: func() *setecruntime.RuntimeConfig {
				c := baseConfig()
				c.Runtimes[setecruntime.BackendKataQEMU] = setecruntime.BackendConfig{Enabled: false}
				return c
			}(),
			class:   mkSandboxClass("x", "", mkRuntime(setecruntime.BackendKataFC, setecruntime.BackendKataQEMU)),
			wantErr: true,
			wantMsg: "not enabled",
		},
		{
			name:    "fallback contains unknown backend → reject",
			cfg:     baseConfig(),
			class:   mkSandboxClass("x", "", mkRuntime(setecruntime.BackendKataFC, "bogus-runtime")),
			wantErr: true,
			wantMsg: "not a recognised backend",
		},
		// --- dev-only (runc) ---
		{
			name: "backend=runc, devOnly=true, gate namespace labelled → accept",
			cfg: func() *setecruntime.RuntimeConfig {
				c := baseConfig()
				c.Runtimes[setecruntime.BackendRunc] = setecruntime.BackendConfig{Enabled: true, DevOnly: true}
				return c
			}(),
			nsObjs: []client.Object{gateNamespaceLabelled(allowLabel)},
			class:  mkSandboxClass("runc", "", mkRuntime(setecruntime.BackendRunc)),
		},
		{
			name: "backend=runc, devOnly=true, gate namespace NOT labelled → reject",
			cfg: func() *setecruntime.RuntimeConfig {
				c := baseConfig()
				c.Runtimes[setecruntime.BackendRunc] = setecruntime.BackendConfig{Enabled: true, DevOnly: true}
				return c
			}(),
			nsObjs:  []client.Object{gateNamespaceUnlabelled()},
			class:   mkSandboxClass("runc", "", mkRuntime(setecruntime.BackendRunc)),
			wantErr: true,
			wantMsg: "dev-only",
		},
		{
			name: "backend=runc, devOnly=false → accept without label",
			cfg: func() *setecruntime.RuntimeConfig {
				c := baseConfig()
				c.Runtimes[setecruntime.BackendRunc] = setecruntime.BackendConfig{Enabled: true, DevOnly: false}
				return c
			}(),
			nsObjs: []client.Object{gateNamespaceUnlabelled()},
			class:  mkSandboxClass("runc", "", mkRuntime(setecruntime.BackendRunc)),
		},
		// --- no Runtime block → skip validation ---
		{
			name:  "Runtime nil → no validation (admissible without Runtime)",
			cfg:   baseConfig(),
			class: mkSandboxClass("legacy", setecv1alpha1.VMMFirecracker, nil),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := fakeClientWithNS(t, tc.nsObjs...)
			w := webhookWith(c, tc.cfg)

			_, err := w.ValidateCreate(context.Background(), tc.class)

			if tc.wantErr {
				if err == nil {
					t.Fatalf("ValidateCreate() expected error containing %q, got nil", tc.wantMsg)
				}
				if tc.wantMsg != "" && !strings.Contains(err.Error(), tc.wantMsg) {
					t.Fatalf("error %q does not contain %q", err.Error(), tc.wantMsg)
				}
				return
			}
			if err != nil {
				t.Fatalf("ValidateCreate() unexpected error: %v", err)
			}
		})
	}
}

func TestSandboxClassWebhook_ValidateUpdate(t *testing.T) {
	t.Parallel()

	// Build a config where gvisor starts disabled and is later enabled.
	cfgGvisorEnabled := baseConfig()

	t.Run("backend changes kata-fc→gvisor with gvisor enabled → accept", func(t *testing.T) {
		t.Parallel()
		c := fakeClientWithNS(t)
		w := webhookWith(c, cfgGvisorEnabled)

		old := mkSandboxClass("cls", "", mkRuntime(setecruntime.BackendKataFC))
		newCls := mkSandboxClass("cls", "", mkRuntime(setecruntime.BackendGVisor))

		_, err := w.ValidateUpdate(context.Background(), old, newCls)
		if err != nil {
			t.Fatalf("ValidateUpdate() unexpected error: %v", err)
		}
	})

	t.Run("backend changes kata-fc→gvisor with gvisor disabled → reject", func(t *testing.T) {
		t.Parallel()
		cfg := baseConfig()
		cfg.Runtimes[setecruntime.BackendGVisor] = setecruntime.BackendConfig{Enabled: false}

		c := fakeClientWithNS(t)
		w := webhookWith(c, cfg)

		old := mkSandboxClass("cls", "", mkRuntime(setecruntime.BackendKataFC))
		newCls := mkSandboxClass("cls", "", mkRuntime(setecruntime.BackendGVisor))

		_, err := w.ValidateUpdate(context.Background(), old, newCls)
		if err == nil {
			t.Fatal("ValidateUpdate() expected rejection of disabled gvisor, got nil")
		}
		if !strings.Contains(err.Error(), "not enabled") {
			t.Fatalf("error %q does not contain 'not enabled'", err.Error())
		}
	})
}

func TestSandboxClassWebhook_ValidateDelete(t *testing.T) {
	t.Parallel()
	w := webhookWith(fakeClientWithNS(t), baseConfig())
	class := mkSandboxClass("any", "", mkRuntime(setecruntime.BackendKataFC))
	warnings, err := w.ValidateDelete(context.Background(), class)
	if err != nil {
		t.Fatalf("ValidateDelete() error = %v, want nil", err)
	}
	if warnings != nil {
		t.Fatalf("ValidateDelete() warnings = %v, want nil", warnings)
	}
}

// TestSandboxClassWebhook_DevOnlyFallback ensures that a devOnly backend in
// the fallback list also triggers the gate check.
func TestSandboxClassWebhook_DevOnlyFallback(t *testing.T) {
	t.Parallel()

	cfg := baseConfig()
	cfg.Runtimes[setecruntime.BackendRunc] = setecruntime.BackendConfig{Enabled: true, DevOnly: true}

	t.Run("devOnly in fallback, gate labelled → accept", func(t *testing.T) {
		t.Parallel()
		c := fakeClientWithNS(t, gateNamespaceLabelled(defaultAllowDevLabel))
		w := webhookWith(c, cfg)
		class := mkSandboxClass("x", "", mkRuntime(setecruntime.BackendKataFC, setecruntime.BackendRunc))
		_, err := w.ValidateCreate(context.Background(), class)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("devOnly in fallback, gate not labelled → reject", func(t *testing.T) {
		t.Parallel()
		c := fakeClientWithNS(t, gateNamespaceUnlabelled())
		w := webhookWith(c, cfg)
		class := mkSandboxClass("x", "", mkRuntime(setecruntime.BackendKataFC, setecruntime.BackendRunc))
		_, err := w.ValidateCreate(context.Background(), class)
		if err == nil {
			t.Fatal("expected error for unlabelled gate namespace")
		}
		if !strings.Contains(err.Error(), "dev-only") {
			t.Fatalf("error %q does not contain 'dev-only'", err.Error())
		}
	})
}
