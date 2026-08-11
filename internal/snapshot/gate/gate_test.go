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

package gate

import (
	"context"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	setecv1alpha1 "github.com/zeroroot-ai/setec/api/v1alpha1"
)

// fullEvidence returns Evidence with every invariant confirmed.
func fullEvidence() Evidence {
	return Evidence{
		CleanBase:          true,
		EntropyReseeded:    true,
		IdentityUniquified: true,
		SingleSession:      true,
		ProvenanceVerified: true,
		EncryptedAtRest:    true,
	}
}

func TestEvaluate_FullEvidencePasses(t *testing.T) {
	if v := Evaluate(fullEvidence()); len(v) != 0 {
		t.Fatalf("violations = %v, want none", v)
	}
}

func TestEvaluate_ZeroValueFailsEverything(t *testing.T) {
	got := Evaluate(Evidence{})
	want := []Invariant{
		InvariantCleanBase,
		InvariantEncryptedAtRest,
		InvariantSingleSession,
		InvariantUniquified,
		InvariantProvenance,
	}
	// Evaluate returns a sorted list; sort want the same way.
	if len(got) != 5 {
		t.Fatalf("violations = %v, want all five", got)
	}
	seen := map[Invariant]bool{}
	for _, v := range got {
		seen[v] = true
	}
	for _, w := range want {
		if !seen[w] {
			t.Fatalf("missing violation %q in %v", w, got)
		}
	}
}

func TestEvaluate_EachSingleViolation(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Evidence)
		want   Invariant
	}{
		{"clean-base", func(e *Evidence) { e.CleanBase = false }, InvariantCleanBase},
		{"reseed", func(e *Evidence) { e.EntropyReseeded = false }, InvariantUniquified},
		{"identity", func(e *Evidence) { e.IdentityUniquified = false }, InvariantUniquified},
		{"session", func(e *Evidence) { e.SingleSession = false }, InvariantSingleSession},
		{"provenance", func(e *Evidence) { e.ProvenanceVerified = false }, InvariantProvenance},
		{"atrest", func(e *Evidence) { e.EncryptedAtRest = false }, InvariantEncryptedAtRest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := fullEvidence()
			tc.mutate(&ev)
			got := Evaluate(ev)
			if !reflect.DeepEqual(got, []Invariant{tc.want}) {
				t.Fatalf("violations = %v, want [%s]", got, tc.want)
			}
		})
	}
}

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := setecv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func fakeReader(t *testing.T, objs ...client.Object) client.Reader {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(objs...).Build()
}

func annotatedClass() *setecv1alpha1.SandboxClass {
	return &setecv1alpha1.SandboxClass{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "dev",
			Annotations: map[string]string{AllowUnverifiedRestoresAnnotation: "true"},
		},
	}
}

func devNamespace(labelled bool) *corev1.Namespace {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: DefaultGateNamespace}}
	if labelled {
		ns.Labels = map[string]string{DefaultAllowDevLabel: "true"}
	}
	return ns
}

func TestDecide_ViolationRefusedWithoutOptOut(t *testing.T) {
	g := &Gate{Reader: fakeReader(t, devNamespace(true))}
	ev := fullEvidence()
	ev.EntropyReseeded = false

	// No annotation on the class: refused even with the dev label.
	d, err := g.Decide(context.Background(), &setecv1alpha1.SandboxClass{}, ev)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if d.Allowed {
		t.Fatal("unverified restore allowed without the class annotation")
	}
	if len(d.Violations) != 1 || d.Violations[0] != InvariantUniquified {
		t.Fatalf("violations = %v", d.Violations)
	}
}

func TestDecide_AnnotationWithoutDevLabelRefused(t *testing.T) {
	g := &Gate{Reader: fakeReader(t, devNamespace(false))}
	ev := fullEvidence()
	ev.EncryptedAtRest = false

	d, err := g.Decide(context.Background(), annotatedClass(), ev)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if d.Allowed {
		t.Fatal("annotation alone must not grant the opt-out (cluster dev label absent)")
	}
}

func TestDecide_OptOutAllowsButReportsViolations(t *testing.T) {
	g := &Gate{Reader: fakeReader(t, devNamespace(true))}
	ev := fullEvidence()
	ev.ProvenanceVerified = false

	d, err := g.Decide(context.Background(), annotatedClass(), ev)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if !d.Allowed || !d.DevOptOut {
		t.Fatalf("decision = %+v, want allowed via dev opt-out", d)
	}
	if len(d.Violations) != 1 || d.Violations[0] != InvariantProvenance {
		t.Fatalf("violations must still be reported under the opt-out: %v", d.Violations)
	}
}

func TestDecide_NilGateFailsClosed(t *testing.T) {
	var g *Gate
	ev := fullEvidence()
	ev.CleanBase = false
	d, err := g.Decide(context.Background(), annotatedClass(), ev)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if d.Allowed {
		t.Fatal("nil gate must never grant the opt-out")
	}
}

func TestDecide_UnreadableGateNamespaceFailsClosed(t *testing.T) {
	// Reader with no namespace object: the Get fails with NotFound and
	// the opt-out resolution fails closed.
	g := &Gate{Reader: fakeReader(t)}
	ev := fullEvidence()
	ev.SingleSession = false
	d, err := g.Decide(context.Background(), annotatedClass(), ev)
	if err == nil {
		t.Fatal("expected the opt-out resolution error to surface")
	}
	if d.Allowed {
		t.Fatal("unreadable gate namespace must fail closed")
	}
}

func TestDecide_FullEvidenceNeedsNoOptOut(t *testing.T) {
	var g *Gate
	d, err := g.Decide(context.Background(), nil, fullEvidence())
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if !d.Allowed || d.DevOptOut || len(d.Violations) != 0 {
		t.Fatalf("decision = %+v, want plainly allowed", d)
	}
}
