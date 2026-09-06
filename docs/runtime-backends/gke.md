# Google GKE playbook

Short playbook for choosing Setec runtime backends on Google Kubernetes Engine. Verify every node-type claim against the current GKE documentation — Google's supported-image and machine-type lists change regularly.

## What's available per node type

- **GKE Sandbox (gVisor) — natively supported.** GKE provides first-class gVisor support via GKE Sandbox. The image type must be `cos_containerd` (or `ubuntu_containerd`), and the sandbox flag is set per-node-pool. Sandbox cannot be enabled on the default node pool — create at least one additional pool with `--sandbox=type=gvisor`. See [GKE Sandbox concepts](https://docs.cloud.google.com/kubernetes-engine/docs/concepts/sandbox-pods) and [Harden workload isolation with GKE Sandbox](https://docs.cloud.google.com/kubernetes-engine/docs/how-to/sandbox-pods).
- **Nested virtualization on GKE Standard.** GKE Standard clusters support nested virtualization on Compute Engine VMs using Intel VT-x when the node image is `UBUNTU_CONTAINERD` (or `COS_CONTAINERD` on a recent enough GKE minor — verify against the current GKE [nested virtualization docs](https://docs.cloud.google.com/kubernetes-engine/docs/how-to/nested-virtualization)). This is the path for running `kata-qemu` on non-metal nodes. Nested virt is not available on Autopilot.
- **C3 bare-metal node pools.** C3 metal is the GKE node flavour that can expose KVM directly — it is the natural target for `kata-fc`. Availability, sizes, and the specific flag to request metal vary by region; verify against vendor docs before committing capacity.
- **Default non-metal, non-nested-virt pools.** No `/dev/kvm`. Practical backends: **gvisor** (via GKE Sandbox) and **runc**.

## Practical combinations on GKE

| Node pool kind | Recommended backends |
|---|---|
| C3 bare-metal pool (KVM exposed) | kata-fc (primary), kata-qemu, gvisor, runc |
| GKE Standard pool with nested-virt image (UBUNTU_CONTAINERD or COS_CONTAINERD at supported version) | kata-qemu (primary), gvisor (fallback) |
| GKE Sandbox pool (`--sandbox=type=gvisor`) | gvisor (primary), runc (dev only) |
| Autopilot or default Standard pools without sandbox/nested-virt | gvisor if GKE Sandbox pool added; otherwise runc (dev only) |

Verify capability on a target node:

```bash
kubectl debug node/<node-name> -it --image=alpine -- sh -c \
  'ls -l /dev/kvm 2>&1; cat /proc/cpuinfo | grep -E "vmx|svm" | head -1'
```

## Helm install — gVisor via GKE Sandbox

Create a sandbox-enabled node pool first, then deploy Setec with only gvisor (and optionally runc) enabled. GKE installs and manages `runsc` for you on sandbox pools, so Setec's node-agent only needs to detect the capability.

```bash
# 1. Add a sandbox node pool (one-time)
gcloud container node-pools create setec-gvisor \
  --cluster <cluster> --region <region> \
  --image-type cos_containerd \
  --sandbox type=gvisor \
  --machine-type n2-standard-4 \
  --num-nodes 2

# 2. Deploy Setec
helm upgrade --install setec oci://ghcr.io/zeroroot-ai/charts/setec \
  --namespace setec-system --create-namespace \
  --set runtimes.kata-fc.enabled=false \
  --set runtimes.kata-qemu.enabled=false \
  --set runtimes.gvisor.enabled=true \
  --set runtimes.gvisor.runtimeClassName=gvisor \
  --set runtimes.runc.enabled=false \
  --set defaults.runtime.backend=gvisor
```

GKE's sandbox pool registers the `gvisor` `RuntimeClass` cluster-wide. Set `runtimes.gvisor.install=false` in Setec's values if you do not want the Setec chart to render its own `RuntimeClass` object (recommended on GKE, to avoid conflicts).

## Running kata-fc on C3 metal

If you need microVM isolation for a subset of Sandboxes, create a C3 bare-metal node pool (verify current C3 metal SKU names and regional availability against vendor docs), taint it so only Sandboxes land there, install `kata-runtime` + register the `kata-fc` and `kata-qemu` `RuntimeClass` objects, and configure a dedicated `SandboxClass` with `runtime.backend=kata-fc` and `fallback: [gvisor]`. The rest of the cluster can keep using gVisor via GKE Sandbox. Verify against vendor docs for the current metal instance flags and kata installation procedure on GKE.

## References

- GKE Sandbox concepts: https://docs.cloud.google.com/kubernetes-engine/docs/concepts/sandbox-pods
- GKE Sandbox how-to: https://docs.cloud.google.com/kubernetes-engine/docs/how-to/sandbox-pods
- GKE nested virtualization: https://docs.cloud.google.com/kubernetes-engine/docs/how-to/nested-virtualization
- gVisor security model: https://gvisor.dev/docs/architecture_guide/security/
- Setec runtime matrix and decision guide: [./README.md](./README.md)
