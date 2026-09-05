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

package poolentry

import (
	"errors"
	"strings"
	"testing"
)

func cleanVerdict() ScanVerdict {
	return ScanVerdict{
		ScannerVersion: "v1-abcdefabcdef",
		Clean:          true,
		StateSHA256:    strings.Repeat("a", 64),
		MemorySHA256:   strings.Repeat("b", 64),
	}
}

func TestScanVerdict_Roundtrip(t *testing.T) {
	dir := t.TempDir()
	want := cleanVerdict()
	if err := WriteScan(dir, want); err != nil {
		t.Fatalf("WriteScan: %v", err)
	}
	got, err := ReadScan(dir)
	if err != nil {
		t.Fatalf("ReadScan: %v", err)
	}
	if got != want {
		t.Fatalf("roundtrip mismatch: got %+v want %+v", got, want)
	}
	if v, err := VerifyScan(dir); err != nil || v != want {
		t.Fatalf("VerifyScan = %+v, %v; want the clean verdict and nil", v, err)
	}
}

// TestVerifyScan_FailsClosed pins invariant 1's evidence rules: a
// missing, dirty, or incomplete verdict is a violation — absence of
// evidence is never evidence of absence.
func TestVerifyScan_FailsClosed(t *testing.T) {
	mutations := map[string]func(*ScanVerdict){
		"dirty":              func(v *ScanVerdict) { v.Clean = false },
		"no-scanner-version": func(v *ScanVerdict) { v.ScannerVersion = "" },
		"no-state-digest":    func(v *ScanVerdict) { v.StateSHA256 = "" },
		"no-memory-digest":   func(v *ScanVerdict) { v.MemorySHA256 = "" },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			v := cleanVerdict()
			mutate(&v)
			if err := WriteScan(dir, v); err != nil {
				t.Fatalf("WriteScan: %v", err)
			}
			if _, err := VerifyScan(dir); !errors.Is(err, ErrScanVerdict) {
				t.Fatalf("VerifyScan error = %v, want ErrScanVerdict", err)
			}
		})
	}

	t.Run("missing", func(t *testing.T) {
		if _, err := VerifyScan(t.TempDir()); !errors.Is(err, ErrScanVerdict) {
			t.Fatalf("VerifyScan error = %v, want ErrScanVerdict for a missing record", err)
		}
	})
}

// TestDEKAAD_BindsScanVerdict: any change to the recorded verdict —
// like any change to identity or provenance — must change the AAD, so
// a sealed DEK cannot survive a swapped record.
func TestDEKAAD_BindsScanVerdict(t *testing.T) {
	prov := Provenance{Source: SourceClassImageBoot, ImageRef: "img:v1"}
	base := DEKAAD("e-1", prov, cleanVerdict())

	mutations := map[string]func(*ScanVerdict){
		"scanner-version": func(v *ScanVerdict) { v.ScannerVersion = "v1-000000000000" },
		"clean-flag":      func(v *ScanVerdict) { v.Clean = false },
		"state-digest":    func(v *ScanVerdict) { v.StateSHA256 = strings.Repeat("c", 64) },
		"memory-digest":   func(v *ScanVerdict) { v.MemorySHA256 = strings.Repeat("d", 64) },
	}
	for name, mutate := range mutations {
		v := cleanVerdict()
		mutate(&v)
		if DEKAAD("e-1", prov, v) == base {
			t.Fatalf("mutating the verdict's %s did not change the AAD", name)
		}
	}
	if DEKAAD("e-2", prov, cleanVerdict()) == base {
		t.Fatal("changing the entry id did not change the AAD")
	}
}
