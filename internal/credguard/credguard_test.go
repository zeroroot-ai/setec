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

package credguard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// bypassSource is a file that assembles a TLS credential by hand: the
// exact shape internal/credentials was created to remove from the rest
// of the tree.
const bypassSource = `package sample

import "crypto/tls"

func serverConfig(cert tls.Certificate) *tls.Config {
	return &tls.Config{Certificates: []tls.Certificate{cert}}
}
`

// cleanSource references nothing the guard watches.
const cleanSource = `package sample

func Greeting() string { return "hello" }
`

const bypassFile = "bypass.go"

// TestRepositoryBuildsNoCredentialOutsideTheModule is the guard itself.
// Everything else in this file proves this test is capable of failing.
func TestRepositoryBuildsNoCredentialOutsideTheModule(t *testing.T) {
	if err := Check(moduleRoot(t), Exemptions()); err != nil {
		t.Fatalf("credential guard rejected the tree:\n%v", err)
	}
}

// TestExemptionsAreDeliberate holds the allow-list to the standard the
// guard is for: no entry without a reason, no duplicate, and no entry
// whose path has drifted out of the tree.
func TestExemptionsAreDeliberate(t *testing.T) {
	root := moduleRoot(t)
	seen := map[string]bool{}
	for _, e := range Exemptions() {
		if strings.TrimSpace(e.Reason) == "" {
			t.Errorf("exemption %q carries no reason", e.Path)
		}
		if seen[e.Path] {
			t.Errorf("exemption %q is listed twice", e.Path)
		}
		seen[e.Path] = true
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(e.Path))); err != nil {
			t.Errorf("exemption %q names a path that is not in the tree: %v", e.Path, err)
		}
		if strings.Contains(e.Path, "*") {
			t.Errorf("exemption %q is a pattern; exemptions name one file or one directory", e.Path)
		}
	}
}

// TestScanRejectsAnEmptyTree is the self-test that matters most. A
// guard reporting success over zero files is the failure mode this
// repository family keeps rediscovering, so "scanned nothing" is an
// error here and this test is what says so.
func TestScanRejectsAnEmptyTree(t *testing.T) {
	_, err := Scan(t.TempDir(), nil)
	if err == nil {
		t.Fatal("scanning an empty tree: want an error, got a pass")
	}
	if !strings.Contains(err.Error(), "no Go files") {
		t.Fatalf("error should name the empty scan as the cause, got: %v", err)
	}
}

// TestScanRejectsAMissingRoot covers the other way to scan nothing:
// point the guard somewhere that is not there.
func TestScanRejectsAMissingRoot(t *testing.T) {
	if _, err := Scan(filepath.Join(t.TempDir(), "absent"), nil); err == nil {
		t.Fatal("scanning a missing root: want an error, got a pass")
	}
}

// TestScanRejectsARootThatIsAFile keeps the third variant honest.
func TestScanRejectsARootThatIsAFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "notadir")
	writeFile(t, file, cleanSource)
	if _, err := Scan(file, nil); err == nil {
		t.Fatal("scanning a file as if it were a tree: want an error, got a pass")
	}
}

// TestScanRejectsATreeThatIsEntirelyExempt closes the subtler version
// of the same hole: files exist, the guard walked them, and the
// allow-list left nothing to judge.
func TestScanRejectsATreeThatIsEntirelyExempt(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "sample", bypassFile), bypassSource)

	_, err := Scan(root, []Exemption{{Path: "sample", Reason: "test fixture"}})
	if err == nil {
		t.Fatal("scanning a fully exempt tree: want an error, got a pass")
	}
	if !strings.Contains(err.Error(), "inspected nothing") {
		t.Fatalf("error should name the empty inspection as the cause, got: %v", err)
	}
}

// TestScanFlagsAHandBuiltConfig is the mutation the guard exists for.
func TestScanFlagsAHandBuiltConfig(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, bypassFile), bypassSource)
	writeFile(t, filepath.Join(root, "clean.go"), cleanSource)

	report := scanOK(t, root, nil)
	if len(report.Violations) == 0 {
		t.Fatal("a hand-built tls.Config went unflagged")
	}
	for _, v := range report.Violations {
		if v.File != bypassFile {
			t.Errorf("flagged %s, which builds no credential", v.File)
		}
	}
	if report.Inspected != 2 {
		t.Errorf("inspected %d files, want 2", report.Inspected)
	}
}

// TestScanFlagsABypassInATestFile records the framing decision: tests
// are in scope. A bypass helper that lives in a _test.go file and gets
// a production caller later is the hole a wholesale test exemption
// would leave open.
func TestScanFlagsABypassInATestFile(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "helper_test.go"), bypassSource)
	writeFile(t, filepath.Join(root, "clean.go"), cleanSource)

	if report := scanOK(t, root, nil); len(report.Violations) == 0 {
		t.Fatal("a hand-built tls.Config in a _test.go file went unflagged")
	}
}

// TestScanSeesThroughAnAliasedImport proves the guard judges the
// package a symbol came from rather than the qualifier it was spelled
// with.
func TestScanSeesThroughAnAliasedImport(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, bypassFile), `package sample

import crypt "crypto/tls"

func config() *crypt.Config { return &crypt.Config{} }
`)
	writeFile(t, filepath.Join(root, "clean.go"), cleanSource)

	if report := scanOK(t, root, nil); len(report.Violations) == 0 {
		t.Fatal("an aliased crypto/tls import went unflagged")
	}
}

// TestScanFlagsADotImport covers the evasion the selector walk cannot
// follow: with a dot import there is no qualifier to match, so the
// import itself has to be the violation.
func TestScanFlagsADotImport(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, bypassFile), `package sample

import . "crypto/tls"

func config() *Config { return &Config{} }
`)
	writeFile(t, filepath.Join(root, "clean.go"), cleanSource)

	report := scanOK(t, root, nil)
	if len(report.Violations) == 0 {
		t.Fatal("a dot-imported crypto/tls went unflagged")
	}
	if report.Violations[0].Symbol != "" {
		t.Errorf("dot-import violation should name the import, not a symbol; got %q",
			report.Violations[0].Symbol)
	}
}

// TestScanFlagsTheWorkloadAPI keeps the second credential source inside
// the module too. SPIFFE is where peer authorization is decided, so a
// second consumer would be a second answer to who is authorized.
func TestScanFlagsTheWorkloadAPI(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, bypassFile), `package sample

import "github.com/spiffe/go-spiffe/v2/workloadapi"

func source() *workloadapi.X509Source { return nil }
`)
	writeFile(t, filepath.Join(root, "clean.go"), cleanSource)

	if report := scanOK(t, root, nil); len(report.Violations) == 0 {
		t.Fatal("a go-spiffe import outside the credential module went unflagged")
	}
}

// TestScanFlagsTransportCredentialConstruction covers the last step of
// building a credential, which is where a bypass would surface even if
// the tls.Config came from somewhere clever.
func TestScanFlagsTransportCredentialConstruction(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, bypassFile), `package sample

import grpccreds "google.golang.org/grpc/credentials"

func creds() grpccreds.TransportCredentials {
	return grpccreds.NewClientTLSFromCert(nil, "")
}
`)
	writeFile(t, filepath.Join(root, "clean.go"), cleanSource)

	report := scanOK(t, root, nil)
	if len(report.Violations) != 1 {
		t.Fatalf("want exactly the NewClientTLSFromCert call flagged, got %d violations: %v",
			len(report.Violations), report.Violations)
	}
	if got := report.Violations[0].Symbol; got != "NewClientTLSFromCert" {
		t.Errorf("flagged %q; TransportCredentials is the type consumers legitimately name", got)
	}
}

// TestScanPermitsInspectionOfAnEstablishedConnection is the paired
// acceptance case. A guard that only ever refuses would pass by
// refusing everything, and reading a peer's identity off a connection
// somebody else configured is not building a credential.
func TestScanPermitsInspectionOfAnEstablishedConnection(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "inspect.go"), `package sample

import (
	"crypto/tls"

	"google.golang.org/grpc/credentials"
)

func peerState(info credentials.TLSInfo) tls.ConnectionState { return info.State }
`)

	if report := scanOK(t, root, nil); len(report.Violations) != 0 {
		t.Fatalf("reading peer state off an established connection was flagged: %v", report.Violations)
	}
}

// TestScanPermitsOrdinaryCertificateParsing pairs the crypto/x509
// deny-list with its acceptance case: parsing a certificate is ordinary
// and only trust-anchor assembly is the credential module's.
func TestScanPermitsOrdinaryCertificateParsing(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "parse.go"), `package sample

import "crypto/x509"

func parse(raw []byte) (*x509.Certificate, error) { return x509.ParseCertificate(raw) }
`)
	if report := scanOK(t, root, nil); len(report.Violations) != 0 {
		t.Fatalf("parsing a certificate was flagged: %v", report.Violations)
	}

	pool := t.TempDir()
	writeFile(t, filepath.Join(pool, "pool.go"), `package sample

import "crypto/x509"

func anchors() *x509.CertPool { return x509.NewCertPool() }
`)
	if report := scanOK(t, pool, nil); len(report.Violations) == 0 {
		t.Fatal("assembling trust anchors by hand went unflagged")
	}
}

// TestScanHonoursAnExemptionWithoutWideningIt proves an exemption
// covers what it names and nothing adjacent.
func TestScanHonoursAnExemptionWithoutWideningIt(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "allowed", bypassFile), bypassSource)
	writeFile(t, filepath.Join(root, "allowed-neighbour", bypassFile), bypassSource)

	report := scanOK(t, root, []Exemption{{Path: "allowed", Reason: "test fixture"}})
	if len(report.Violations) == 0 {
		t.Fatal("the neighbour of an exempt directory went unflagged")
	}
	for _, v := range report.Violations {
		if !strings.HasPrefix(v.File, "allowed-neighbour/") {
			t.Errorf("flagged %q; the exemption names \"allowed\" and covers nothing else", v.File)
		}
	}
	if report.Exempted != 1 || report.Inspected != 1 {
		t.Errorf("exempted %d and inspected %d files, want 1 each",
			report.Exempted, report.Inspected)
	}
}

// TestScanRejectsAStaleExemption stops the allow-list decaying into a
// list of paths nobody can find any more.
func TestScanRejectsAStaleExemption(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "clean.go"), cleanSource)

	report := scanOK(t, root, []Exemption{{Path: "gone", Reason: "test fixture"}})
	if len(report.StaleExemptions) != 1 {
		t.Fatalf("want the absent path reported as stale, got %v", report.StaleExemptions)
	}
	if report.Err() == nil {
		t.Fatal("a stale exemption should fail the guard")
	}
}

// TestScanRejectsAnObsoleteExemption is the stronger version: the path
// is still there but no longer holds anything the guard would flag, so
// the exemption has stopped earning its place.
func TestScanRejectsAnObsoleteExemption(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "tidied", "clean.go"), cleanSource)
	writeFile(t, filepath.Join(root, "other.go"), cleanSource)

	report := scanOK(t, root, []Exemption{{Path: "tidied", Reason: "test fixture"}})
	if len(report.ObsoleteExemptions) != 1 {
		t.Fatalf("want the no-longer-needed exemption reported, got %v", report.ObsoleteExemptions)
	}
	if report.Err() == nil {
		t.Fatal("an obsolete exemption should fail the guard")
	}
}

// TestScanRejectsAnUnparseableFile keeps the walk fail-closed: a file
// the guard cannot read is not a file the guard can vouch for.
func TestScanRejectsAnUnparseableFile(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "broken.go"), "package sample\n\nfunc (")
	if _, err := Scan(root, nil); err == nil {
		t.Fatal("an unparseable file: want an error, got a pass")
	}
}

// TestScanRejectsAnExemptionWithoutAReason keeps "silent skip" out of
// the vocabulary.
func TestScanRejectsAnExemptionWithoutAReason(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "clean.go"), cleanSource)
	if _, err := Scan(root, []Exemption{{Path: "clean.go"}}); err == nil {
		t.Fatal("an exemption with no reason: want an error, got a pass")
	}
}

// scanOK runs a scan that is expected to complete, and fails the test
// if it could not.
func scanOK(t *testing.T, root string, exemptions []Exemption) *Report {
	t.Helper()
	report, err := Scan(root, exemptions)
	if err != nil {
		t.Fatalf("Scan(%q): %v", root, err)
	}
	return report
}

// writeFile writes source at p, creating parent directories.
func writeFile(t *testing.T, p, source string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("mkdir %q: %v", filepath.Dir(p), err)
	}
	if err := os.WriteFile(p, []byte(source), 0o600); err != nil {
		t.Fatalf("write %q: %v", p, err)
	}
}

// moduleRoot walks up from the test's working directory to the
// directory holding go.mod. It fails rather than falling back on a
// relative guess: a guard that scanned the wrong tree is a guard that
// scanned nothing.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("working directory: %v", err)
	}
	for {
		if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the test's working directory; cannot locate the module root")
		}
		dir = parent
	}
}
