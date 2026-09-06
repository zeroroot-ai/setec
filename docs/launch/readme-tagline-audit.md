<!-- SPDX-License-Identifier: Apache-2.0 -->
# README tagline audit

Decision notes on the wording that sits directly under the logo at the top of `README.md`, the badge row, and the 30-second pitch paragraph.

## Tagline candidates

| # | Tagline                                                               | Notes                                                                                        |
|---|-----------------------------------------------------------------------|----------------------------------------------------------------------------------------------|
| 1 | microVM isolation as a Kubernetes primitive                           | Short, abstract, optimises for the Kubernetes reader.                                        |
| 2 | Firecracker microVMs on demand, via a single Kubernetes CRD           | Concrete, mentions Firecracker and CRD in the same breath.                                   |
| 3 | Kubernetes-native Firecracker microVMs for sandboxing untrusted code  | Leads with the use case. Slightly long at 73 chars.                                          |
| 4 | Hardware-isolated workloads, Kubernetes-shaped                        | Poetic but drops the word microVM.                                                           |
| 5 | A Kubernetes operator for Firecracker microVMs                        | Accurate and boring. Good for project directories and listing pages.                         |

**Chosen:** candidate 1, "microVM isolation as a Kubernetes primitive". It is short enough to live cleanly under a centred logo, concedes nothing about the technical substrate ("Kubernetes primitive" is the exact positioning we want), and leaves the concrete details for the paragraph that immediately follows.

Candidate 2 is the fallback if reviewer feedback says "primitive" reads as too academic.

## Badge order

From the v0.1.0 chart, badges in README appear in this left-to-right order:

1. CI status (`ci.yml`)
2. Latest release (`release.yml`)
3. Apache 2.0 license
4. OSSF Scorecard (once the workflow has its first run and emits a URL)
5. CodeQL status
6. Kubernetes version compatibility (static shield, "v1.28+")

Rationale:

- CI first because the most common question a potential adopter asks is "does it build".
- Release second because adopters want to know if the project is tagged or pre-release.
- License third because regulated environments filter on license before anything else.
- Scorecard + CodeQL next because security-conscious reviewers expect the health signal.
- Kubernetes compatibility last because it is the most project-specific datum.

## 30-second pitch paragraph

The paragraph that follows the badges. Aim: answer "what is this" in under 30 seconds of scrolling without requiring the reader to know what Firecracker is.

**Preferred draft (commit this to README):**

> Setec is a Kubernetes operator that runs workloads inside Firecracker microVMs via Kata Containers. Declare a `Sandbox` custom resource and the operator materialises a hardware-isolated microVM for you, complete with lifecycle control, a programmatic gRPC frontend, snapshot / restore, and a pre-warm pool that delivers sub-100ms cold starts. Cloud-agnostic, self-hostable, Apache 2.0.

Character count: roughly 475. Sentences: three. Jargon:

- "Kubernetes operator" (assumed context; if not, the reader is in the wrong place).
- "Firecracker microVMs" - explicitly named because it is the load-bearing choice.
- "Kata Containers" - named because the reader needs to know this is not a fork.
- "CRD" - written as "custom resource" in prose; parenthesised "CRD" is fine later in the doc.
- "gRPC frontend" - parenthesise "(mTLS)" once if space permits.

**Alternative draft (shorter, less descriptive):**

> Setec turns `kubectl apply -f sandbox.yaml` into a Firecracker microVM. Cloud-agnostic, self-hostable, Apache 2.0.

Trade-off: punchier on first read, but buries the snapshot + pool story. Acceptable if reviewers say the preferred draft is too long.

## Sneakers reference

Rule: one brief mention per document, as colour. Preferred location in the README: the very end of the Community / Footer block, never the tagline or the 30-second pitch.

Form it should take:

> The name is a 1990s-movie reference to a group of security researchers. The goal is not to be cute; it is for hardware-isolated workloads to be boring infrastructure.

(One sentence, no quote, no plot summary.)

Do not:

- Lead with the etymology.
- Use Sneakers imagery in the logo.
- Repeat the reference in sub-page titles, headers, or subheads.

## Next-steps links

At the bottom of the README, after the example manifest:

- Getting started (narrative)
- Quickstart (terse)
- Documentation hub (`docs/README.md`)
- Example programs (`examples/`)
- Contributing guide
- Code of conduct
- Security policy

All relative paths. No mailto links outside `SECURITY.md`.

## Cloud-vendor grep check

README must return zero matches for any of:

- `AWS`
- `GCP`
- `Azure`
- `EKS`
- `GKE`
- `AKS`

The wording candidates above are clean. Keep it that way.
