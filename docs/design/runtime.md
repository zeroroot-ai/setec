<!-- SPDX-License-Identifier: Apache-2.0 -->
# Runtime

`setec` runs each Sandbox as one Firecracker machine in one launcher Pod. The launcher is the only runtime. The Kata, gVisor and runc backends, the node installer and the runtime agent were removed (setec#198).

## The parts

- **The KVM device plugin.** A DaemonSet on each amd64 node offers `/dev/kvm` and `/dev/net/tun` as the resources `setec.zeroroot.ai/kvm` and `setec.zeroroot.ai/tun`. A node without `/dev/kvm` advertises them as unhealthy and gets no launcher Pod. `cmd/setec-device-plugin/`, `internal/deviceplugin/`, `charts/setec/templates/device-plugin-daemonset.yaml`.
- **The disk builder.** A Job in the operator namespace turns an image digest into a squashfs disk, signs it with the ed25519 seed of `launcher.diskBuilder.signingSecret`, and pushes it to `launcher.diskRepo`. One Job runs for each digest. `cmd/setec-disk-builder/`, `internal/diskbuilder/`.
- **The launcher Pod.** One `launcher` container asks for one share of each device. It mounts the signed disk as an image volume, checks the signature with `launcher.diskBuilder.publicKeys`, and boots Firecracker with the guest kernel and the guest agent of the launcher image. It is not privileged, mounts nothing from the host, and has exactly one capability, `NET_ADMIN`, to join the machine to the Pod network interface. Its memory is the memory of the machine plus 256Mi. The extra memory holds Firecracker, the launcher, and the page cache of a snapshot write, which the kernel charges to the Pod. The `requests` of the class lower the CPU and memory requests of the Pod below these limits. `cmd/setec-launcher/`, `internal/launcher/`, `internal/podspec/launcher.go`.
- **The guest agent.** The init process of the machine. It runs the workload, serves exec over vsock, reseeds the entropy pool and gives the machine a new identity after a snapshot load. `cmd/setec-guest-agent/`, `internal/guestagent/`.
- **The node agent.** A DaemonSet that takes the snapshots of the machines on its node and stages a snapshot in the work volume of a launcher Pod for a restore. It calls no Kubernetes API. `cmd/node-agent/`, `internal/nodeagent/`.

## Selection

- `SandboxClass.spec.runtime.backend` is `launcher` or empty (`api/v1alpha1/sandboxclass_types.go`).
- The admission webhook refuses a class that names a removed backend (`kata-fc`, `kata-qemu`, `gvisor`, `runc`), with a message that names setec#198 (`internal/runtime/backend.go`, `internal/webhook/sandboxclass_webhook.go`).
- A class that skipped admission still cannot run a Sandbox: the operator fails the Sandbox with the reason `UnsupportedBackend` (`internal/controller/sandbox_controller.go`).
- The operator records `launcher` in `status.runtime.chosen`.

## The substrate

The substrate is x86 with KVM. Each published image is `linux/amd64`, and every launcher Pod has a node affinity for `kubernetes.io/arch=amd64`. Each node that runs Sandboxes needs `/dev/kvm`, either bare metal or a VM with nested virtualization. Kubernetes is 1.35 or later, because the launcher Pod mounts its disk as an image volume.

## Node preparation

A node needs `/dev/kvm` and `/dev/net/tun`, and the kernel modules `tun`, `sch_ingress`, `cls_matchall` and `act_mirred` for the tap device and the traffic redirects of the launcher. Nothing else is installed on the node: Firecracker, the guest kernel and the guest agent come in the launcher image. `firecracker.env` and `kernel/kernel.env` pin their releases.
