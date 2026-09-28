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

## Addendum (2026-09-27): kata-fc's workspace PVC is Block-mode, not Filesystem

setec#91: `TestSession_WorkspaceSurvivesPodKill` showed that on kata-fc a
session's workspace did not survive a VM restart. Kata Containers +
Firecracker carries no virtio-fs, so kata cannot share a host directory with
the guest the way it does on kata-qemu — it copies a filesystem-mode volume's
contents into the guest once, at container start, and guest writes never
reach the PVC back on the host. A raw block device is the one volume type
Firecracker can attach to the guest (the same mechanism kata already uses for
a container's own rootfs).

**Decision (owner, 2026-09-27):** on the kata-fc backend only, the session
workspace PVC is provisioned with `volumeMode: Block` and the Pod's workload
container consumes it via `volumeDevices` instead of `volumeMounts`. The
container's command becomes the static `cmd/setec-keepalive` binary (the same
one that boots a command-less session, setec#7), which formats the raw device
as ext4 — only if it carries no filesystem yet, so a session VM that restarts
against the same PVC never loses its corpus (`internal/workspace.FormatOnce`)
— mounts it onto an ordinary emptyDir at `/workspace`, and then either execs
the Sandbox's own `spec.command` (a session that declared one) or falls into
the existing keepalive reap loop (a session that declared none).

The format and mount happen inside the *same* container that will run the
workload, in that container's own mount namespace, rather than in a separate
init container that shares its mount with a sibling. The alternative design —
an init container mounting onto a shared emptyDir with `Bidirectional`
propagation so the workload container sees it via `HostToContainer` — was
tried and rejected empirically: the chart's own
`sandbox-namespace-host-guard` ValidatingAdmissionPolicy (setec#159) refuses
*any* privileged container in a Sandbox namespace, full stop, and Kubernetes
requires exactly that (`privileged: true`) to grant `Bidirectional`
propagation at all. That guard is deliberately absolute — unlike individual
capabilities, which ADR-0052 already treats as costing nothing extra because
the microVM is the containment boundary, `privileged` hands the container the
node itself, which no backend's containment model excuses. The one-container
design needs only `CAP_SYS_ADMIN` for the `mount(2)` call, added to the same
capability set that already carries NET_RAW/NET_ADMIN, and it never reaches
the Sandbox's own command: an exec'd binary with no file capabilities of its
own gets an empty effective/permitted set regardless of what the exec'ing
process held, because nothing here populates the ambient capability set.

gVisor and runc keep the original filesystem-mode PVC unchanged: neither has
kata-fc's virtio-fs gap, and this addendum introduces no new code path for
either backend (ADR-0027).

This does not change the workspace's durability contract: the volume is still
a portable RWO CSI PVC (any Block-capable CSI driver works — most CSI
drivers, including the AWS EBS CSI driver, support both volumeMode values
from the same StorageClass, since the mode is a property of the PVC request,
not the class), still re-attaches to a fresh Pod on node loss, and still
holds the corpus/findings/worktree with continuous persistence. Only how the
guest turns that block device back into a mounted filesystem is new.
