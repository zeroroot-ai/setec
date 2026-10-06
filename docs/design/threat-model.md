<!-- SPDX-License-Identifier: Apache-2.0 -->
# Threat model

This page states what `setec` protects, from whom, and where the boundary is for each backend. It describes `main` today.

## What is protected

- The node and the cluster from the code in a Sandbox.
- One tenant's Sandboxes and data from a different tenant.
- The `setec` API from a caller that is not enrolled.

The code in a Sandbox is untrusted. The image, the command and the network intent come from the caller and pass the class policy first ([isolation](isolation.md)).

## The boundary

Each Sandbox is a Firecracker microVM with its own kernel, on KVM. An escape needs a break of the guest kernel, then of Firecracker, then of KVM or the host kernel. The launcher is the only runtime: the Kata, gVisor and runc backends were removed (setec#198).

The launcher Pod around the machine adds the Pod controls of the [isolation](isolation.md) page: no privilege, one capability (`NET_ADMIN`, to join the machine to the Pod network), a read-only root, no ServiceAccount token, nothing mounted from the host, and a NetworkPolicy that denies by default. The machine boots only a disk that the install key signed.

## Pods that touch the host

Two Pods of the chart touch the host on purpose. Each one is a named exception in its template:

- The node agent is privileged and runs as root, because it reads and writes the work volumes of the launcher Pods under the kubelet directory. It calls no Kubernetes API, so it holds no token and no RBAC (`charts/setec/templates/daemonset.yaml`).
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
