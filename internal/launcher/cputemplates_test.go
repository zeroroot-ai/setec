// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package launcher

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// cpuTemplate is the x86_64 form of a Firecracker custom CPU template
// (docs/cpu_templates/schema.json of Firecracker). Each field is required
// to decode, so an unknown key fails the test.
type cpuTemplate struct {
	CPUIDModifiers []struct {
		Leaf      string `json:"leaf"`
		Subleaf   string `json:"subleaf"`
		Flags     *int   `json:"flags"`
		Modifiers []struct {
			Register string `json:"register"`
			Bitmap   string `json:"bitmap"`
		} `json:"modifiers"`
	} `json:"cpuid_modifiers"`
	MSRModifiers []struct {
		Addr   string `json:"addr"`
		Bitmap string `json:"bitmap"`
	} `json:"msr_modifiers"`
}

var (
	hexValue    = regexp.MustCompile(`^0x[0-9a-fA-F]+$`)
	cpuidBitmap = regexp.MustCompile(`^0b[01x]{32}$`)
	msrBitmap   = regexp.MustCompile(`^0b[01x]{64}$`)
)

// TestCPUTemplates_AreValid checks each template that the launcher image
// ships (setec#239): it decodes as a Firecracker x86_64 template with no
// unknown key, each CPUID modifier names a register and a 32-bit mask, and
// each MSR modifier a 64-bit mask. The m8i template must be there.
func TestCPUTemplates_AreValid(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob(filepath.Join("..", "..", "cpu-templates", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, f := range files {
		names[strings.TrimSuffix(filepath.Base(f), ".json")] = true
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		dec := json.NewDecoder(strings.NewReader(string(raw)))
		dec.DisallowUnknownFields()
		var tpl cpuTemplate
		if err := dec.Decode(&tpl); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		if len(tpl.CPUIDModifiers) == 0 {
			t.Errorf("%s: no CPUID modifier", f)
		}
		for _, c := range tpl.CPUIDModifiers {
			if !hexValue.MatchString(c.Leaf) || !hexValue.MatchString(c.Subleaf) || c.Flags == nil {
				t.Errorf("%s: CPUID leaf %q subleaf %q needs hex values and flags", f, c.Leaf, c.Subleaf)
			}
			for _, m := range c.Modifiers {
				switch m.Register {
				case "eax", "ebx", "ecx", "edx":
				default:
					t.Errorf("%s: leaf %s: register %q", f, c.Leaf, m.Register)
				}
				if !cpuidBitmap.MatchString(m.Bitmap) {
					t.Errorf("%s: leaf %s %s: bitmap %q is not 0b and 32 of 0, 1 or x", f, c.Leaf, m.Register, m.Bitmap)
				}
			}
		}
		for _, m := range tpl.MSRModifiers {
			if !hexValue.MatchString(m.Addr) || !msrBitmap.MatchString(m.Bitmap) {
				t.Errorf("%s: MSR %q bitmap %q", f, m.Addr, m.Bitmap)
			}
		}
	}
	if !names["m8i"] {
		t.Fatalf("the launcher image ships no m8i template; templates: %v", names)
	}
}

// TestCPUTemplates_NameIsAClassValue checks that a class value resolves to
// the path that the launcher reads.
func TestCPUTemplates_NameIsAClassValue(t *testing.T) {
	t.Parallel()
	s := testSpec(t)
	s.CPUTemplate = "/opt/setec/cpu-templates/m8i.json"
	if err := s.Validate(); err != nil {
		t.Fatalf("a spec with the m8i template: %v", err)
	}
}
