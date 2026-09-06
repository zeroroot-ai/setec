# Prerequisites

Setec runs workloads inside one of four runtime backends. The operator
itself has modest requirements — but the Nodes that run `Sandbox`
workloads must meet the requirements of at least one enabled backend.
This document explains what that means per backend and how to prepare a
cluster. For a side-by-side isolation / CVE-surface / overhead matrix
plus managed-K8s platform playbooks, see
[docs/runtime-backends/](runtime-backends/README.md).

## Choose a backend

| Backend | Isolation | Node requirement | Typical use |
|---|---|---|---|
| `kata-fc` | microVM (Firecracker) | `/dev/kvm` + Kata Containers | Bare metal / nested-virt; strongest default |
| `kata-qemu` | microVM (QEMU) | `/dev/kvm` + Kata Containers | Same model, QEMU VMM; TCG fallback where KVM absent |
| `gvisor` | User-space kernel (Sentry) | `runsc` binary + gvisor RuntimeClass | Managed K8s without nested-virt |
| `runc` | Namespaces + cgroups | Any container runtime (dev-only) | Local dev, feature-flagged |

## kata-fc / kata-qemu: KVM requirement

Kata's Firecracker and QEMU VMMs are both [Kernel-based Virtual Machine](https://www.linux-kvm.org/)
(KVM) monitors. They boot a guest kernel inside a hardware-virtualized
context provided by the host's CPU and the Linux KVM subsystem. That
hardware boundary is what makes microVM isolation stronger than
shared-kernel container isolation: a workload that escapes its namespace
still faces a full guest kernel and a virtualization boundary before it
reaches the host. Without KVM (`/dev/kvm`), neither VMM can start a VM,
Kata cannot schedule a Kata-runtime Pod, and these backends are unusable.
`kata-qemu` has a TCG (pure-software) fallback, but it is 10-100× slower
and Setec does not surface it as a normal path.

A Node needs direct or pass-through access to the CPU's virtualization
extensions (Intel VT-x / AMD-V) exposed through `/dev/kvm`. In practice
that means one of the following:

- A **bare-metal Linux host** — virtualization extensions are available
  natively and KVM works out of the box (given an appropriate kernel).
- A **VM with nested virtualization enabled** — the outer hypervisor must
  be configured to pass VT-x/AMD-V into the guest. Nested virt carries a
  performance cost and configuration varies by host hypervisor; consult
  your hypervisor's documentation. If the guest does not see `/dev/kvm`,
  nested virt is not enabled.

Verify KVM availability on a candidate Node:

```bash
# On the Node itself (e.g., via SSH or a debug Pod):
ls -l /dev/kvm
kvm-ok   # from the cpu-checker package on Debian/Ubuntu-like distros
```

For `kata-fc`, KVM is the only prerequisite: **the Setec chart prepares
the node for you**. The portable installer DaemonSet (ADR-0003,
`installer.enabled=true` by default) targets each x86 KVM-capable node
and lays down the stock Kata + Firecracker release bundled in its image,
provisions the containerd devmapper thin-pool with correct boot ordering,
and registers the `kata-fc` handler with containerd (stock containerd and
k3s are supported). The chart renders the `kata-fc` `RuntimeClass`, and
the runtime-agent labels capable nodes. No manual node preparation is
necessary.

If your cluster already installs Kata out of band — `kata-deploy`, a
baked node image, an administrator — the installer detects the existing
`kata-fc` registration and stands down without touching the node. Upstream
references for that path:

- Project home: <https://katacontainers.io/>
- `kata-deploy` (DaemonSet installer, also covers `kata-qemu`):
  <https://github.com/kata-containers/kata-containers/tree/main/tools/packaging/kata-deploy>

For `kata-qemu`, install Kata out of band (`kata-deploy` registers both
`kata-fc` and `kata-qemu`); the Setec installer covers `kata-fc` only. If
your environment uses non-default RuntimeClass names, set
`runtime.kata-fc.runtimeClassName` / `runtime.kata-qemu.runtimeClassName`
in `values.yaml` when installing the Setec chart.

## gvisor: no KVM required

gVisor is a user-space kernel written in Go. The Sentry process
intercepts every syscall from the guest and serves it entirely in
user space, reaching the host kernel only through a narrow filtered
subset gated by seccomp-bpf. This means gVisor runs on any Linux host
with a container runtime — no KVM, no nested virtualization, no special
CPU extensions.

Node requirement: the `runsc` binary installed + a Kubernetes
`RuntimeClass` named `gvisor` whose handler points at `runsc`. The
upstream project ships a DaemonSet installer:

```bash
kubectl apply -f https://raw.githubusercontent.com/google/gvisor/master/tools/images/install-runsc.yaml
kubectl apply -f - <<'EOF'
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata:
  name: gvisor
handler: runsc
EOF
```

- Upstream: <https://gvisor.dev/>
- Install docs: <https://gvisor.dev/docs/user_guide/install/>
- Security model: <https://gvisor.dev/docs/architecture_guide/security/>

## runc: dev only

`runc` is the default OCI container runtime shipped with every
Kubernetes distribution. It provides namespace + cgroup isolation only;
the guest shares the host kernel. Any container-escape bug is a direct
host compromise.

Setec surfaces `runc` only when Helm flag `runtime.runc.enabled=true`
AND `runtime.runc.devOnly=true` are both set at install time. Both the
flag and a validating webhook on `SandboxClass` block production use.

## runtime-agent: node capability detection

Setec ships a DaemonSet named `runtime-agent` that probes each Node for
each enabled backend's prerequisites and writes labels:

```
setec.zeroroot.ai/runtime.kata-fc=true
setec.zeroroot.ai/runtime.kata-qemu=true
setec.zeroroot.ai/runtime.gvisor=true
setec.zeroroot.ai/runtime.runc=true
```

Absent a backend's prerequisites the label reads `false`, and a backend the
agent no longer probes has its label removed. Only `true` counts as capable.
The scheduler uses these labels to pick the highest-isolation backend each
`Sandbox` can run on, per the `SandboxClass` fallback chain.

For the two Kata backends **and for gvisor**, host hardware (or the `runsc`
binary) is a necessary condition, not a sufficient one. The agent also
verifies that containerd on the node registers the matching CRI runtime
handler — a
`[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.kata-qemu]` table,
or `...runtimes.runsc` for gvisor. A node with KVM but no handler cannot run
a single `Sandbox`: every pod sandbox fails with `no runtime for
"kata-qemu" is configured`. Such a node reports `false`, so the operator
holds the `Sandbox` in `Pending` instead of scheduling it onto a node that
will reject it. All three labels carry the same guarantee; none of them is
best-effort.

### Where the agent looks for that registration

It starts at `/etc/containerd/config.toml` and the conventional drop-in
directories beside it, plus the k3s equivalents under
`/var/lib/rancher/k3s/agent/etc/containerd` — and then it **follows the
`imports` array** in every file it reads, because a containerd config is a
graph rather than a directory. Installers rely on that: kata-deploy 3.28+
writes `/opt/kata/containerd/config.d/kata-deploy.toml` and registers it by
appending to `imports`, so a node running Kata perfectly has a
`config.toml` that mentions only `runc`.

The agent reads those files through the read-only host mounts the chart
gives it:

| value | mounts | default |
|---|---|---|
| `runtimeAgent.mountContainerdConfig` | `/etc/containerd` | on |
| `runtimeAgent.mountK3sContainerdConfig` | `/var/lib/rancher/k3s/agent/etc/containerd` | off (k3s/RKE2 only) |
| `runtimeAgent.extraContainerdConfigDirs` | each listed path | `[/opt/kata/containerd]` |

With nothing mounted the handler cannot be verified and the labels stay
`false`. The three states are distinct, and the probe says which one it is:

- **configured** — a scanned file registers the handler. Label `true`.
- **absent** — the scan was complete and no file registers it. Label `false`.
- **unverifiable** — no config was readable at all, *or* a config named an
  `imports` path this agent cannot read. Label `false`, and the reason names
  the path to add to `extraContainerdConfigDirs`.

That third case is the one to check first when a node you believe is
Kata-capable reports `false`. The full probe outcome, including which config
files were scanned, is published on the Node as the
`setec.zeroroot.ai/runtime-probe` annotation:

```bash
kubectl get node <name> \
  -o jsonpath='{.metadata.annotations.setec\.zeroroot\.ai/runtime-probe}'
```

Check node capabilities:

```bash
kubectl get nodes \
  -L setec.zeroroot.ai/runtime.kata-fc \
  -L setec.zeroroot.ai/runtime.kata-qemu \
  -L setec.zeroroot.ai/runtime.gvisor \
  -L setec.zeroroot.ai/runtime.runc
```

Setec does not detect, depend on, or favor any cloud or vendor. Any
conformant Kubernetes distribution whose Nodes meet at least one
backend's prerequisites will work.

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
