# setec — Context

microVM isolation as a Kubernetes primitive: setec runs untrusted code
(gibson's tools/agents) inside hardware-virtualized sandboxes, tenancy-unaware,
as gibson's sole untrusted-execution boundary (docs/design/threat-model.md open-core split).

## Ubiquitous language

- **Sandbox** — a single untrusted-execution unit (a `Sandbox` CR → one
  launcher Pod → one Firecracker microVM). The isolation boundary around one
  tool/agent run.
- **SandboxClass** — a named runtime profile: resource ceilings, network
  modes, the warm pool, and checkpoints. Cluster-scoped. Its
  `runtime.backend` is `launcher` or empty.
- **Launcher** — the one runtime (setec#198): a launcher Pod runs one
  Firecracker machine from a signed image disk. The Kata, gVisor and runc
  backends were removed.
- **North star (2026-08-11)** — a real gibson-executor tool run executing
  inside a Firecracker microVM on an actual KVM (bare-metal) node in staging,
  end-to-end: gibson dispatch → Sandbox → KVM node → launcher → guest-agent →
  result. **Includes snapshot/restore** (warm per-run start), because per-run
  cold-boot latency caps throughput for a fresh-sandbox-per-tool model.
- **Substrate** — the node microVMs run on: **x86** nodes with KVM (nested
  virt or metal). Nothing is installed on the node: the KVM device plugin
  offers `/dev/kvm` and `/dev/net/tun`, and the launcher image carries
  Firecracker, the guest kernel and the guest agent (docs/design/runtime.md).
  arm64 is unsupported (see docs/design/runtime.md).
- **Dispatch** — the gibson-daemon → setec-frontend hop. Every hop is mTLS,
  and a component uses exactly one credential source: PEM files (the default)
  or the SPIFFE Workload API socket (docs/design/threat-model.md). In SPIFFE mode the server
  accepts only the SPIFFE IDs in its allow list. One caller identity
  (`platform/daemon`), so setec derives no tenancy from the cert (docs/design/threat-model.md).

## Architecture decisions (the ADR files live in the `docs` repo)

- **Runtime** — one Firecracker machine in each launcher Pod, booted from a
  squashfs disk that the disk builder made and signed for one image digest
  (docs/design/runtime.md, setec#198).
- **Node-prep** — none beyond `/dev/kvm`, `/dev/net/tun` and the traffic
  control modules. Works on any cluster (docs/design/runtime.md).
- **Warm-start** — a declarative SandboxClass knob (`PreWarmPoolSize`); setec
  keeps warm bases, snapshots of a machine that booted the image and ran no
  workload. **Operator manages zero templates.** A Sandbox of the class loads
  a base in its launcher Pod, gated on the isolation invariants
  (docs/design/isolation.md).

- **Lifecycle** — a Sandbox is **ephemeral** (run-to-completion, auto-destroy,
  stateless; snapshot fast-start) or **session** (long-lived, reattach by
  handle, durable workspace, explicit teardown). See docs/design/lifecycles.md.
- **Session survival (L2)** — a session always has a **durable workspace**
  (never lose corpus/findings) plus **memory checkpoints** for suspend-idle and
  resume-on-node-loss (process continues). Isolation: **one session per VM,
  wiped at session end**; intra-session suspend/resume ok (docs/design/isolation.md/0146).
- **Snapshot restore, checkpoints and fork.** A launcher Pod loads a snapshot
  before its machine runs, so a restore, a checkpoint resume and a fork all
  take the same path: the node agent stages the files, and the launcher
  reseeds and uniquifies the guest before it runs (setec#105).
- **Storage** — durable workspace on a **portable CSI volume** (continuous data
  safety), memory checkpoints on **S3-compatible object storage** (S3 or MinIO;
  process continuity). Both node-independent and portable; no AWS lock (docs/design/storage.md).
