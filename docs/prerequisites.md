# Prerequisites

Setec runs each `Sandbox` as one Firecracker machine in one launcher Pod.
The operator itself has modest requirements. The Nodes that run `Sandbox`
workloads must expose KVM. This document explains what that means and how
to prepare a cluster. [docs/design/runtime.md](design/runtime.md) describes
the parts of the runtime.

## KVM

Firecracker is a [Kernel-based Virtual Machine](https://www.linux-kvm.org/)
(KVM) monitor. It boots a guest kernel inside a hardware-virtualized
context that the CPU and the Linux KVM subsystem provide. That hardware
boundary is what makes microVM isolation stronger than shared-kernel
container isolation: a workload that escapes its process still faces a
full guest kernel and a virtualization boundary before it reaches the host.
Without `/dev/kvm`, no launcher Pod can start a machine.

A Node needs direct or pass-through access to the CPU's virtualization
extensions (Intel VT-x / AMD-V) exposed through `/dev/kvm`. In practice
that means one of the following:

- A **bare-metal Linux host**. The virtualization extensions are available
  natively and KVM works out of the box (given an appropriate kernel).
- A **VM with nested virtualization enabled**. The outer hypervisor must
  pass VT-x/AMD-V into the guest. Nested virtualization costs performance,
  and its configuration varies by hypervisor. If the guest does not see
  `/dev/kvm`, nested virtualization is not enabled.

Verify KVM availability on a candidate Node:

```bash
# On the Node itself (e.g., via SSH or a debug Pod):
ls -l /dev/kvm
kvm-ok   # from the cpu-checker package on Debian/Ubuntu-like distros
```

## What else a Node needs

- **x86-64 (amd64).** Each published setec image is `linux/amd64`.
- **`/dev/net/tun`** and the kernel modules `tun`, `sch_ingress`,
  `cls_matchall` and `act_mirred`. The launcher joins its machine to the
  Pod network with a tap device and two traffic redirects.
- **Kubernetes 1.35 or later.** Each launcher Pod mounts the signed disk of
  its image as an image volume.

Nothing else is installed on the Node. Firecracker, the guest kernel and
the guest agent come in the launcher image.

## What the cluster needs

- **An OCI registry for the signed image disks** (`launcher.diskRepo`). The
  disk builder pushes each disk there, and the kubelet pulls it with the
  image credentials of the Node.
- **A disk signing key.** A Secret holds the base64 ed25519 seed under the
  key `seed` (`launcher.diskBuilder.signingSecret`), and the chart lists
  its public keys (`launcher.diskBuilder.publicKeys`). Each launcher refuses
  a disk that none of the keys signed.

The chart refuses to render without these three values.

## Node capability

The KVM device plugin DaemonSet offers `/dev/kvm` and `/dev/net/tun` of
each Node as the extended resources `setec.zeroroot.ai/kvm` and
`setec.zeroroot.ai/tun`. A launcher Pod asks for one of each, so the
scheduler places it only on a Node that offers them. A Node without
`/dev/kvm` advertises the resources as unhealthy.

Check node capability:

```bash
kubectl get nodes -o custom-columns='NAME:.metadata.name,KVM:.status.allocatable.setec\.zeroroot\.ai/kvm'
```

Setec does not detect, depend on, or favor any cloud or vendor. Any
conformant Kubernetes distribution whose Nodes meet these requirements
works.

## Representative consumer scenarios

Setec is a substrate. These are illustrative workload patterns — not
endorsements of any specific downstream product.

- **AI agent code execution.** An agent system generates code on the fly
  and needs to execute it against real interpreters (Python, shell, etc.)
  without granting that code access to the host, the agent's runtime, or
  other tenants' data.
- **CI and build sandboxing.** Per-job microVMs run untrusted build
  scripts, `Dockerfile` instructions, or post-install hooks from third-
  party packages with a hardware isolation boundary between jobs.
- **Security research.** Malware triage, detonation of suspicious
  samples, or fuzzing harnesses run inside short-lived microVMs that are
  discarded after each run.
- **Ephemeral developer environments.** A platform provisions a fresh
  microVM per pull-request preview or per interactive session, isolating
  the user's environment from every other user's and from the platform's
  control plane.

In all four cases the interface is the same: apply a `Sandbox` CR, read
the phase and logs, delete it. Consumers talk to the CRD (or, in a future
phase, a gRPC frontend); Setec is unaware of and undifferentiated by who
its consumers are.
