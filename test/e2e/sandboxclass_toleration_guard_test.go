// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

// This file deliberately carries NO `e2e` build tag.
//
// Every other file in this package is behind `//go:build e2e` and therefore
// only compiles in the job that has a cluster to install into. The defect
// this guard exists to catch (setec#330) is a source-level omission, not a
// runtime one — a SandboxClass built from a bare struct literal carries no
// toleration and its Sandboxes hang `Pending` on a tainted cluster. Catching
// that needs `go test ./...`, which every PR runs, not a nightly e2e job that
// needs a metal node to even start.

package e2e

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSandboxClasses_AreBuiltViaConstructor fails when any file in this
// package builds a SandboxClass *with a Spec* from a raw composite literal
// instead of newSandboxClass().
//
// WHY THE RULE IS "HAS A Spec FIELD" RATHER THAN "IS A SandboxClass LITERAL"
//
// Deleting a cluster-scoped object needs only its name, so
// `&setecv1alpha1.SandboxClass{ObjectMeta: ...}` is legitimate and common in
// t.Cleanup. A literal that sets Spec is a create or an update — the only
// two operations that decide what tolerations the class ends up with — so
// that is the exact and complete set this guard must reject.
//
// The ONLY exemption is newSandboxClass itself, which necessarily builds the
// literal every other site is required to delegate to it. Exempting by
// function rather than by file is deliberate: a whole-file exemption would
// silently cover every future SandboxClass added to that file too.
func TestSandboxClasses_AreBuiltViaConstructor(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob package files: %v", err)
	}
	if len(files) < 5 {
		// Reach floor: an empty or mis-globbed scan must fail rather than
		// pass silently. This package has ~17 files.
		t.Fatalf("scanned only %d files; the guard is not reaching the package", len(files))
	}

	var offenders []string
	scanned, exempted := 0, 0
	for _, path := range files {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		fset := token.NewFileSet()
		// ParseFile ignores build tags, which is exactly what this needs:
		// the guard must see the e2e-tagged files it is policing.
		f, err := parser.ParseFile(fset, path, src, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		scanned++

		for _, decl := range f.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name != nil &&
				fn.Name.Name == constructorName {
				exempted++
				continue
			}
			ast.Inspect(decl, func(n ast.Node) bool {
				lit, ok := n.(*ast.CompositeLit)
				if !ok || !isSandboxClassType(lit.Type) {
					return true
				}
				for _, elt := range lit.Elts {
					kv, ok := elt.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					if id, ok := kv.Key.(*ast.Ident); ok && id.Name == "Spec" {
						pos := fset.Position(lit.Pos())
						offenders = append(offenders,
							filepath.Base(pos.Filename)+":"+itoa(pos.Line))
					}
				}
				return true
			})
		}
	}

	if scanned == 0 {
		t.Fatal("guard scanned no files; it is not reaching the package")
	}
	// The constructor must exist and must have been the thing we skipped. If
	// it is renamed or deleted, the exemption silently starts covering
	// nothing — or worse, the guard passes because every call site was
	// rewritten to something it no longer recognises.
	if exempted != 1 {
		t.Fatalf("expected to exempt exactly one %s declaration, exempted %d; "+
			"was it renamed or duplicated?", constructorName, exempted)
	}
	if len(offenders) > 0 {
		t.Errorf("SandboxClass built with a Spec from a raw literal at %s\n"+
			"Use newSandboxClass(name, spec) instead. A raw literal carries no\n"+
			"sandbox-host toleration, so on a cluster whose KVM nodes are\n"+
			"tainted the Sandbox is steered at a node it cannot land on and\n"+
			"sits Pending until timeout, with the only evidence a\n"+
			"FailedScheduling event on the Pod (setec#330).",
			strings.Join(offenders, ", "))
	}
}

// constructorName is the sole exempt function: the one that legitimately
// builds the literal every other site must delegate to it.
const constructorName = "newSandboxClass"

// isSandboxClassType reports whether a composite-literal type names
// SandboxClass, in either the qualified (setecv1alpha1.SandboxClass) or bare
// form.
func isSandboxClassType(e ast.Expr) bool {
	switch t := e.(type) {
	case *ast.SelectorExpr:
		return t.Sel != nil && t.Sel.Name == "SandboxClass"
	case *ast.Ident:
		return t.Name == "SandboxClass"
	}
	return false
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
