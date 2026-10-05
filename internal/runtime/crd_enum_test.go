// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package runtime

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"
)

// TestSandboxClassCRDAcceptsEveryKnownBackend proves that the API server
// accepts each backend that the operator knows. The CRD enum is a second
// list of backends. A backend that is missing from it is refused before the
// webhook or the operator see the SandboxClass.
func TestSandboxClassCRDAcceptsEveryKnownBackend(t *testing.T) {
	t.Parallel()
	for _, path := range []string{
		"config/crd/bases/setec.zeroroot.ai_sandboxclasses.yaml",
		"charts/setec/crds/setec.zeroroot.ai_sandboxclasses.yaml",
	} {
		raw, err := os.ReadFile(filepath.Join("..", "..", filepath.FromSlash(path)))
		if err != nil {
			t.Fatal(err)
		}
		var crd apiextensionsv1.CustomResourceDefinition
		if err := yaml.Unmarshal(raw, &crd); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if len(crd.Spec.Versions) == 0 {
			t.Fatalf("%s: the CRD has no version", path)
		}
		for _, v := range crd.Spec.Versions {
			backend := v.Schema.OpenAPIV3Schema.Properties["spec"].Properties["runtime"].Properties["backend"]
			var enum []string
			for _, e := range backend.Enum {
				var s string
				if err := yaml.Unmarshal(e.Raw, &s); err != nil {
					t.Fatalf("%s %s: enum value %s: %v", path, v.Name, e.Raw, err)
				}
				enum = append(enum, s)
			}
			slices.Sort(enum)
			if !slices.Equal(enum, AllKnownBackends) {
				t.Errorf("%s %s: spec.runtime.backend enum = %v, want AllKnownBackends %v",
					path, v.Name, enum, AllKnownBackends)
			}
		}
	}
}
