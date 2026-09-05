# 0001 — The microVM substrate is x86 only

## Status

Accepted (2026-08-11)

## Context

setec runs an evolving set of untrusted **security tools** and their runtime
payloads (nuclei templates, downloaded exploits, C dependencies) inside Kata
microVMs. The CPU architecture of the sandbox node cascades into the entire
tool build-matrix. x86 is the universal target of the security-tooling
ecosystem; some tools ship x86-only, and on arm64 every tool + transitive dep
must be audited and multi-arch-built forever. Firecracker/Kata are also most
battle-tested on x86. arm64/Graviton is ~cheaper but the pool is scale-to-zero,
so the saving is marginal.

This ADR is about **architecture only** — *how* KVM-capable nodes are provided
(EKS AMI, self-hosted, etc.) is ADR-0003.

## Decision

**The sandbox substrate is x86, exclusively.** KVM-capable nodes must be x86;
the gibson-executor image and every tool image are built `linux/amd64` only
(no multi-arch, so nothing silently attempts arm64). arm64 is unsupported for
the untrusted-execution plane; re-adding it is a new ADR.

## Consequences

- Node pools / AMIs / installers target x86 KVM-capable nodes.
- Images are single-arch amd64; the multi-arch tax is avoided.
- Higher node cost accepted for universal tool compatibility + kata maturity.
