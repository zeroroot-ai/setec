<!-- SPDX-License-Identifier: Apache-2.0 -->
# Architecture

This page describes how `setec` works on `main` today. Each statement names the path in this repository that proves it. When a design changes, the pull request that changes the code also changes the page.

## The parts

| Part | What it does | Code |
|---|---|---|
| Operator | Turns each `Sandbox` object into one Pod, a NetworkPolicy and, for a session, a workspace volume. Serves the admission webhooks. | `cmd/main.go`, `internal/controller/`, `internal/webhook/` |
| Frontend | The gRPC API (`SandboxService`, `LeaseService`). It creates and reads `Sandbox` objects for an enrolled client. | `cmd/frontend/`, `internal/frontend/`, `api/grpc/v1/` |
| Node agent | One per node. Takes, stores and restores snapshots, and holds the thin pool. | `cmd/node-agent/`, `internal/nodeagent/` |
| Runtime agent | One per node. Probes each backend and labels the node with the backends it can run. | `cmd/runtime-agent/`, `internal/runtimeagent/` |
| Installer | One per node, optional. Lays the kata-fc payload and registers it with containerd. | `cmd/installer/`, `internal/installer/` |
| Guest agent | Runs inside a restored microVM. Reseeds the random number generator and confirms the reseed. | `cmd/setec-guest-agent/`, `internal/entropy/` |
| Keepalive | The command of a session that names none. It also formats the workspace block device of a kata-fc session. | `cmd/setec-keepalive/` |

## The objects

- `Sandbox`: one workload. Image, command, resources, network intent and lifecycle. `api/v1alpha1/sandbox_types.go`.
- `SandboxClass`: the policy for a group of Sandboxes. Backend and fallback chain, resource ceilings, network defaults, session policy. `api/v1alpha1/sandboxclass_types.go`.
- `Snapshot`: a point-in-time capture of a Sandbox. `api/v1alpha1/snapshot_types.go`.

## The path of one Sandbox

1. A client calls `Launch`, or a user creates a `Sandbox` object. `internal/frontend/service.go`.
2. The admission webhook checks the Sandbox against its class. `internal/webhook/sandbox_webhook.go`, `internal/class/validator.go`.
3. The operator picks a backend from the class and the node labels. `internal/runtime/dispatcher.go`.
4. The operator writes the NetworkPolicy, then the Pod. `internal/netpol/generator.go`, `internal/podspec/builder.go`.
5. The kubelet starts the Pod with the RuntimeClass of the backend.
6. The operator records the phase, the exit code and the backend in the status. `internal/controller/sandbox_controller.go`.

## Design pages

- [Isolation](design/isolation.md): the Pod, the network and the restore checks.
- [Lifecycles](design/lifecycles.md): ephemeral and session Sandboxes, suspend, idle eviction, the warm pool and exec.
- [Storage](design/storage.md): the scratch volume, the session workspace and the snapshot store.
- [Runtime](design/runtime.md): the backends, their selection and the node preparation.
- [Threat model](design/threat-model.md): what each backend protects, and who may call `setec`.
