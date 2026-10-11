<!-- SPDX-License-Identifier: Apache-2.0 -->
# Runtime

`setec` runs each Sandbox as one Firecracker machine in one launcher Pod. The launcher is the only runtime. The Kata, gVisor and runc backends, the node installer and the runtime agent were removed (setec#198).

## The parts

- **The KVM device plugin.** A DaemonSet on each amd64 node offers `/dev/kvm` and `/dev/net/tun` as the resources `setec.zeroroot.ai/kvm` and `setec.zeroroot.ai/tun`. A node without `/dev/kvm` advertises them as unhealthy and gets no launcher Pod. `cmd/setec-device-plugin/`, `internal/deviceplugin/`, `charts/setec/templates/device-plugin-daemonset.yaml`.
- **The disk builder.** Three steps turn an image digest into a signed squashfs disk in `launcher.diskRepo`, and only the operator holds the ed25519 seed of `launcher.diskBuilder.signingSecret`. A build Job pulls the image from a registry of `launcher.imageSources`, makes the disk, pushes the disk layer, and reports the digest of the layer and the SHA-256 of the disk in its termination message. The operator checks the report and signs the image digest and the disk SHA-256. A publish Job pushes the signature layer and the manifest under the tag. No Job mounts the seed. The build Job reaches the image registries and the disk registry by name, the publish Job reaches only the disk registry, and the operator has no registry egress (`charts/setec/templates/system-namespace-policy.yaml`). The tag of the disk is the digest hex and the key id of the seed (`<digest hex>-<key id>`), so one pair of Jobs runs for each digest and signing key. A rotated seed builds and signs a new disk beside the old one. The operator refuses a seed whose public key is not in `launcher.diskBuilder.publicKeys`, and an image of a registry that `launcher.imageSources` does not name. `cmd/setec-disk-builder/`, `internal/diskbuilder/`, `internal/controller/launcher_disk.go`.
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

## CPU templates

A snapshot holds the CPU state of its machine, so it loads only where the guest sees the same CPU features. A class fixes them with a Firecracker custom CPU template: `spec.cpuTemplate` names a file of `/opt/setec/cpu-templates/` in the launcher image, without `.json` (`internal/podspec/launcher.go`, `LauncherCPUTemplateDir`). The launcher passes it to Firecracker as the CPU configuration of the machine (`internal/launcher/vmconfig.go`). A class with a template also names the instance type of its nodes in `spec.nodeSelector`, because a template matches one CPU.

A snapshot also holds the page cache and the file system state of its image disk, so it loads only over a disk with the same bytes. The launcher writes the SHA-256 of the disk that it checked into its work directory. The node agent stores that digest with each snapshot, and stages it for a restore. The launcher compares it with the disk of the new Pod before the load, and refuses another disk. A rotation of the disk signing key builds a new disk, and the check covers it too (`internal/launcher/run.go`, `checkSnapshotDisk`).

| Template | Instance types | Host kernel |
|---|---|---|
| `m8i` | m8i: Intel Xeon 6 (Granite Rapids) | 5.17 or later |

Firecracker has no static template for Granite Rapids. `m8i` is the custom template that Firecracker tests on that CPU, from the release that `firecracker.env` pins: it turns off the features that the T2 template turns off, so each m8i node shows one feature set. The file, its source and its license are in `cpu-templates/`. `TestCPUTemplates_AreValid` checks the format of each template, and the e2e scenario `TestLauncher_CPUTemplateSnapshotLoads` loads a snapshot in a class with the template on m8i nodes. The CPU of a GitHub-hosted runner is never m8i, so the scenario runs each day against the setec installed on the m8i nodes of staging (`make e2e-cpu-template`, which installs nothing).

## Node preparation

A node needs `/dev/kvm` and `/dev/net/tun`, and the kernel modules `tun`, `sch_ingress`, `cls_matchall` and `act_mirred` for the tap device and the traffic redirects of the launcher. Nothing else is installed on the node: Firecracker, the guest kernel and the guest agent come in the launcher image. `firecracker.env` and `kernel/kernel.env` pin their releases.
