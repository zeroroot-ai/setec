// Copyright 2026 The Setec Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package internal_test

import (
	"go/ast"
	"go/token"
	"path/filepath"
	"runtime"
	"testing"

	astchecks "github.com/zero-day-ai/ast-checks"
)

// panicMatcher is a local Matcher implementation that flags bare panic(...)
// calls. The ast-checks ForbiddenCallsite primitive matches only qualified
// pkg.Func call shapes (ast.SelectorExpr); panic() is a builtin whose AST
// node is a bare ast.Ident — so we extend via the public Matcher interface
// rather than stretching ForbiddenCallsite.
//
// This is the canonical extension pattern for builtins: implement the three
// Matcher methods, pass the instance to Walk via WalkOpts.Matchers.
type panicMatcher struct{}

func (panicMatcher) Name() string { return "ForbiddenCallsite" }
func (panicMatcher) Rule() string {
	return "no panic() in operator internal packages — use error returns"
}

func (panicMatcher) Match(fset *token.FileSet, node ast.Node, src []byte) (bool, string) {
	call, ok := node.(*ast.CallExpr)
	if !ok {
		return false, ""
	}
	ident, ok := call.Fun.(*ast.Ident)
	if !ok {
		return false, ""
	}
	if ident.Name != "panic" {
		return false, ""
	}
	// Render a one-line snippet for the finding.
	pos := fset.Position(call.Pos())
	end := fset.Position(call.End())
	if pos.Offset >= 0 && end.Offset > pos.Offset && end.Offset <= len(src) {
		snippet := string(src[pos.Offset:end.Offset])
		if len(snippet) > 80 {
			snippet = snippet[:77] + "..."
		}
		return true, snippet
	}
	return true, "panic(...)"
}

// TestNoPanicInInternal asserts that no Go file under setec's internal/
// packages calls panic() at runtime. Setec is a Kubernetes operator; a panic
// in reconciler or controller code kills the operator process, drops all
// in-flight reconciliations, and requires a pod restart — exactly the kind of
// non-graceful failure an operator must never inflict on the cluster.
//
// Legitimate uses of panic (e.g. must-succeed initialization) belong only in
// cmd/main.go (startup) or _test.go files, both of which are excluded from
// this walk.
//
// If you discover a pre-existing panic call that needs to be fixed
// incrementally, add it to the allowlist below with a CategoryLegacyOptional
// tag and a follow-up issue reference, then open the fix as a separate PR.
//
// Implements slice 3.3 of the production-readiness epic
// (zeroroot-ai/.github#50).
func TestNoPanicInInternal(t *testing.T) {
	_, thisFile, _, _ := runtime.Caller(0)
	// thisFile is at internal/no_panic_in_internal_test.go; repo root is
	// one level up.
	repoRoot := filepath.Join(filepath.Dir(thisFile), "..")
	internalRoot := filepath.Join(repoRoot, "internal")

	matchers := []astchecks.Matcher{panicMatcher{}}

	// Existing-debt allowlist. Each entry is a "<file>:<line>" coordinate
	// relative to the repo root. Add entries here only for pre-existing
	// violations that cannot be fixed in this PR; every new entry MUST have
	// a follow-up issue reference in IssueURL.
	//
	// On first bootstrap run this list is empty — setec/internal has no
	// panic() calls today (confirmed 2026-05-19).
	allowlist := astchecks.Allowlist{}

	opts := astchecks.WalkOpts{
		ScopeDirs:     []string{internalRoot},
		RepoRoot:      repoRoot,
		Matchers:      matchers,
		Allowlist:     allowlist,
		SkipTestFiles: true,
		SkipGenerated: true,
	}

	findings, err := astchecks.Walk(opts)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}

	if len(findings) > 0 {
		t.Errorf("panic() calls found in setec/internal (forbidden — use error returns):\n%s\n\n"+
			"Panics in operator internals kill the controller process and drop\n"+
			"in-flight reconciliations. Replace with explicit error returns.\n"+
			"If this is a genuine short-term exception, add it to the allowlist\n"+
			"in this file with a CategoryLegacyOptional tag and an issue URL.\n",
			astchecks.RenderFindings(findings))
	}

	t.Logf("allowlisted entries: %d", len(allowlist))
}
