# Setec kata-fc Graviton-metal EKS AMI (Packer)

> **Optional deployment profile** (ADR-0003). The DEFAULT node-prep path
> is the chart's portable installer DaemonSet (`installer.enabled=true`),
> which converges any x86 KVM-capable node with no AWS dependency. This
> AMI pre-bakes the same components for faster node-ready on EKS +
> Karpenter (`karpenter.enabled=true`) — an optimisation, never a
> requirement.
>
> **STALE — arm64 bake (setec#195).** The sandbox substrate is x86 only
> ([ADR-0001](../../docs/adr/0001-x86-substrate.md)); the chart's Karpenter
> NodePool now requires `kubernetes.io/arch=amd64` and defaults to
> `c6id.metal` / `m6id.metal`, so the arm64/Graviton AMI this template
> currently bakes can no longer be provisioned by it. The x86 rebake of this
> template is tracked in
> [setec#195](https://github.com/zeroroot-ai/setec/issues/195).

Bakes an **immutable** arm64 EKS node AMI that boots ready to run `kata-fc`
Firecracker microVMs — **no kata-deploy**, no live containerd mutation. A
node either boots capable or fails loudly in `setec-thinpool.service`.

Built from the current **EKS-optimized AL2023 arm64** base (resolved via the
public SSM parameter for the pinned Kubernetes version). Target instance
types are the cheapest Graviton bare metal with local NVMe: **`c6gd.metal` /
`m6gd.metal`** (KVM requires `.metal`; the devmapper thin-pool requires the
`d` suffix's NVMe instance store).

## What the AMI bakes in

| Piece | Where | Why |
|---|---|---|
| kata-containers static release (pinned, includes Firecracker + jailer + guest kernel/rootfs) | `/opt/kata` | the VMM stack |
| `containerd-shim-kata-fc-v2` symlink | `/usr/local/bin` | kata-deploy-parity shim resolution for `runtime_type = "io.containerd.kata-fc.v2"` |
| static containerd drop-in: kata-fc handler + devmapper snapshotter | `/etc/containerd/config.d/99-setec-kata-fc.toml` | the nodeadm-rendered containerd config imports `config.d/*.toml` at every boot — zero runtime rewrites. The config **schema version (2 vs 3) is detected once at bake time** from the base image's containerd |
| boot-time thin-pool provisioner | `setec-thinpool.service` → `/usr/local/sbin/setec-thinpool.sh` | builds an LVM thin-pool (`setec-thinpool`) from unused NVMe **instance-store** devices, idempotent across reboots; rebuilds + clears stale devmapper snapshotter state after a stop/start wiped the ephemeral disks. Ordered `Before=containerd.service`, and containerd `Requires=` it |
| static RuntimeClass manifest | `/etc/setec/manifests/runtimeclass-kata-fc.yaml` (also in `files/`) | `kata-fc` handler, kata-deploy-parity `overhead.podFixed` (130Mi / 250m), scheduling nodeSelector `setec.zeroroot.ai/runtime.kata-fc=true` |

Firecracker needs a block device per container rootfs (no overlayfs) — hence
the devmapper snapshotter bound to the local-NVMe thin-pool. Regular (runc)
pods on the node keep using the default overlayfs snapshotter on the EBS
root; only `kata-fc` pods hit devmapper (per-runtime `snapshotter` field).

## Build

```bash
cd packer/eks-kata-fc-ami
packer init .
packer validate .
packer build \
  -var 'region=us-east-1' \
  -var 'k8s_version=1.33' \
  -var 'kata_version=3.28.0' \
  -var 'kata_sha256=<sha256 of kata-static-3.28.0-arm64.tar.xz>' \
  .
```

The build instance is a cheap non-metal Graviton (`m7g.xlarge` default) —
baking only writes files; KVM is not needed until a node runs microVMs.

| Variable | Default | Notes |
|---|---|---|
| `region` | `us-east-1` | build region |
| `k8s_version` | `1.33` | selects the EKS-optimized AL2023 arm64 base via SSM |
| `kata_version` | `3.28.0` | pinned kata static release (bundles Firecracker) |
| `kata_sha256` | `""` | **pin this** for a reproducible bake; empty falls back to the release `.sha256sum` sidecar |
| `build_instance_type` | `m7g.xlarge` | any arm64 type works |
| `ami_name_prefix` | `setec-kata-fc` | AMI selectors should match `setec-kata-fc-*` |
| `root_volume_size_gb` | `100` | EBS root (images via overlayfs, kata guest artifacts) |

## Launching nodes

Launch the AMI on `c6gd.metal` / `m6gd.metal` with standard EKS **nodeadm**
user data (managed node group launch template or Karpenter `EC2NodeClass`
with `amiFamily: AL2023` semantics — see `docs/runtime-backends/eks.md`).
The bake changes nothing about node bootstrap: nodeadm joins the cluster
exactly as on the stock AMI.

Apply the RuntimeClass once per cluster (or let the Setec chart render it
with `runtimes.kata-fc.install=true` — the chart variant carries no
`overhead` block; prefer the static manifest for kata-deploy-parity
scheduler accounting):

```bash
kubectl apply -f files/runtimeclass-kata-fc.yaml
```

Setec's runtime-agent DaemonSet probes the node (`/dev/kvm` + the built-in
arm64 `kvm` module) and labels it `setec.zeroroot.ai/runtime.kata-fc=true`,
which satisfies the RuntimeClass scheduling nodeSelector.

## On-node verification checklist

```bash
# 1. node joined and KVM exposed
kubectl get nodes -l setec.zeroroot.ai/runtime.kata-fc=true
kubectl debug node/<node> -it --image=alpine -- ls -l /host/dev/kvm

# 2. containerd: kata-fc handler + devmapper ok, with NO kata-deploy
#    (on the node, e.g. via SSM session)
ctr plugins ls | grep devmapper          # ... devmapper ... ok
crictl info | grep -A3 '"kata-fc"'

# 3. a kata-fc pod boots a real microVM (guest kernel != host kernel)
kubectl run kata-smoke --restart=Never --image=alpine \
  --overrides='{"spec":{"runtimeClassName":"kata-fc"}}' \
  -- sh -c 'uname -r'
kubectl logs kata-smoke   # compare with `uname -r` on the node

# 4. thin-pool survives a reboot; rebuilt after stop/start
systemctl status setec-thinpool.service
dmsetup info setec-thinpool
```

## Upgrades

Never mutate a running node. Bump `kata_version` / `k8s_version`, rebuild,
and roll nodes onto the new AMI (Karpenter drift or a node-group AMI
update). That is the whole point.
