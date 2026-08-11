# 0003 — setec is a stock-Kata consumer and owns a portable node installer

## Status

Accepted (2026-08-11)

## Context

setec must be **plug-and-play and portable**, not AWS-locked. It needs
KVM-capable nodes running Kata Containers + Firecracker + the devmapper
snapshotter. Two temptations were rejected:

- **Own the runtime / reinvent the shim** (a setec-native Firecracker shim).
  Considered — it would make snapshot native and setec fully self-contained —
  but the owner wants to keep **stock Kata+FC**: Firecracker+jailer stays the
  proven isolation boundary and setec does not take on a security-critical
  runtime.
- **Require a baked EC2 AMI + Karpenter** as the only way to get sandbox nodes.
  Rejected: it locks setec to AWS/EKS. It remains available as an *optimization*
  but must not be a requirement.

## Decision

setec is a **stock Kata + Firecracker (`kata-fc`) consumer** — no forked/patched
Kata as a requirement, no setec-owned shim.

setec **owns node provisioning via a portable installer DaemonSet**
(kata-deploy style): given any x86 KVM-capable Kubernetes node, the installer
lays down Kata+FC, configures containerd + the devmapper thin-pool (boot-ordered
so containerd starts after it), and registers the `kata-fc` RuntimeClass. This
is the **default** and works on any cluster.

The **baked-AMI + Karpenter bare-metal** path (EKS) is an **optional deployment
profile** that pre-bakes the same components for faster node-ready, selected by
Helm values — never required.

## Consequences

- setec ships and maintains the installer DaemonSet (privileged; must handle the
  common node OSes it supports) — the plug-and-play cost lives here, not with the
  operator.
- Snapshot/restore uses stock-Kata mechanisms only (see ADR-0004); no Kata fork.
- EKS keeps the fast AMI path as an opt-in; correctness never depends on it.
- A self-hosted customer with their own KVM nodes installs setec and gets
  sandboxes with no AWS anything.
