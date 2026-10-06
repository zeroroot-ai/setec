<!-- SPDX-License-Identifier: Apache-2.0 -->
# Code-scanning dismissals — setec

Every dismissed code-scanning alert on this repo is recorded here, with the
evidence that justified the dismissal and the condition that would reverse it.

The GitHub API caps `dismissed_comment` at **280 characters**, which is far too
short for a real reachability argument. Each dismissal therefore carries a short
comment naming the CVE, the one-line reason, and a citation of this file. This
document is the substantive record; the API comment is the pointer.

**This file is version-controlled on purpose.** A dismissal that lives only in
the GitHub UI is invisible to review, invisible to `git log`, and impossible to
re-audit when the threat model changes.

## Six images

| SARIF category | Dockerfile | What it is |
|---|---|---|
| `trivy-setec` | `Dockerfile` | The operator (controller-manager). Reconciles `Sandbox`/`SandboxClass`; serves the admission webhooks. |
| `trivy-setec-frontend` | `Dockerfile` | The API frontend. |
| `trivy-setec-node-agent` | `Dockerfile` | Per-node snapshot agent. |
| `trivy-setec-device-plugin` | `Dockerfile` | Per-node KVM device plugin. |
| `trivy-setec-launcher` | `Dockerfile.launcher` | The launcher of each Sandbox Pod. **Also carries the Firecracker release that `firecracker.env` pins**, the guest kernel and the initrd with the guest agent. |
| `trivy-setec-disk-builder` | `Dockerfile.disk-builder` | The Job that turns an image digest into a signed disk. |

The node installer image and its Kata payload left in the cutover of
setec#198. Every finding that this repo carried before the cutover was in that
payload, not in a binary that setec compiles.

## Reachability classes

Every alert is assigned to exactly one class before any dismissal decision.

| Class | Scope | Dismissal policy |
|---|---|---|
| **A — setec's own compiled code** | `/manager`, `/frontend`, `/node-agent`, `/device-plugin`, `setec-launcher`, `setec-guest-agent`, `setec-disk-builder`: everything built from `cmd/` and `internal/` in this repo | **Never dismissed.** These get fixed. |
| **B — Firecracker, host side** | `/usr/local/bin/firecracker` in the launcher image: the VMM that the launcher starts in the Pod | Dismissable **only** on symbol-level evidence that the vulnerable code is not linked into the binary. Never on a narrative argument. |
| **C — guest-side artifacts** | The guest kernel and the initrd that boot inside the machine | Contained by the machine boundary. Dismissable with a stated containment argument. |

### Class B is not "sandbox contained" — read this before triaging one

The tempting shortcut is: *setec runs untrusted workloads in microVMs, therefore
a CVE in Firecracker is contained by the microVM.* **That is wrong, and getting
it wrong is the main way triage fails on this repo.**

Firecracker runs **in the launcher Pod**, outside the guest. It is not a thing
the microVM contains. It is the process that *implements* the microVM boundary,
and its device emulation parses input from a guest that runs **untrusted code by
design**.

Per docs/design/threat-model.md the split is: a cross-tenant leak is a `gibson` bug, **a sandbox
escape is a `setec` bug.** Firecracker is on setec's side of that line. A
memory-safety defect in its guest-facing path is a sandbox-escape primitive,
not a contained finding.

## `FixedVersion: NONE` and "it's upstream" are not verdicts

Ask three questions, in order, before reachability is even considered:

1. **Does the artifact need to be in the image at all?** The distro or upstream
   shipping no patch says nothing about whether the file needs to ship.
2. **Has upstream since published a build that fixes it?** A pinned release is
   only as fresh as the last time someone moved the pin. Move the pin in
   `firecracker.env` or `kernel/kernel.env` first.
3. **Only then**: is the vulnerable code provably not linked?

A dismissal without a symbol count from a real symbol dump, plus a non-zero
control symbol that proves the dump resolved, does not go in this file.

## Dismissals

### History: the Kata payload (Entries 1 to 6 and 9 to 13)

These entries recorded the Kata payload of the installer image: removed
binaries, pin bumps, and symbol evidence for findings in
`containerd-shim-kata-v2`. The installer image and the payload left in
setec#198, so no shipped image holds those findings. `git log -p` of this file
holds the full text. The entry numbers stay retired, so a dismissal comment on
GitHub that cites one still points at its record in the history.

### Entry 7 — CodeQL alerts #23 and #24, dismissed as false positives (setec#21)

**#24 `go/disabled-certificate-check`, `internal/credentials/credentials.go`.**
`InsecureSkipVerify` is set only when the peer certificate carries no name
that Go's hostname check could use. Verification is not dropped, it is
replaced: `authorizeUnnamedPeer` parses the presented chain, verifies it
against the source's trust anchors with `x509.Certificate.Verify` and
`ExtKeyUsageServerAuth`, and then asks the source to authorize the verified
identity. Go offers no other way to keep chain verification and skip only
the name check. CodeQL flags the field, not the replacement.

**#23 `go/weak-sensitive-data-hashing`, `internal/snapshot/secretscan/scanner.go`.**
`Version()` hashes the builtin detector rule names and regex patterns with
SHA-256 to derive a detector-set version string. No credential is hashed.
CodeQL matched the word "password" in a rule name. `Version()` left in
setec#198 with the pool path that read it, so this alert closes as fixed.

**Reverses if** a name-bearing certificate contract replaces the unnamed
peer path.

### Entry 8 — Scorecard Token-Permissions alerts #2 to #17, dismissed (setec#21)

Scorecard scores every job-level `contents: write`, `packages: write` and
`security-events: write` as 0 and cannot see inside a `workflow_call`. The
grants in `images.yml` and `release-please.yml` are exactly the set the org
reusable image workflow declares (`packages: write` and `id-token: write` to
push and sign, `attestations: write` for the SBOM attestation,
`security-events: write` to upload the Trivy SARIF, `actions: read` for the
upload). `release.yml` needs `contents: write` to create the draft release
and `release-please.yml` needs it to open the release PR. Nothing can be
removed without breaking the job. The one fixable finding, the top-level
`packages: write` in `publish-chart.yml` (#18), was moved to the job.

Dismissal reasons: `security-events: write` as false positive (the SARIF
upload exists, one workflow_call away), the rest as won't fix.

**Reverses if** the reusable workflow needs fewer permissions.
