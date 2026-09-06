# 0004 — Warm-start is an automated, operator-templateless snapshot pool

## Status

Accepted (2026-08-11)

## Context

Per-run cold-boot latency (Kata + rootfs + guest-agent + tool startup — seconds)
caps throughput for a fresh-sandbox-per-tool model, so snapshot/restore
warm-start is part of "done" (not optional). But the owner will **not** manage a
bunch of templates, and it must run on **stock Kata** (ADR-0003). Kata exposes no
restore API upstream.

## Decision

Warm-start is a **single declarative knob** on a SandboxClass
(`PreWarmPoolSize`, `PreWarmImage`, `PreWarmTTL`). Everything else is
**automated by setec** — the node-agent builds, refreshes, restores, and
destroys the paused-snapshot pool from the class image; the operator never
curates a snapshot or template.

Restore lands in a **real `kata-fc` Pod** (full k8s-native: CNI, NetworkPolicy,
observability, `kubectl`). Because stock Kata has no restore API, setec's
node-agent drives restore through the **Kata VM's Firecracker socket**
(the existing snapshot Coordinator path) rather than a Kata fork or an
operator-managed VM factory. The three internal layers — SandboxClass API →
node-agent snapshot pool → frontend Lease/Coordinator — are **one flow**.

Acceptance is **gated on the per-restore isolation invariants** (ADR-0005).

## Consequences

- Operator UX: set a pool size, get fast sandboxes; zero template management.
- setec owns the snapshot lifecycle + the stock-Kata socket-restore robustness
  (the engineering risk, not the operator's).
- If socket-restore proves unreliable on stock Kata, the fallback is an
  *upstreamed* Kata restore capability — never operator-visible templates.
