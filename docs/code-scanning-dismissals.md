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

## Six images, one surface with findings

| SARIF category | Dockerfile | What it is |
|---|---|---|
| `trivy-setec` | `Dockerfile` | The operator (controller-manager). Reconciles `Sandbox`/`SandboxClass`; serves the admission webhooks. |
| `trivy-setec-frontend` | `Dockerfile` | The API frontend. |
| `trivy-setec-node-agent` | `Dockerfile` | Per-node pool/snapshot agent. |
| `trivy-setec-runtime-agent` | `Dockerfile` | Per-node runtime prober. |
| `trivy-setec-guest-agent` | `Dockerfile` | Static in-guest binary; not a runnable service image. |
| `trivy-setec-installer` | `Dockerfile.installer` | The node installer. **Additionally carries the stock kata-containers static release as an immutable payload** (ADR-0003). |

Five of the six scan clean. **Every finding this repo has ever carried belongs
to `trivy-setec-installer`, and every one of them is inside the kata payload —
not in any binary setec compiles.**

## Reachability classes

Every alert is assigned to exactly one class before any dismissal decision.

| Class | Scope | Dismissal policy |
|---|---|---|
| **A — setec's own compiled code** | `/manager`, `/frontend`, `/node-agent`, `/runtime-agent`, `/setec-guest-agent`, `/entrypoint` (the installer binary) — everything built from `cmd/` and `internal/` in this repo | **Never dismissed.** This is the operator's and frontend's own request-handling path. These get fixed. |
| **B — kata payload, host-side control plane** | `/opt/kata/bin/containerd-shim-kata-v2` — the process containerd launches to create and supervise the microVM | Dismissable **only** on symbol-level evidence that the vulnerable code is not linked into the binary. Never on a narrative argument. |
| **C — kata payload, guest-side artifacts** | The guest kernel (`vmlinux.container`) and rootfs image (`kata-containers.img`) laid inside the microVM | Contained by the microVM boundary. Dismissable with a stated containment argument. *(No entry has ever fallen in this class — Trivy does not unpack these.)* |

### Class B is not "sandbox contained" — read this before triaging one

The tempting shortcut is: *setec runs untrusted workloads in microVMs, therefore
a CVE in the kata payload is contained by the microVM.* **That is wrong for the
shim, and getting it wrong is the main way triage fails on this repo.**

`containerd-shim-kata-v2` runs **on the host**, outside the guest. It is not a
thing the microVM contains — it is the process that *implements* the microVM
boundary. It sits between two inputs:

- **containerd/kubelet side** — OCI spec, image config, annotations, partly
  derived from a tenant-supplied `Sandbox` spec.
- **guest side** — ttrpc over vsock to `kata-agent`, inside a VM that is
  running **untrusted code by design**. A workload that compromises its guest
  agent is then speaking directly to the shim's parser.

Per ADR-0052 the split is: a cross-tenant leak is a `gibson` bug, **a sandbox
escape is a `setec` bug.** The shim is on setec's side of that line. A
remote-code-execution or memory-safety defect in the shim's guest-facing path is
a sandbox-escape primitive, not a contained finding.

So Class B gets the *strictest* dismissal bar in this ledger, not the loosest:
**linked-and-unproven stays open.**

## `FixedVersion: NONE` and "it's upstream" are not verdicts

Two orders of question, in order, before reachability is even considered:

1. **Does the artifact need to be in the image at all?** The distro or upstream
   shipping no patch says nothing about whether the file needs to ship. Entry 1
   removed 47 findings — half of everything this repo had — by deleting two
   binaries nothing executed.
2. **Has upstream since published a build that fixes it?** A pinned payload is
   only as fresh as the last time someone moved the pin. The kata bump in
   Entry 2 removed a further 29.
3. **Only then**: is the vulnerable code provably not linked?

## Current status of Class A

**Class A is clean, and no Class-A alert has ever been dismissed.**

Verified locally on images built from `main` (`trivy image`, all scanners):

| Image | Binary | HIGH/CRITICAL | Total |
|---|---|---|---|
| `setec` | `/manager` | 0 | 0 |
| `setec-frontend` | `/frontend` | 0 | 0 |
| `setec-node-agent` | `/node-agent` | 0 | 0 |
| `setec-runtime-agent` | `/runtime-agent` | 0 | 0 |
| `setec-guest-agent` | `/setec-guest-agent` | 0 | 0 |
| `setec-installer` | `/entrypoint` | 0 | 0 |

OS package layer is also 0 in every image (distroless-static-debian12).

## Dismissals

### Entry 1 — 47 findings removed by deletion, not dismissal (setec#284)

Not a dismissal. Recorded because it is the largest single reduction and sets
the precedent for how this repo triages.

`Dockerfile.installer` extracted `/opt/kata/bin/kata-runtime` and
`/opt/kata/bin/kata-collect-data.sh` from the kata tarball. `kata-runtime` is a
Go binary of the same vintage as the shim and carried an **identical 47-CVE
set** — the same stdlib and the same vendored dependency graph.

Nothing in setec ever executed either file. containerd resolves
`runtime_type = "io.containerd.kata-fc.v2"` directly to
`containerd-shim-kata-v2`; `requiredKataArtifacts` in `internal/installer/kata.go`
never listed `kata-runtime`; and `internal/runtimeagent/probe/kata_fc.go`
performs no binary lookup at all. The only consumer was a cosmetic
kata-deploy-parity symlink at `/usr/local/bin/kata-runtime`.

Both are now excluded from the extract list, the parity symlink is gone, and the
payload gate carries a **negative** assertion (`test ! -e .../kata-runtime`) so
re-adding either fails the image build rather than silently re-importing 47 CVEs.

**Reverses if** any setec code path acquires a genuine need to exec the kata CLI
on a node. The AMI bake path (`packer/`) extracts the full tarball independently
and is unaffected.

### Entry 2 — kata payload bumped 3.28.0 → 3.32.0 (setec#284)

Not a dismissal. The pin had drifted three releases behind. Moving it to the
newest 3.x release took the shim from **47 findings (1 CRITICAL, 29 HIGH) to
18 (0 CRITICAL, 12 HIGH)**, clearing the CRITICAL and 29 others outright.

Pins moved in lockstep, as `packer/eks-kata-fc-ami/README.md` requires:
`Dockerfile.installer`, `packer/eks-kata-fc-ami/eks-kata-fc.pkr.hcl`,
`packer/eks-kata-fc-ami/README.md`, `development/k3s/scripts/20-install-kata.sh`.

**4.0.0 was evaluated and deliberately not taken.** It would reach 12 findings,
but it rewrites the runtime in Rust ("runtime-rs"), makes that the default, and
**runtime-rs does not list Firecracker among its supported hypervisors** (QEMU,
Cloud Hypervisor, Dragonball). The Go runtime setec depends on is *deprecated*
in 4.0.0. Adopting it for a 6-finding delta would move setec's entire substrate
onto a deprecated upstream path, and no e2e microVM boot validation is currently
available to prove the FC path still works. Tracked in setec#286.

### Entry 3 — CVE-2026-39822, `os.Root` symlink traversal (stdlib)

**Class B. Dismissed: vulnerable code not linked.**

GO-2026-4970 names exactly twelve affected symbols, all on the `os.Root` API:
`os.OpenInRoot`, `os.(*Root).Create`, `.Open`, `.OpenFile`, `.OpenRoot`,
`.ReadFile`, `.WriteFile`, `os.openRootInRoot`, `os.(*rootFS).Open`,
`.ReadDir`, `.ReadFile`, `os.rootOpenFileNolog`.

`go tool nm` on the shipped `containerd-shim-kata-v2` (kata 3.32.0) resolves
**173,331 symbols** and returns **0 matches for every one of the twelve**. The
`os.Root` API is not linked into the binary; the Go linker eliminated it. Control
symbols in the same dump — `os.OpenFile` (2), `golang.org/x/mod/semver` (12),
`html/template.(*Template).Execute` (4) — are present, confirming the dump
resolves real symbols and the zeros are genuine absences rather than a tooling
artifact.

**Reverses if** a future kata build links `os.Root`. Re-check with:
`go tool nm /opt/kata/bin/containerd-shim-kata-v2 | grep -F 'os.(*Root)'`

### Entry 4 — CVE-2026-56864, `x/mod/sumdb` unauthenticated hash acceptance

**Class B. Dismissed: vulnerable code not linked.**

GO-2026-6180 names a single affected symbol:
`golang.org/x/mod/sumdb.(*Client).Lookup`. Zero matches in the shim's symbol
table. The only `golang.org/x/mod` subpackage linked is
`golang.org/x/mod/semver` (12 symbols) — version-string comparison, which shares
no code with the checksum-database client.

The vulnerability is additionally reachable only when fetching Go modules
through a malicious `GOPROXY`/`GOSUMDB`. The shim is a compiled artifact; it
resolves no modules at runtime.

**Reverses if** `golang.org/x/mod/sumdb` appears in the shim's symbol table.

### Entry 5 — CVE-2026-56865, `x/mod/sumdb/tlog` tile verification bypass

**Class B. Dismissed: vulnerable code not linked.**

GO-2026-6179 names `golang.org/x/mod/sumdb/tlog.tileHashReader.ReadHashes`.
Zero matches for the `golang.org/x/mod/sumdb/tlog` package prefix in the shim's
symbol table. Same evidence and same reversal condition as Entry 4.

## Residual open findings — NOT dismissed

**15 findings remain open on `trivy-setec-installer`, all in
`/opt/kata/bin/containerd-shim-kata-v2`, and all are Class B.**

They are **deliberately left open**. For each one, `go tool nm` confirms the
vulnerable package *is* linked into the shipped binary. Linked is not the same
as reachable from untrusted input — but under the Class-B bar above, "I could
not prove it reachable" is not a dismissal reason. Proving the negative would
need symbol-level dataflow analysis of upstream kata's shim, which this repo has
not done.

| CVE | Sev | Package | Fixed in | Linked? |
|---|---|---|---|---|
| CVE-2026-53488 | HIGH | `containerd/containerd` v1.7.32 | 1.7.33 | yes (6,495 syms) |
| CVE-2026-47262 | MED | `containerd/containerd` v1.7.32 | 1.7.33 | yes |
| CVE-2026-41579 | MED | `opencontainers/runc` v1.2.8 | 1.3.6 | yes (384 syms) |
| CVE-2026-46600 | HIGH | `golang.org/x/net` v0.55.0 | 0.56.0 | yes (871 syms, http2) |
| CVE-2026-56852 | HIGH | `golang.org/x/text` v0.37.0 | 0.39.0 | yes (417 syms) |
| GHSA-hrxh-6v49-42gf | HIGH | `google.golang.org/grpc` v1.79.3 | 1.82.1 | yes (4,282 syms) |
| CVE-2026-2303 | MED | `go.mongodb.org/mongo-driver` v1.14.0 | 1.17.7 | yes (2,393 syms) |
| CVE-2026-33818 | HIGH | stdlib `encoding/asn1` | 1.25.13 | yes (232 syms) |
| CVE-2026-39821 | HIGH | stdlib `x/net/idna` | 1.25.13 | yes (80 syms) |
| CVE-2026-56853 | HIGH | stdlib `net/http` HTTP/2 | 1.25.13 | yes (19 syms) |
| CVE-2026-56858 | HIGH | stdlib `html/template` | 1.25.13 | yes (398 syms) |
| CVE-2026-56859 | HIGH | stdlib `encoding/xml` | 1.25.13 | yes (304 syms) |
| CVE-2026-56860 | HIGH | stdlib `net/url` | 1.25.13 | yes (123 syms) |
| CVE-2026-56862 | HIGH | stdlib `crypto/tls` | 1.25.13 | yes (1,376 syms) |
| CVE-2026-42505 | MED | stdlib `crypto/tls` ECH | 1.25.12 | yes |

Two of these deserve naming because they involve **malicious container images**,
which is precisely setec's threat model rather than an incidental risk:
CVE-2026-53488 (host-root command execution via unvalidated image config labels)
and CVE-2026-41579 (host filesystem integrity compromised by malicious images).

**setec cannot fix any of these directly.** They live in upstream kata's
vendored dependency graph and its Go toolchain. kata 3.32.0 is built with
**Go 1.25.11**; nine of the fifteen need **Go 1.25.13**. The only levers are:

1. Bump the kata pin the moment upstream publishes a release built on
   Go 1.25.13+ with refreshed vendored deps. This is the expected path and
   clears most of the table in one move.
2. Build the shim from source against a patched toolchain — which would
   abandon the stock-static-release property ADR-0003 exists to preserve, and
   is an architecture decision, not a triage decision.

Tracked as upstream dependency debt in setec#285. Re-audit on every kata pin bump.

### Entry 6 — kata 4.1.0 evaluated on 2026-09-07, not taken (setec#21)

Not a dismissal. The 20 open Trivy findings on the shim on 2026-09-07 all have
a fixed version, and kata 4.1.0 (2026-08-21) clears 18 of them: it builds
with Go 1.25.13 and pins containerd v1.7.33, runc v1.3.6, x/mod v0.40.0,
x/net v0.56.0, x/text v0.39.0 and mongo-driver v1.17.7. Two would remain
(grpc wants v1.83.1, 4.1.0 pins v1.82.1; cilium/ebpf wants v0.22.0, 4.1.0
pins v0.17.3).

The pin still stays on 3.32.0, for the reason Entry 2 gives and one new
fact. In 4.x the release tarballs are split. `kata-static-4.1.0-amd64.tar.zst`
carries only `shim-v2-rust` and no Firecracker. The Go shim
(`containerd-shim-kata-v2`) and the Firecracker and jailer binaries live in
a second tarball, `kata-go-static-4.1.0-amd64.tar.zst` (1.2 GB), which is
the deprecated Go runtime. Moving to it is a substrate decision: it changes
the payload contract in `Dockerfile.installer`, the packer bake, and the k3s
dev path (the 4.x kata-deploy chart installs runtime-rs, which has no
Firecracker hypervisor, so `kata-fc` would not appear). No 3.x release newer
than 3.32.0 exists. **These 20 alerts stay open on purpose**: they have a fix
and must not be dismissed, and the fix is an owner decision.

**Reverses if** the owner accepts the deprecated Go runtime tarball, or
upstream ships Firecracker support in runtime-rs.

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
CodeQL matched the word "password" in a rule name.

**Reverses if** a name-bearing certificate contract replaces the unnamed
peer path, or `Version()` starts hashing anything but rule definitions.

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

## Re-audit procedure

```sh
# Ground truth for the payload, without building the image:
curl -fsSL -o kata.tar.zst \
  "https://github.com/kata-containers/kata-containers/releases/download/${VER}/kata-static-${VER}-amd64.tar.zst"
mkdir -p x && tar --zstd -xf kata.tar.zst -C x ./opt/kata/bin/containerd-shim-kata-v2
trivy rootfs --scanners vuln x

# Linkage evidence for any candidate dismissal:
go tool nm x/opt/kata/bin/containerd-shim-kata-v2 | awk '{print $NF}' > syms.txt
grep -cF '<exact symbol from the GO-YYYY-NNNN advisory>' syms.txt
```

A dismissal without a symbol count from a real `nm` dump, plus a non-zero
control symbol proving the dump resolved, does not go in this file.
