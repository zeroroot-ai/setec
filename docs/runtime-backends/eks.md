# AWS EKS playbook

Short playbook for choosing Setec runtime backends on Amazon EKS. One page, copy-pasteable. Verify every instance-type claim against current AWS documentation at install time — the EC2 catalog evolves quickly.

## What's available per node type

- **`.metal` instance types (bare metal)** — the Nitro bare-metal sizes (for example `m7i.metal-24xl`, `m7i.metal-48xl`, `m6i.metal`, `c7i.metal-*`, `r7i.metal-*`) expose Intel VT-x directly to the OS. These are the nodes where `kata-fc` works without fuss — `/dev/kvm` is present and KVM modules load normally. Graviton-based `.metal` sizes (for example `m7g.metal`, `c7g.metal`) expose ARM virt extensions; kata-fc support on ARM depends on the kata build — verify against vendor docs.
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

## References

- AWS nested virtualization documentation: https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/amazon-ec2-nested-virtualization.html
- AWS C8i/M8i/R8i nested virt announcement (Feb 2026): https://aws.amazon.com/about-aws/whats-new/2026/02/amazon-ec2-nested-virtualization-on-virtual/
- Setec runtime matrix and decision guide: [./README.md](./README.md)
