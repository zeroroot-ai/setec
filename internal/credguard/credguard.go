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

// Package credguard fails the build when an mTLS credential is
// assembled anywhere but internal/credentials.
//
// Four setec surfaces — the frontend server, the node-agent server, the
// snapshot dialer and the tracing exporter — each grew their own
// tls.Config by hand. Nothing stopped the fourth. internal/credentials
// now owns all four; this package is what stops the fifth. It is the
// difference between a state of the tree and a property of it.
//
// # What it looks at
//
// Every .go file under the scan root, parsed for references to the
// packages an mTLS credential can only be built out of. It resolves
// each file's imports first, so the check is on the *package* a symbol
// came from and not on the spelling of the qualifier: importing
// crypto/tls under another name, or dot-importing it, does not evade
// the guard.
//
// No type checking and no build is involved, so the guard sees the
// examples/ directories too, which are separate Go modules the root
// module's tooling cannot otherwise reach.
//
// # Tests are in scope
//
// A _test.go file is scanned exactly like production code. Exempting
// tests as a class would leave the obvious hole open: a helper that
// builds a bypassing tls.Config lives in a _test.go file, production
// code grows a caller, and the guard never had an opinion. The two
// test files that legitimately hand-build a client config are exempt
// by name, in Exemptions, with a reason each.
//
// # Failing closed
//
// A guard that passes having examined nothing is worse than no guard,
// because it also reports success. Scan therefore treats each of these
// as an error rather than a pass: a scan root that does not exist or is
// not a directory, a root holding no Go files at all, a root where
// every Go file is exempt, a file that will not parse, an exemption
// naming a path that is not there, and an exemption that no longer
// covers anything the guard would have flagged.
package credguard

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
)

// symbolPolicy says how references to a watched package are judged.
type symbolPolicy int

const (
	// importForbidden means importing the package at all is a
	// violation. It is for packages with no legitimate use outside the
	// credential module, where naming individual symbols would only
	// invite an argument about which ones.
	importForbidden symbolPolicy = iota

	// allowListed means every symbol of the package is forbidden
	// except the ones named. It is the right shape where the package's
	// credential-building surface is most of it and the harmless part
	// is small and enumerable — a symbol nobody thought about is red,
	// which is the direction a security guard should fail in.
	allowListed

	// denyListed means every symbol is permitted except the ones
	// named. It is for packages with a large, legitimate,
	// non-credential surface, where an allow-list would be a list of
	// everything.
	denyListed
)

// watched is one package the guard has an opinion about.
type watched struct {
	// importPath is the package's import path.
	importPath string
	// matchPrefix treats importPath as covering its whole subtree.
	matchPrefix bool
	// policy selects how symbols is read.
	policy symbolPolicy
	// symbols maps a symbol name to the reason it is listed. Under
	// allowListed the reason says why the symbol is harmless; under
	// denyListed it says why the symbol builds a credential. It is
	// unused under importForbidden.
	symbols map[string]string
	// why states what the package has to do with mTLS credentials. It
	// is quoted in the violation so a contributor who trips the guard
	// reads the argument rather than just the rule.
	why string
}

// watchedPackages is the full set of packages the guard watches.
//
// The list is deliberately short. An mTLS credential in Go is a
// keypair, a set of trust anchors, and a tls.Config tying them
// together; everything below is one of those three or the SPIFFE
// machinery for obtaining them.
var watchedPackages = []watched{
	{
		importPath: "crypto/tls",
		policy:     allowListed,
		why: "crypto/tls is where a TLS credential is assembled; setec assembles " +
			"them only in internal/credentials, which is what makes the TLS floor, " +
			"the mandatory client certificate and the peer-authorization hook " +
			"properties of every setec hop rather than of whichever call site " +
			"remembered them",
		symbols: map[string]string{
			"ConnectionState": "reads an already-established connection to extract the " +
				"peer's certificate; it configures nothing and cannot weaken a handshake",
			"VersionTLS12": "a version constant, used to assert a floor rather than to set one",
			"VersionTLS13": "a version constant, used to assert a floor rather than to set one",
		},
	},
	{
		importPath: "google.golang.org/grpc/credentials",
		policy:     allowListed,
		why: "the constructors in this package turn a tls.Config into gRPC transport " +
			"credentials, which is the last step of building one; internal/credentials " +
			"is the only place in setec that takes it",
		symbols: map[string]string{
			"TransportCredentials": "the interface internal/credentials hands out, and the " +
				"type every consumer names to receive it",
			"TLSInfo": "peer information off an established connection, used to identify a " +
				"caller rather than to configure one",
			"AuthInfo": "the interface TLSInfo satisfies, named when narrowing a peer",
		},
	},
	{
		importPath: "crypto/x509",
		policy:     denyListed,
		why: "the pool a peer's chain is verified against is half of an mTLS credential; " +
			"the rest of crypto/x509 is certificate parsing, which is ordinary",
		symbols: map[string]string{
			"NewCertPool": "assembles the trust anchors a peer is verified against, which is " +
				"internal/credentials' half of the credential",
			"SystemCertPool": "adopts the host trust store as the anchor set, silently widening " +
				"who is accepted",
		},
	},
	{
		importPath:  "github.com/spiffe/go-spiffe",
		matchPrefix: true,
		policy:      importForbidden,
		why: "the SPIFFE Workload API is a credential source, and internal/credentials " +
			"is the only consumer of it in setec; a second one would be a second answer " +
			"to which peers are authorized",
	},
}

// skippedDirs are directory names the walk does not descend into.
// None of them holds compiled Go source: .git is object storage, and
// bin and dist are build output. Everything else in the tree is
// scanned, including testdata and the separate modules under examples.
var skippedDirs = map[string]bool{".git": true, "bin": true, "dist": true, "vendor": true}

// Exemption is one allow-list entry: a path the guard does not judge,
// and the reason it does not.
//
// There is no way to exempt a path without stating why, and no silent
// skip anywhere in the guard. An exemption whose path has gone away, or
// that no longer covers anything the guard would have flagged, is an
// error — so the list cannot quietly decay into a list of places nobody
// remembers exempting.
type Exemption struct {
	// Path is slash-separated and relative to the scan root. It names
	// either one file or one directory and its subtree.
	Path string
	// Reason states why this path may build its own credential.
	Reason string
}

// covers reports whether rel, a slash path relative to the scan root,
// falls under this exemption.
func (e Exemption) covers(rel string) bool {
	return rel == e.Path || strings.HasPrefix(rel, e.Path+"/")
}

// Violation is one reference the guard rejects.
type Violation struct {
	// File is the slash path relative to the scan root.
	File string
	// Line is the line the reference is on.
	Line int
	// Import is the watched package the reference resolved to.
	Import string
	// Symbol is the referenced symbol, empty when the import itself is
	// the violation.
	Symbol string
	// Why is the watched package's rationale.
	Why string
}

// String renders the violation as one reviewable line.
func (v Violation) String() string {
	ref := v.Import
	if v.Symbol != "" {
		ref = path.Base(v.Import) + "." + v.Symbol + " (" + v.Import + ")"
	}
	return fmt.Sprintf("%s:%d: %s\n        %s", v.File, v.Line, ref, v.Why)
}

// Report is the outcome of a scan.
type Report struct {
	// GoFiles is how many .go files the walk found, exempt or not.
	GoFiles int
	// Inspected is how many of them the guard actually judged.
	Inspected int
	// Exempted is how many were skipped by the allow-list.
	Exempted int
	// Violations are the rejected references, in walk order.
	Violations []Violation
	// StaleExemptions name paths that are not in the tree.
	StaleExemptions []Exemption
	// ObsoleteExemptions cover paths that exist but hold nothing the
	// guard would have flagged, so the exemption is no longer earning
	// its place.
	ObsoleteExemptions []Exemption
}

// Check scans root and returns an error describing everything wrong, or
// nil. It is the whole of the guard's contract; Scan is exported for
// callers that want the counts.
func Check(root string, exemptions []Exemption) error {
	report, err := Scan(root, exemptions)
	if err != nil {
		return err
	}
	return report.Err()
}

// Err folds a report into a single error, or nil when the report is
// clean.
func (r *Report) Err() error {
	var b strings.Builder
	if len(r.Violations) > 0 {
		fmt.Fprintf(&b, "credential guard: %d mTLS credential(s) constructed outside internal/credentials:\n",
			len(r.Violations))
		for _, v := range r.Violations {
			fmt.Fprintf(&b, "  - %s\n", v)
		}
		b.WriteString("  Obtain credentials from internal/credentials instead. If this path " +
			"genuinely cannot, add it to credguard.Exemptions with a reason.\n")
	}
	for _, e := range r.StaleExemptions {
		fmt.Fprintf(&b, "credential guard: exemption %q names a path that does not exist; "+
			"delete the entry (reason given: %s)\n", e.Path, e.Reason)
	}
	for _, e := range r.ObsoleteExemptions {
		fmt.Fprintf(&b, "credential guard: exemption %q no longer covers anything the guard "+
			"would flag; delete the entry (reason given: %s)\n", e.Path, e.Reason)
	}
	if b.Len() == 0 {
		return nil
	}
	return errors.New(strings.TrimSuffix(b.String(), "\n"))
}

// Scan walks root and judges every Go file under it against the
// exemptions given.
//
// It returns an error, rather than a clean report, for any condition
// under which a pass would be meaningless: an absent or unusable root,
// a root with no Go files, a root where the allow-list swallowed
// everything, or a file that does not parse.
func Scan(root string, exemptions []Exemption) (*Report, error) {
	info, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("credential guard: scan root %q: %w", root, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("credential guard: scan root %q is not a directory", root)
	}
	if err := validateExemptions(exemptions); err != nil {
		return nil, err
	}

	report := &Report{}
	// flagged counts, per exemption, the violations the guard would
	// have reported had the path not been exempt. An exemption that
	// ends on zero is not holding anything back and is reported as
	// obsolete.
	flagged := make([]int, len(exemptions))
	fset := token.NewFileSet()

	walkErr := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != root && skippedDirs[d.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".go") {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		report.GoFiles++

		violations, err := inspectFile(fset, p, rel)
		if err != nil {
			return err
		}
		if i := exemptionIndex(exemptions, rel); i >= 0 {
			report.Exempted++
			flagged[i] += len(violations)
			return nil
		}
		report.Inspected++
		report.Violations = append(report.Violations, violations...)
		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("credential guard: %w", walkErr)
	}

	switch {
	case report.GoFiles == 0:
		return nil, fmt.Errorf(
			"credential guard: no Go files under scan root %q; a guard that scanned nothing has proved nothing",
			root)
	case report.Inspected == 0:
		return nil, fmt.Errorf(
			"credential guard: all %d Go files under scan root %q are exempt; the guard inspected nothing",
			report.GoFiles, root)
	}

	for i, e := range exemptions {
		switch _, statErr := os.Stat(filepath.Join(root, filepath.FromSlash(e.Path))); {
		case statErr != nil:
			report.StaleExemptions = append(report.StaleExemptions, e)
		case flagged[i] == 0:
			report.ObsoleteExemptions = append(report.ObsoleteExemptions, e)
		}
	}
	return report, nil
}

// validateExemptions rejects a malformed allow-list before any scanning
// happens, so a typo presents as a guard error rather than as an
// exemption that silently covers nothing.
func validateExemptions(exemptions []Exemption) error {
	seen := make(map[string]bool, len(exemptions))
	for _, e := range exemptions {
		switch {
		case e.Path == "":
			return errors.New("credential guard: exemption with an empty path")
		case e.Reason == "":
			return fmt.Errorf("credential guard: exemption %q carries no reason", e.Path)
		case path.Clean(e.Path) != e.Path || strings.HasPrefix(e.Path, "/") || strings.HasPrefix(e.Path, ".."):
			return fmt.Errorf("credential guard: exemption %q must be a clean relative slash path", e.Path)
		case seen[e.Path]:
			return fmt.Errorf("credential guard: exemption %q is listed twice", e.Path)
		}
		seen[e.Path] = true
	}
	return nil
}

// exemptionIndex returns the index of the first exemption covering rel,
// or -1.
func exemptionIndex(exemptions []Exemption, rel string) int {
	for i, e := range exemptions {
		if e.covers(rel) {
			return i
		}
	}
	return -1
}

// inspectFile parses one file and returns the references it makes that
// the guard rejects.
//
// Imports are resolved first so that judgement is on the package a
// symbol came from. That is what makes an aliased import no different
// from a plain one, and it is why the guard cannot be walked around by
// spelling crypto/tls something else.
func inspectFile(fset *token.FileSet, absPath, rel string) ([]Violation, error) {
	file, err := parser.ParseFile(fset, absPath, nil, parser.SkipObjectResolution)
	if err != nil {
		return nil, fmt.Errorf("credential guard: parse %s: %w", rel, err)
	}

	var violations []Violation
	qualifiers := make(map[string]watched)
	for _, imp := range file.Imports {
		importPath, unquoteErr := strconv.Unquote(imp.Path.Value)
		if unquoteErr != nil {
			return nil, fmt.Errorf("credential guard: %s: import path %s: %w",
				rel, imp.Path.Value, unquoteErr)
		}
		w, ok := watchedFor(importPath)
		if !ok {
			continue
		}
		if w.policy == importForbidden {
			violations = append(violations, Violation{
				File:   rel,
				Line:   fset.Position(imp.Pos()).Line,
				Import: importPath,
				Why:    w.why,
			})
			continue
		}
		qualifier := path.Base(importPath)
		if imp.Name != nil {
			qualifier = imp.Name.Name
		}
		switch qualifier {
		case "_":
			// A blank import can reference no symbol, so there is
			// nothing to judge.
			continue
		case ".":
			// A dot import makes every reference unqualified and so
			// invisible to a reader scanning for "tls.". Reject the
			// import rather than trying to follow it.
			violations = append(violations, Violation{
				File:   rel,
				Line:   fset.Position(imp.Pos()).Line,
				Import: importPath,
				Why: "dot-imported, which hides every reference to it from both this guard " +
					"and a human reader; " + w.why,
			})
			continue
		}
		qualifiers[qualifier] = w
	}

	if len(qualifiers) > 0 {
		ast.Inspect(file, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			ident, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			w, ok := qualifiers[ident.Name]
			if !ok || w.permits(sel.Sel.Name) {
				return true
			}
			violations = append(violations, Violation{
				File:   rel,
				Line:   fset.Position(sel.Pos()).Line,
				Import: w.importPath,
				Symbol: sel.Sel.Name,
				Why:    w.why,
			})
			return true
		})
	}
	return violations, nil
}

// watchedFor returns the watched entry covering importPath.
func watchedFor(importPath string) (watched, bool) {
	for _, w := range watchedPackages {
		if w.matchPrefix {
			if importPath == w.importPath || strings.HasPrefix(importPath, w.importPath+"/") {
				return w, true
			}
			continue
		}
		if importPath == w.importPath {
			return w, true
		}
	}
	return watched{}, false
}

// permits reports whether a reference to symbol is acceptable.
func (w watched) permits(symbol string) bool {
	_, listed := w.symbols[symbol]
	switch w.policy {
	case allowListed:
		return listed
	case denyListed:
		return !listed
	case importForbidden:
		return false
	}
	return false
}
