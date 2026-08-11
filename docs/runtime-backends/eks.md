# AWS EKS playbook

Short playbook for choosing Setec runtime backends on Amazon EKS. One page, copy-pasteable. Verify every instance-type claim against current AWS documentation at install time — the EC2 catalog evolves quickly.

## What's available per node type

- **`.metal` instance types (bare metal)** — the Nitro bare-metal sizes (for example `m7i.metal-24xl`, `m7i.metal-48xl`, `m6i.metal`, `c7i.metal-*`, `r7i.metal-*`) expose Intel VT-x directly to the OS. These are the nodes where `kata-fc` works without fuss — `/dev/kvm` is present and KVM modules load normally. Graviton-based `.metal` sizes (for example `m7g.metal`, `c7g.metal`) are **not supported**: the sandbox substrate is x86 only ([ADR-0001](../adr/0001-x86-substrate.md)) — setec images are `linux/amd64` single-arch and every sandbox component pins `kubernetes.io/arch=amd64`.
- **Virtualized EC2 instances with nested-virt (C8i, M8i, R8i)** — AWS announced support for nested KVM/Hyper-V on C8i, M8i, and R8i virtual (non-metal) instances in February 2026 ([AWS announcement](https://aws.amazon.com/about-aws/whats-new/2026/02/amazon-ec2-nested-virtualization-on-virtual/)). On these instance types a non-metal EKS node can run `kata-fc` or `kata-qemu`. Confirm the region and launch template before depending on this — check current vendor docs.
- **All other default EKS node types (m7i/m6i/m5/c7i/c6i/t3/t3a/c7g/m7g etc., non-metal, non-C8i/M8i/R8i)** — do **not** expose `/dev/kvm`. Kata-fc and kata-qemu will fail to start Sandboxes on these nodes. Practical backends: **gvisor** and **runc**.

## Practical combinations on EKS

| Node pool kind | Recommended backends |
|---|---|
| `.metal` pool (any generation with KVM exposure) | kata-fc (primary), kata-qemu, gvisor, runc |
| C8i/M8i/R8i non-metal pool (nested-virt capable) | kata-qemu (primary), gvisor (fallback), runc (dev only) |
| All other non-metal pools | gvisor (primary), runc (dev only) |

Verify instance capabilities on a target node before depending on kata:

```bash
kubectl debug node/<node-name> -it --image=alpine -- sh -c \
  'ls -l /dev/kvm 2>&1; lsmod | grep -E "kvm_(intel|amd)" || echo "no kvm module"'
```

## Helm install — gvisor + runc on a default (non-metal) EKS cluster

Copy-paste starting point. Kata backends are disabled because `/dev/kvm` is not present. `runc` is installed but devOnly-gated.

```bash
helm upgrade --install setec oci://ghcr.io/zeroroot-ai/charts/setec \
  --namespace setec-system --create-namespace \
  --set runtimes.kata-fc.enabled=false \
  --set runtimes.kata-qemu.enabled=false \
  --set runtimes.gvisor.enabled=true \
  --set runtimes.gvisor.runtimeClassName=gvisor \
  --set runtimes.runc.enabled=true \
  --set runtimes.runc.devOnly=true \
  --set defaults.runtime.backend=gvisor \
  --set defaults.runtime.fallback='{runc}'
```

You must still install `runsc` and register a `gvisor` `RuntimeClass` on the worker nodes. For EKS-managed node groups this is typically done via a DaemonSet or a node bootstrap script; for Karpenter-provisioned nodes, add the install step to the NodeClass user-data. Setec does not install `runsc` on your behalf on managed EKS.

## Creating a metal pool for kata-fc

If you need `kata-fc` for a subset of workloads (for example, untrusted model-agent code), run those on a dedicated bare-metal node group and leave the rest of the fleet on the default pool. Create a managed node group with a `.metal` instance type (verify current availability — `m7i.metal-24xl`, `c7i.metal-*`, `r7i.metal-*` are common at time of writing; check current vendor docs), taint it so only Sandboxes land there, and set the matching SandboxClass to request `kata-fc` with `fallback: [gvisor]`. Setec's node-agent will label the metal nodes `setec.zeroroot.ai/runtime.kata-fc=true` once KVM is detected, and the scheduler will place Sandboxes accordingly. See the top-level Kata installation docs for `runsc`- and `kata-runtime`-on-EKS procedures; verify against vendor docs for your EKS version.

## Baked x86-metal AMI for kata-fc (recommended)

The recommended production path for `kata-fc` on EKS is the **Packer-baked
immutable AMI** in [`packer/eks-kata-fc-ami/`](../../packer/eks-kata-fc-ami/README.md)
— it replaces kata-deploy's live containerd mutation (which can brick a node
mid-run) with a node that either boots capable or fails loudly at boot:

- **Base**: current EKS-optimized AL2023 **x86_64** AMI (pinned Kubernetes
  version via the public SSM parameter). arm64 is unsupported per
  [ADR-0001](../adr/0001-x86-substrate.md); the Packer template still
  reflects the earlier Graviton bake and its x86 rebake is tracked in
  [setec#195](https://github.com/zeroroot-ai/setec/issues/195).
- **Targets**: cheapest x86 bare metal with local NVMe —
  **`c6id.metal` / `m6id.metal`**. `.metal` supplies `/dev/kvm` (VT-x); the
  `d` suffix supplies the instance-store NVMe the devmapper thin-pool is
  built from. (The runtime-agent probe verifies the loaded
  `kvm_intel`/`kvm_amd` module, so the node self-labels
  `setec.zeroroot.ai/runtime.kata-fc=true`.)
- **Baked in**: pinned kata-containers static release (bundles Firecracker)
  under `/opt/kata`; containerd statically configured via an
  `/etc/containerd/config.d/` drop-in (kata-fc handler + devmapper
  snapshotter); a boot-time `setec-thinpool.service` that provisions the
  thin-pool from local NVMe idempotently (and rebuilds it after a
  stop/start wiped the instance store); a static `kata-fc` RuntimeClass
  manifest with kata-deploy-parity `overhead` at
  `/etc/setec/manifests/runtimeclass-kata-fc.yaml`.
- **No kata-deploy DaemonSet** on these nodes, ever. Upgrades are a new
  bake + node roll, never in-place mutation.

Chart settings when running on baked nodes: keep
`runtimes.kata-fc.enabled=true` and set `runtimes.kata-fc.install=false`,
then `kubectl apply -f` the baked RuntimeClass manifest once per cluster (it
carries the `overhead.podFixed` block the chart-rendered variant omits).
Build instructions and the on-node verification checklist (including the
`ctr plugins ls` devmapper check and a guest-kernel smoke pod) live in the
[packer README](../../packer/eks-kata-fc-ami/README.md).

## Karpenter scale-to-zero for the baked kata AMI

The chart can render a Karpenter `EC2NodeClass` + `NodePool` (Karpenter >=
1.0, installed out of band) that provision the cheapest x86 bare metal
(`c6id.metal` / `m6id.metal`) from the [baked kata-fc AMI](#baked-x86-metal-ami-for-kata-fc-recommended)
**on demand** and **scale to zero** when no kata Sandbox is running:

```bash
helm upgrade --install setec oci://ghcr.io/zeroroot-ai/charts/setec \
  --namespace setec-system \
  --reuse-values \
  --set karpenter.enabled=true \
  --set karpenter.role=<node-iam-role-name> \
  --set-json 'karpenter.subnetSelectorTerms=[{"tags":{"karpenter.sh/discovery":"<cluster>"}}]' \
  --set-json 'karpenter.securityGroupSelectorTerms=[{"tags":{"karpenter.sh/discovery":"<cluster>"}}]'
```

How the pieces line up:

- The `EC2NodeClass` selects the baked AMI (`setec-kata-fc-*` by name, or
  pin an AMI id) with `amiFamily: AL2023` so Karpenter emits standard
  nodeadm user data.
- The `NodePool` requires `kubernetes.io/arch=amd64` (ADR-0001) and
  restricts to `c6id.metal` / `m6id.metal` (on-demand only by default),
  stamps the `setec.zeroroot.ai/runtime.kata-fc=true` label the
  kata-fc RuntimeClass schedules on, and taints the node
  `kata=true:NoSchedule` so only Sandbox pods (and tolerating DaemonSets,
  like Setec's runtime-agent) land on the expensive metal.
- SandboxClasses targeting kata-fc must tolerate the taint via
  `spec.tolerations` (propagated to every Sandbox pod under the class):

  ```yaml
  apiVersion: setec.zeroroot.ai/v1alpha1
  kind: SandboxClass
  metadata:
    name: kata-metal
  spec:
    vmm: firecracker
    runtimeClassName: kata-fc
    tolerations:
      - key: kata
        operator: Equal
        value: "true"
        effect: NoSchedule
  ```

- Creating a Sandbox with no warm node leaves its pod pending → Karpenter
  provisions a metal node from the baked AMI (boots kata-fc-capable, no
  kata-deploy) → the microVM runs. After `karpenter.consolidateAfter`
  (default 5m) with no Sandbox pods, `WhenEmpty` consolidation deprovisions
  the node.

### Cost model

- **On-demand metal floor**: you pay the `.metal` hourly rate only while a
  node exists. Karpenter picks the cheapest eligible type automatically
  (verify current pricing across the eligible x86 metal types). `karpenter.limits` caps runaway scale-out.
- **Scale-to-zero**: idle cost ≈ $0 — no standing node group, no
  kata-deploy DaemonSet keeping nodes warm. The trade is the cold-start
  gap (metal boot, typically minutes); Setec's warm-pool/snapshot machinery
  bridges bursts *within* a node's lifetime, and a longer
  `consolidateAfter` bridges bursts *between* Sandboxes.
- **Spot caveat**: `capacityTypes` defaults to on-demand only. Spot metal
  is materially cheaper, but an interruption kills every running microVM on
  the node with two minutes' notice — opt in only for workloads that
  tolerate losing in-flight Sandboxes.

## References

- AWS nested virtualization documentation: https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/amazon-ec2-nested-virtualization.html
- AWS C8i/M8i/R8i nested virt announcement (Feb 2026): https://aws.amazon.com/about-aws/whats-new/2026/02/amazon-ec2-nested-virtualization-on-virtual/
- Setec runtime matrix and decision guide: [./README.md](./README.md)
