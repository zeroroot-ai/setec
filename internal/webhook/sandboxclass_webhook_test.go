// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package webhook

import (
	"context"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
	setecruntime "github.com/zeroroot-ai/setec/internal/runtime"
)

// schemeWithCore returns a scheme with the setec CRDs and the core types.
func schemeWithCore(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(s))
	utilruntime.Must(setecv1alpha1.AddToScheme(s))
	return s
}

// fakeClientWithNS returns a fake client that holds objs.
func fakeClientWithNS(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(schemeWithCore(t)).WithObjects(objs...).Build()
}

// classWebhook returns a SandboxClassWebhook on an empty fake client.
func classWebhook(t *testing.T) *SandboxClassWebhook {
	t.Helper()
	return &SandboxClassWebhook{Client: fakeClientWithNS(t)}
}

// mkSandboxClass constructs a minimal SandboxClass for use in tests.
func mkSandboxClass(name string, rt *setecv1alpha1.SandboxClassRuntime) *setecv1alpha1.SandboxClass {
	return &setecv1alpha1.SandboxClass{Name: name, Spec: setecv1alpha1.SandboxClassSpec{Runtime: rt}}
}

// mkRuntime builds a SandboxClassRuntime for one backend.
func mkRuntime(backend string) *setecv1alpha1.SandboxClassRuntime {
	return &setecv1alpha1.SandboxClassRuntime{Backend: backend}
}

// TestSandboxClassWebhook_DefaultIsTheLauncher proves that a class with no
// runtime gets the one backend, and that Default keeps a set runtime.
func TestSandboxClassWebhook_DefaultIsTheLauncher(t *testing.T) {
	t.Parallel()
	w := classWebhook(t)
	cls := mkSandboxClass("plain", nil)
	if err := w.Default(context.Background(), cls); err != nil {
		t.Fatal(err)
	}
	if cls.Spec.Runtime == nil || cls.Spec.Runtime.Backend != setecruntime.BackendLauncher {
		t.Fatalf("runtime = %+v, want the launcher", cls.Spec.Runtime)
	}
	set := mkSandboxClass("set", mkRuntime(""))
	if err := w.Default(context.Background(), set); err != nil || set.Spec.Runtime.Backend != "" {
		t.Fatalf("Default changed a set runtime: %+v, %v", set.Spec.Runtime, err)
	}
}

// TestSandboxClassWebhook_RefusesARemovedBackend pins the cutover of
// setec#198 at admission: a class that names a removed backend is
// refused with the reason, and the launcher passes on create, on
// update and with no runtime.
func TestSandboxClassWebhook_RefusesARemovedBackend(t *testing.T) {
	t.Parallel()
	w := classWebhook(t)
	ctx := context.Background()
	for _, removed := range setecruntime.RemovedBackends {
		cls := mkSandboxClass("old", mkRuntime(removed))
		if _, err := w.ValidateCreate(ctx, cls); err == nil || !strings.Contains(err.Error(), "was removed") {
			t.Errorf("create with %q = %v, want the removal reason", removed, err)
		}
		if _, err := w.ValidateUpdate(ctx, nil, cls); err == nil {
			t.Errorf("update to %q was accepted", removed)
		}
	}
	for _, ok := range []*setecv1alpha1.SandboxClass{
		mkSandboxClass("launcher", mkRuntime(setecruntime.BackendLauncher)),
		mkSandboxClass("empty", mkRuntime("")),
		mkSandboxClass("nil", nil),
	} {
		if _, err := w.ValidateCreate(ctx, ok); err != nil {
			t.Errorf("%s: %v", ok.Name, err)
		}
	}
	if _, err := w.ValidateDelete(ctx, mkSandboxClass("old", mkRuntime(setecruntime.RemovedBackends[0]))); err != nil {
		t.Errorf("delete of a class with a removed backend: %v", err)
	}
}
