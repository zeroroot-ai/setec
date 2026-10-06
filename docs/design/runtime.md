<!-- SPDX-License-Identifier: Apache-2.0 -->
# Runtime

`setec` runs each Sandbox on one of four backends. Each backend is a Kubernetes RuntimeClass. The [runtime backends](../runtime-backends/README.md) page compares them and gives the platform playbooks.

| Backend | Handler | What runs the workload |
|---|---|---|
| `kata-fc` | `kata-fc` | Kata Containers with a Firecracker microVM. The default. |
| `kata-qemu` | `kata-qemu` | Kata Containers with a QEMU microVM. |
| `gvisor` | `runsc` | gVisor, a kernel in user space. |
| `runc` | `runc` | A plain container. For a dev cluster only. |

The names are in `internal/runtime/config.go`. The RuntimeClasses come from `charts/setec/templates/runtime-classes.yaml`.

## Selection

- A `SandboxClass` names a backend and an ordered fallback chain (`api/v1alpha1/sandboxclass_types.go`).
- The runtime agent on each node probes each backend and labels the node `setec.zeroroot.ai/runtime.<backend>=true` (`internal/runtimeagent/labels.go`).
- The operator picks the first backend of the chain that a node offers, and records the choice in `status.runtime.chosen` (`internal/runtime/dispatcher.go`).
- `runc` is refused unless the cluster operator labels the gate namespace for dev use (`internal/webhook/sandboxclass_webhook.go`).

## The substrate

The substrate is x86 with KVM. Each published image is `linux/amd64`, and every Sandbox Pod has a node affinity for `kubernetes.io/arch=amd64` (`internal/runtime/affinity.go`). The two Kata backends need `/dev/kvm` on the node.

## Node preparation

- The optional installer DaemonSet lays the stock kata-fc release on a node and registers it and the devmapper snapshotter with containerd (`internal/installer/`, `charts/setec/templates/installer-daemonset.yaml`). `kata.env` pins the release.
- The installer lays no gVisor. A node that runs the `gvisor` backend gets `runsc` from its own preparation.
- A node whose kata is owned by something else (kata-deploy, a baked image) keeps it. The installer then supplies the snapshotter only.

## The launcher runtime, in progress

The launcher runtime replaces the four backends at one cutover. Its parts land on `main` one at a time, beside the backends of today:

- **The KVM device plugin.** A DaemonSet on each amd64 node offers `/dev/kvm` and `/dev/net/tun` as the resources `setec.zeroroot.ai/kvm` and `setec.zeroroot.ai/tun`. A node without `/dev/kvm` advertises them as unhealthy. `cmd/setec-device-plugin/`, `internal/deviceplugin/`, `charts/setec/templates/device-plugin-daemonset.yaml`.
- **The launcher Pod spec.** One `launcher` container asks for one share of each device. It is not privileged, mounts nothing from the host, and has exactly one capability, `NET_ADMIN`, to join the machine to the Pod network interface. Its memory is the memory of the machine plus 256Mi. The extra memory holds Firecracker, the launcher, and the page cache of a snapshot write, which the kernel charges to the Pod. `internal/podspec/launcher.go`. The operator does not create launcher Pods yet.

