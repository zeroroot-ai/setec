// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package class

import (
	"bytes"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// renderClasses renders templates/sandboxclass-default.yaml with the values
// and returns each SandboxClass spec by name, or the helm error output.
func renderClasses(t *testing.T, sets ...string) (map[string]map[string]any, string) {
	t.Helper()
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Fatalf("this test renders the chart and needs helm on PATH: %v", err)
	}
	args := make([]string, 0, 12+2*len(sets))
	args = append(args, "template", "setec", filepath.Join("..", "..", "charts", "setec"),
		"-f", filepath.Join("..", "..", "hack", "chart-launcher-values.yaml"),
		"--set", "webhook.certManager.enabled=true",
		"--set", "sandboxNamespaces={sandbox-workloads}",
		"--show-only", "templates/sandboxclass-default.yaml")
	for _, s := range sets {
		args = append(args, "--set", s)
	}
	var out, errb bytes.Buffer
	cmd := exec.Command(helm, args...) //nolint:gosec // fixed arguments
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return nil, errb.String()
	}
	specs := map[string]map[string]any{}
	for _, doc := range strings.Split(out.String(), "\n---") {
		var obj struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Spec map[string]any `json:"spec"`
		}
		if err := yaml.Unmarshal([]byte(doc), &obj); err != nil {
			t.Fatalf("the render is not YAML: %v", err)
		}
		if obj.Metadata.Name != "" {
			specs[obj.Metadata.Name] = obj.Spec
		}
	}
	return specs, ""
}

// TestSandboxClassChart_SpecOverrideChangesOneClass checks that an override
// replaces the named key of one class, and leaves the other classes as the
// chart default renders them (gibson#498).
func TestSandboxClassChart_SpecOverrideChangesOneClass(t *testing.T) {
	t.Parallel()
	base, errOut := renderClasses(t)
	if errOut != "" {
		t.Fatalf("the default render failed: %s", errOut)
	}
	got, errOut := renderClasses(t,
		"sandboxClasses.specOverrides.tool.egressExemptCIDRs={10.60.0.66/32}",
		"sandboxClasses.specOverrides.tool.default=false")
	if errOut != "" {
		t.Fatalf("the render with an override failed: %s", errOut)
	}
	cidrs, _ := got["tool"]["egressExemptCIDRs"].([]any)
	if len(cidrs) != 1 || cidrs[0] != "10.60.0.66/32" {
		t.Errorf("tool egressExemptCIDRs = %v, want [10.60.0.66/32]", got["tool"]["egressExemptCIDRs"])
	}
	// A false value replaces too. A deep merge would drop it.
	if got["tool"]["default"] != false {
		t.Errorf("tool default = %v, want false", got["tool"]["default"])
	}
	if got["tool"]["defaultNetworkMode"] != base["tool"]["defaultNetworkMode"] {
		t.Errorf("tool defaultNetworkMode changed: %v", got["tool"]["defaultNetworkMode"])
	}
	for name, spec := range base {
		if name == "tool" {
			continue
		}
		a, _ := yaml.Marshal(spec)
		b, _ := yaml.Marshal(got[name])
		if !bytes.Equal(a, b) {
			t.Errorf("class %q changed without an override:\n%s\nwant:\n%s", name, b, a)
		}
	}
}

// TestSandboxClassChart_SpecOverrideOfAnUnknownClassFails checks that the
// render refuses an override whose key names no class.
func TestSandboxClassChart_SpecOverrideOfAnUnknownClassFails(t *testing.T) {
	t.Parallel()
	_, errOut := renderClasses(t, "sandboxClasses.specOverrides.nope.default=true")
	if !strings.Contains(errOut, `sandboxClasses.specOverrides names class "nope"`) {
		t.Fatalf("the render did not refuse the unknown class, helm said: %q", errOut)
	}
}
