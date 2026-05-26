# Azure AKS playbook

Short playbook for choosing Setec runtime backends on Azure Kubernetes Service. Verify every SKU claim against the current Azure documentation — Azure's supported-size lists change regularly.

## What's available per node type

- **Most standard AKS SKUs do not support nested virtualization.** Nested virt on Azure is limited to specific VM series. Dv3/Dv4/Dv5 and Ev3/Ev4/Ev5 series are commonly cited as nested-virt capable (for example by Azure's KubeVirt guidance), but the capability depends on the exact CPU model the scheduler places your VM on — Azure does not guarantee a specific CPU within a size family. **Verify against vendor docs** before depending on kata-qemu on AKS, and smoke-test `/dev/kvm` availability per node rather than trusting the SKU name alone.
- **No public Azure bare-metal Kubernetes node SKU.** AKS does not have an equivalent of EKS `.metal` or GKE C3 metal for direct-to-hardware KVM. If `kata-fc` is a hard requirement, consider Azure Dedicated Host + self-managed Kubernetes, or run Setec on on-prem hardware — do not plan on `kata-fc` being available on standard AKS node pools.
- **Standard AKS pools (everything else).** No `/dev/kvm`. Practical backends: **gvisor** and **runc**.

## Practical combinations on AKS

| Node pool kind | Recommended backends |
|---|---|
| Dv5/Ev5 (or other nested-virt-capable series, verified per node) | kata-qemu (primary, subject to per-node verification), gvisor (fallback) |
| Standard AKS pool (most SKUs) | gvisor (primary), runc (dev only) |
| Azure Dedicated Host / self-managed K8s on bare-metal Azure | kata-fc possible — verify against vendor docs |

Verify capability on a target node:

```bash
kubectl debug node/<node-name> -it --image=alpine -- sh -c \
  'ls -l /dev/kvm 2>&1; cat /proc/cpuinfo | grep -E "vmx|svm" | head -1; dmesg | grep -i kvm | head -5'
```

## Helm install — gvisor + runc on a standard AKS cluster

AKS does not install `runsc` for you. You must install `runsc` on the worker nodes (typically via a DaemonSet-driven installer) and register a `gvisor` `RuntimeClass`. Then deploy Setec:

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

If you later add a Dv5/Ev5 pool and verify nested virt works on those nodes, enable `kata-qemu` additively — do not flip the default backend cluster-wide without validating every node pool, because AKS can place a VM on a CPU model that does not support nested virt even within a "supported" size family.

## Notes

- AKS also ships its own pod-sandboxing preview feature built on kata-containers in some regions; that stack is orthogonal to Setec's runtime abstraction. If you use it, set `runtimes.gvisor.install=false` and let AKS manage the `RuntimeClass` objects. Verify against vendor docs for the current preview state and SKU requirements.
- For multi-tenant production isolation on AKS, prefer `gvisor` over `runc`. `runc` is gated by Setec's `devOnly` mechanism for a reason: it is container-only isolation and kernel bugs reachable from the container are host compromises.

## References

- Azure nested virtualization reference material is scattered across the Microsoft Learn domain; start from the general nested-virtualization guidance and verify for your specific VM series and region before committing to kata-qemu on AKS.
- gVisor security model: https://gvisor.dev/docs/architecture_guide/security/
- runc advisories (why `runc` is devOnly): https://github.com/opencontainers/runc/security/advisories
- Setec runtime matrix and decision guide: [./README.md](./README.md)
