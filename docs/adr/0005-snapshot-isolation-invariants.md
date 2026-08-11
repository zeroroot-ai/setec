# 0005 — Snapshot/restore isolation invariants (the untrusted-exec gate)

## Status

Accepted (2026-08-11)

## Context

Warm-start restores a paused snapshot into a Sandbox (ADR-0004). For an
untrusted-execution boundary, restore must not leak state between runs/sessions
or weaken crypto. Several of these are already built; this ADR makes the full
set the acceptance gate for enabling snapshot warm-start in production.

## Decision

A snapshot-restored (or resumed) Sandbox MUST satisfy:

1. **Clean base.** The pool snapshot is the class image booted to
   guest-agent-ready — no tenant data, no secrets, no prior input.
   (`internal/snapshot/secretscan` scans snapshots so none carries secrets at
   rest ✅; template *provenance* — built only from the class image, never a used
   sandbox — is invariant 4.)
2. **Per-restore uniquification.** Each restore/resume gets: a **fresh CSPRNG
   reseed** verified before hand-over (`RNDADDENTROPY` in the guest-agent + the
   coordinator's `EntropyReseeded` event, setec#72 ✅ — closes the Firecracker
   "cloned VMs share the CSPRNG" pitfall); a **unique vsock CID**; a **fresh k8s
   Pod network identity** (CNI-assigned, reconciling the snapshot's stale view);
   a **unique machine-id / hostname / boot-id**.
3. **One-session-then-destroy.** A VM (and its durable workspace + checkpoints)
   serves exactly **one session** — intra-session suspend/resume is allowed
   (ADR-0006), but it is **never** reused across sessions or tenants, and is
   wiped at session end.
4. **Template provenance.** The pool template is built only from a trusted class
   image, never from a used/dirty sandbox. (Enforced in code ✅ setec#190: the
   pool builder refuses a live pre-existing VM, every entry carries a
   provenance record bound into its sealed encryption key, and the pool
   manager destroys any entry whose record is missing or foreign.)
5. **Snapshot at rest.** Node-durable, secret-scanned ✅, encrypted at rest ✅
   (setec#190: per-artifact AES-256-GCM keys sealed by a node-local keyfile —
   never cluster-global; no plaintext write path), and destroyed with its
   pool/session ✅ (key destroyed first — crypto-erase — then ciphertext).

Enabling snapshot warm-start (or session checkpointing) in a non-dev namespace
is gated on 1-5 holding, with 2's entropy reseed *verified* per restore.

## Consequences

- Residuals to verify/close before GA: unique vsock CID, fresh net identity,
  machine-id/boot-id regeneration, and the one-session wipe path. (Entropy
  reseed, secret-scan, encryption-at-rest + key destruction, and template
  provenance are done.)
- A restore that cannot verify the entropy reseed fails closed.
