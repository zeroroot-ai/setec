<!-- SPDX-License-Identifier: Apache-2.0 -->
# Threat model

This page states what `setec` protects, from whom, and where the boundary is for each backend. It describes `main` today.

## What is protected

- The node and the cluster from the code in a Sandbox.
- One tenant's Sandboxes and data from a different tenant.
- The `setec` API from a caller that is not enrolled.

The code in a Sandbox is untrusted. The image, the command and the network intent come from the caller and pass the class policy first ([isolation](isolation.md)).

## The boundary for each backend

| Backend | Boundary | An escape needs |
|---|---|---|
| `kata-fc` | A Firecracker microVM with its own kernel, on KVM. | A break of the guest kernel, then of Firecracker, then of KVM or the host kernel. |
| `kata-qemu` | A QEMU microVM with its own kernel. | A break of the guest kernel, then of QEMU, then of KVM or the host kernel. QEMU has a larger attack surface than Firecracker. |
| `gvisor` | The gVisor kernel in user space, under a seccomp filter. | A break of the gVisor kernel and of the filter to reach the host kernel. |
| `runc` | Linux namespaces and cgroups only. | One bug in the host kernel. Not a boundary for untrusted code. A dev cluster only. |

On every backend the Pod controls of the [isolation](isolation.md) page apply: no privilege, no capabilities but `NET_RAW` and `NET_ADMIN`, a read-only root, no ServiceAccount token, and a NetworkPolicy that denies by default.

## Pods that touch the host

Two Pods of the chart touch the host on purpose. Each one is a named exception in its template:

- The installer is privileged, because it writes host files and restarts containerd. It holds no Kubernetes credential (`charts/setec/templates/installer-daemonset.yaml`).
- The device plugin runs as root with two host paths, the kubelet plugin directory and `/dev`. It is not privileged and holds no capability and no credential (`charts/setec/templates/device-plugin-daemonset.yaml`).

## Callers

- Each `setec` gRPC hop uses mTLS with TLS 1.3 at least. No setting turns it off (`internal/credentials/credentials.go`).
- The credentials come from files or from the SPIFFE Workload API. In SPIFFE mode a caller must have an SPIFFE ID on the allow list (`internal/credentials/spiffe.go`).
- The operator dials the node agent with mTLS and checks its identity the same way.

## Restore and snapshots

A restored Sandbox could share state with the Sandbox it came from. Five checks must pass before `setec` serves it, and each snapshot artifact is encrypted at rest ([isolation](isolation.md), [storage](storage.md)).

## Out of scope

- The security of the node operating system, containerd and the kubelet.
- Encryption of the session workspace, which belongs to the StorageClass ([storage](storage.md)).
- A cluster administrator. `setec` does not defend against the people who run the cluster.
