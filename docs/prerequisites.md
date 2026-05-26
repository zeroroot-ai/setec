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

Then install Kata Containers. This is out of Setec's scope; use the
upstream project.

- Project home: <https://katacontainers.io/>
- Installation docs: <https://github.com/kata-containers/kata-containers/blob/main/docs/install/README.md>
- `kata-deploy` (manifest-based installer):
  <https://github.com/kata-containers/kata-containers/tree/main/tools/packaging/kata-deploy>

The quickest path on a prepared cluster is `kata-deploy`, which ships as
a DaemonSet that lays down Kata binaries on every labeled Node and
registers the Kata `RuntimeClass` objects (`kata-fc` and `kata-qemu`). If
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

Absent a backend's prerequisites, the corresponding label is NOT written
(not set to `false`). The scheduler uses these labels to pick the
highest-isolation backend each `Sandbox` can run on, per the
`SandboxClass` fallback chain. Check node capabilities:

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
