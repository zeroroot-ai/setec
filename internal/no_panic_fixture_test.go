// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package internal_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	astchecks "github.com/zeroroot-ai/ast-checks"
)

// TestNoPanicInInternal_FindsAPlantedPanic is the failing fixture of
// TestNoPanicInInternal (gibson#807 condition 7). It walks a planted tree
// with the same matcher and options, and requires one finding: the bare
// panic in production code. The panic in a _test.go file and the method
// named panic on a value must not count.
func TestNoPanicInInternal_FindsAPlantedPanic(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"internal/bad/bad.go":       "package bad\n\nfunc F() { panic(\"boom\") }\n",
		"internal/bad/bad_test.go":  "package bad\n\nfunc helper() { panic(\"in a test\") }\n",
		"internal/good/good.go":     "package good\n\ntype p struct{}\n\nfunc (p) panic() {}\n\nfunc G() { p{}.panic() }\n",
		"internal/good/errorish.go": "package good\n\nimport \"errors\"\n\nfunc H() error { return errors.New(\"x\") }\n",
	}
	for rel, src := range files {
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}

	findings, err := astchecks.Walk(astchecks.WalkOpts{
		ScopeDirs:     []string{filepath.Join(root, "internal")},
		RepoRoot:      root,
		Matchers:      []astchecks.Matcher{panicMatcher{}},
		Allowlist:     astchecks.Allowlist{},
		SkipTestFiles: true,
		SkipGenerated: true,
	})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("want 1 finding (internal/bad/bad.go), got %d:\n%s",
			len(findings), astchecks.RenderFindings(findings))
	}
	if got := findings[0].String(); !strings.Contains(got, "bad.go") || strings.Contains(got, "_test.go") {
		t.Fatalf("the finding names the wrong file: %s", got)
	}
}
