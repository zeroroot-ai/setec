# 0007 — Portable storage: CSI workspace + S3-compatible checkpoint store

## Status

Accepted (2026-08-11)

## Context

Session survival (ADR-0006, L2) promises "resume on another node when a node
dies." A node-local workspace or checkpoint dies with the node, so both must
live on **node-independent, durable** storage — and per ADR-0003 (no AWS lock)
that storage must be **portable** to any cluster.

## Decision

- **Durable workspace = a portable CSI volume** (RWO PVC). On node loss the
  CSI driver re-attaches it to the failover node. Any CSI driver works, so it is
  cluster-portable; it holds the corpus/findings/worktree with **continuous**
  persistence — this is the data-safety layer, and it is never lost.
- **Memory checkpoints = the pluggable `StorageBackend` targeting S3-compatible
  object storage** — real S3 on EKS, **MinIO** (or any S3-compatible) when
  self-hosted. Node-independent and portable; this is the **process-continuity**
  layer.

Because the workspace already persists data continuously, **checkpoints are for
process continuity, not data safety, and can be infrequent** — the recovery
point is "restart the process from the durable corpus," not "lose work."
Checkpoint frequency is a tunable trade of bandwidth/cost vs replay-on-resume.

## Consequences

- Workspace volumes are CSI PVCs (encrypted at rest, per-session, wiped at
  session end per ADR-0005). Any CSI driver; no AWS dependency.
- The `StorageBackend` gains an S3-compatible object-store implementation
  alongside the local-disk default; self-hosted points it at MinIO.
- Large memory checkpoints pulled from object storage add failover latency;
  keeping checkpoints infrequent (data safety already handled) bounds the cost.
