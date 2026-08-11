# setec — Context

microVM isolation as a Kubernetes primitive: setec runs untrusted code
(gibson's tools/agents) inside hardware-virtualized sandboxes, tenancy-unaware,
as gibson's sole untrusted-execution boundary (ADR-0052 open-core split).

## Ubiquitous language

- **Sandbox** — a single untrusted-execution unit (a `Sandbox` CR → one Pod →
  one microVM). The isolation boundary around one tool/agent run.
- **SandboxClass** — a named runtime profile: which **Backend** (isolation
  runtime) a Sandbox uses, plus resource/tuning params. Cluster-scoped.
- **Backend** — the isolation runtime for a Sandbox: `kata-fc` (Kata +
  Firecracker VMM), `kata-qemu` (Kata + QEMU VMM), `gvisor`, or `runc`. A
  SandboxClass may declare an ordered fallback chain.
- **VMM** — the virtual machine monitor under Kata: Firecracker, QEMU, or
  Cloud Hypervisor. "Firecracker" ⇒ Backend `kata-fc`.
- **North star (2026-08-11)** — a real gibson-executor tool run executing
  inside a Firecracker microVM on an actual KVM (bare-metal) node in staging,
  end-to-end: gibson dispatch → Sandbox → metal node → kata-fc → guest-agent →
  result. **Includes snapshot/restore** (warm per-run start), because per-run
  cold-boot latency caps throughput for a fresh-sandbox-per-tool model.
- **Substrate** — the node microVMs run on: **x86 bare-metal** EC2 only (nested
  virt ⇒ metal; x86 for tool-ecosystem compatibility and kata maturity). A
  pre-baked Packer AMI (kata-fc + devmapper thinpool) on a Karpenter
  on-demand, scale-to-zero NodePool. arm64 is unsupported (see ADR-0001).
- **Dispatch** — the gibson-daemon → setec-frontend hop. gibson authenticates
  over **SPIFFE** (SPIRE Workload API SVID); setec stays **generic mTLS**
  (trust bundle + allowed SPIFFE IDs, no SPIRE dependency). One caller identity
  (`platform/daemon`), so setec derives no tenancy from the cert (ADR-0002).

## Architecture decisions (see docs/adr)

- **Runtime** — stock **Kata + Firecracker** (`kata-fc`); setec does not own a
  shim or require a Kata fork (ADR-0003).
- **Node-prep** — setec owns a portable **installer DaemonSet** (kata-deploy
  style) that lays Kata+FC+devmapper on any x86 KVM node. Works on any cluster;
  the EKS baked-AMI + Karpenter path is an *optional* profile, never required
  (ADR-0003).
- **Warm-start** — a declarative SandboxClass knob (`PreWarmPoolSize`); setec
  automates the whole snapshot pool. **Operator manages zero templates.**
  Restore lands in a real `kata-fc` Pod via the node-agent's FC-socket path
  (ADR-0004), gated on the isolation invariants (ADR-0005).

- **Lifecycle** — a Sandbox is **ephemeral** (run-to-completion, auto-destroy,
  stateless; snapshot fast-start) or **session** (long-lived, reattach by
  handle, durable workspace, explicit teardown). setec#150 (ADR-0006).
- **Session survival (L2)** — a session always has a **durable workspace**
  (never lose corpus/findings) plus **memory checkpoints** for suspend-idle and
  resume-on-node-loss (process continues). Isolation: **one session per VM,
  wiped at session end**; intra-session suspend/resume ok (ADR-0005/0006).
- **Storage** — durable workspace on a **portable CSI volume** (continuous data
  safety), memory checkpoints on **S3-compatible object storage** (S3 or MinIO;
  process continuity). Both node-independent and portable; no AWS lock (ADR-0007).
