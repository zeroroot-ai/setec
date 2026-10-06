<!-- SPDX-License-Identifier: Apache-2.0 -->
# Architecture

This page describes how `setec` works on `main` today. Each statement names the path in this repository that proves it. When a design changes, the pull request that changes the code also changes the page.

## The parts

| Part | What it does | Code |
|---|---|---|
| Operator | Turns each `Sandbox` object into one launcher Pod, a NetworkPolicy and, for a session, a workspace volume. Runs a disk builder Job for each new image digest. Serves the admission webhooks. | `cmd/main.go`, `internal/controller/`, `internal/webhook/` |
| Frontend | The gRPC API (`SandboxService`, `LeaseService`). It creates and reads `Sandbox` objects for an enrolled client. | `cmd/frontend/`, `internal/frontend/`, `api/grpc/v1/` |
| Node agent | One per node. Takes and stores the snapshots of the machines on its node, and stages a snapshot for a restore. | `cmd/node-agent/`, `internal/nodeagent/` |
| KVM device plugin | One per node. Offers `/dev/kvm` and `/dev/net/tun` to the launcher Pods. | `cmd/setec-device-plugin/`, `internal/deviceplugin/` |
| Disk builder | A Job for each image digest. Makes a squashfs disk of the image, signs it, and pushes it. | `cmd/setec-disk-builder/`, `internal/diskbuilder/` |
| Launcher | One per Sandbox, in the launcher Pod. Checks the disk signature, boots or loads the Firecracker machine, and signs the identity tokens of the Sandbox. | `cmd/setec-launcher/`, `internal/launcher/` |
| Guest agent | The init process of each machine. Runs the workload, serves exec, reseeds the random number generator and gives a loaded machine a new identity. | `cmd/setec-guest-agent/`, `internal/guestagent/` |

## The objects

- `Sandbox`: one workload. Image, command, resources, network intent and lifecycle. `api/v1alpha1/sandbox_types.go`.
- `SandboxClass`: the policy for a group of Sandboxes. Resource ceilings, network defaults, the warm pool and session policy. `api/v1alpha1/sandboxclass_types.go`.
- `Snapshot`: a point-in-time capture of a Sandbox. `api/v1alpha1/snapshot_types.go`.

## The path of one Sandbox

1. A client calls `Launch`, or a user creates a `Sandbox` object. `internal/frontend/service.go`.
2. The admission webhook checks the Sandbox against its class. `internal/webhook/sandbox_webhook.go`, `internal/class/validator.go`.
3. The operator refuses a class that names a removed backend. `internal/runtime/backend.go`.
4. The operator makes sure a signed disk of the image digest exists, and runs the disk builder when it does not. `internal/controller/`, `internal/diskbuilder/`.
5. The operator writes the NetworkPolicy, then the launcher Pod. `internal/netpol/generator.go`, `internal/podspec/launcher.go`.
6. The scheduler places the Pod on a node that offers the KVM device, and the launcher boots the machine.
7. The operator records the phase and the exit code in the status. `internal/controller/sandbox_controller.go`.

## Design pages

- [Isolation](design/isolation.md): the Pod, the network and the restore checks.
- [Lifecycles](design/lifecycles.md): ephemeral and session Sandboxes, suspend, idle eviction, the warm pool and exec.
- [Storage](design/storage.md): the scratch volume, the session workspace and the snapshot store.
- [Runtime](design/runtime.md): the launcher runtime, its parts and the node preparation.
- [Threat model](design/threat-model.md): what the machine boundary protects, and who may call `setec`.
