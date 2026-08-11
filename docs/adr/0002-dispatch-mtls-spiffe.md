# 0002 — gibson→setec dispatch authenticates over SPIFFE; setec stays generic mTLS

## Status

Accepted (2026-08-11)

## Context

The gibson daemon dials the setec frontend to create/run Sandboxes. Two
incompatible mTLS mechanisms existed (deploy#1326):

1. **File-cert** — implemented in the daemon: `SandboxConfig.Setec.MTLS` is a
   file-path TLS config (`cert_file`/`key_file`/`ca_file`).
2. **SPIFFE** — the trust mechanism every *other* platform hop uses, via the
   SPIRE Workload API (auto-rotated SVIDs).

Constraints: setec is an **Apache OSS standalone** operator and must not
hard-depend on gibson's SPIRE. The client-cert identity is load-bearing at the
setec frontend (a stale/legacy code comment even claimed setec derived
*tenancy* from the client-cert CN — which contradicts ADR-0052's
"setec is tenancy-unaware"). A silently-wrong wiring fails closed at the
frontend or crashloops the daemon.

## Decision

**gibson dials setec over the SPIFFE Workload API socket** using the daemon's
SPIRE `X509Source` (`spiffe://zeroroot.ai/platform/daemon`, auto-rotated). The
daemon's file-cert `SandboxConfig.Setec.MTLS` path is **removed** — one trust
mechanism, no parallel codepath (ADR-0027 wholesale-flip discipline).

**setec-frontend stays generic mTLS** and never learns "SPIRE": it is configured
with a `trustBundle` (the SPIRE trust bundle, mounted from the `spire-bundle`
ConfigMap and refreshed as it rotates) and an `allowedSpiffeIDs` list, and
authorizes the caller by its **URI SAN**, not a CN. A self-hosted setec without
SPIRE supplies its own CA + allowed identities through the same generic knobs.

Because gibson presents a **single** daemon SVID, setec sees exactly one caller
identity and derives **no tenancy** from the certificate — reinforcing ADR-0052
(setec is tenancy-unaware); tenant isolation stays entirely in gibson.

## Consequences

- Daemon: `SandboxConfig.Setec` gains a SPIFFE source and drops the file-cert
  fields; no cert files to provision, mount, or rotate.
- setec: frontend gains `trustBundle` + `allowedSpiffeIDs` config and a
  bundle mount; no SPIRE dependency enters the OSS codebase.
- The obsolete "tenancy from client-cert CN" behaviour/comment is deleted.
- Self-hosted setec (no SPIRE) still works via the generic mTLS knobs.
