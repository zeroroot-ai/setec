// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package credguard

// Exemptions is setec's allow-list: the paths permitted to build a TLS
// credential without going through internal/credentials.
//
// It lives in its own file so that widening it is a one-file diff a
// reviewer cannot miss. Six entries, and each one has to argue for
// itself:
//
//   - the credential module, which is where credentials are supposed to
//     be built;
//   - three example programs, which are separate Go modules and cannot
//     import internal/ at all;
//   - two node-agent test files, which build a client config to prove
//     the node-agent's server accepts the right callers and refuses the
//     wrong ones.
//
// Nothing here is a glob. examples/ is exempt as three named
// directories rather than as a pattern, so a future directory under
// examples/ that is not a sample client is judged like any other code
// instead of inheriting an exemption written for something else. Nor is
// there an exemption for _test.go as a class: tests are in scope, and
// the two that are exempt are exempt by name.
//
// An entry that stops being needed is a guard failure, not a
// leftover — see Report.ObsoleteExemptions.
func Exemptions() []Exemption {
	return []Exemption{
		{
			Path: "internal/credentials",
			Reason: "the credential module itself. Every mTLS credential in setec is " +
				"assembled here, behind one narrow interface, which is the property the " +
				"rest of this guard exists to keep true.",
		},
		{
			Path: "examples/ai-code-exec",
			Reason: "a separate Go module of sample client code for setec users. Go's " +
				"internal/ rule makes internal/credentials unimportable from it, so it " +
				"has to build its own client tls.Config. Exempt as a named directory, " +
				"not as an examples/ pattern.",
		},
		{
			Path: "examples/ci-sandbox",
			Reason: "a separate Go module of sample client code for setec users. Go's " +
				"internal/ rule makes internal/credentials unimportable from it, so it " +
				"has to build its own client tls.Config. Exempt as a named directory, " +
				"not as an examples/ pattern.",
		},
		{
			Path: "examples/sec-research",
			Reason: "a separate Go module of sample client code for setec users. Go's " +
				"internal/ rule makes internal/credentials unimportable from it, so it " +
				"has to build its own client tls.Config. Exempt as a named directory, " +
				"not as an examples/ pattern.",
		},
		{
			Path: "cmd/node-agent/credentials_test.go",
			Reason: "builds a client tls.Config as a test fixture, to dial the node-agent's " +
				"server and assert which callers it accepts. A test that proves a server " +
				"refuses the wrong peer needs a peer the credential module would not hand " +
				"it. Exempt by file, not by package and not as a _test.go class.",
		},
		{
			Path: "cmd/node-agent/credentials_spiffe_test.go",
			Reason: "builds a client tls.Config carrying a chosen SPIFFE ID, to prove the " +
				"node-agent's SPIFFE mode authorizes the peer and does not merely " +
				"authenticate it. The credential module cannot produce the wrong-identity " +
				"case the test turns on. Exempt by file, not by package and not as a " +
				"_test.go class.",
		},
	}
}
