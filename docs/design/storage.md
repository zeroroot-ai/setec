<!-- SPDX-License-Identifier: Apache-2.0 -->
# Storage

A Sandbox has three kinds of storage. Each one has its own rule for size and encryption.

## Scratch

The image disk of the workload is read-only. The guest writes to a writable layer, an ext4 disk that the launcher makes in the `emptyDir` work volume of the Pod. The guest agent mounts an overlay of the two as the root (`internal/launcher/disks.go`, `internal/guestagent/root_linux.go`). `/tmp`, `/run` and `/dev/shm` are tmpfs in guest memory. The scratch lives and dies with the Pod.

- The size of the writable layer is the scratch size: `spec.resources.scratch`, else the `defaultResources.scratch` of the class, else 10 GiB (`internal/limits/limits.go`, `EffectiveScratch`).
- A class caps the scratch size with `maxResources.scratch`. With no cap, the ceiling is 10 GiB, and the webhook refuses a Sandbox above it (`internal/class/validator.go`).
- The work volume is the scratch size plus 2 GiB for the machine files. The `ephemeral-storage` limit of the Pod is the work volume plus 1 GiB for logs, so a full writable layer cannot fill the disk of the node (`internal/podspec/launcher.go`, `BuildLauncher`).

## The session workspace

A session Sandbox gets a ReadWriteOnce PersistentVolumeClaim at `/workspace`. The Sandbox owns it, so it outlives a Pod restart and leaves with the Sandbox (`internal/controller/sandbox_controller.go`, `newWorkspacePVC`).

- `spec.lifecycle.workspace.size` sets the size. The default is 10 GiB.
- `spec.lifecycle.workspace.storageClassName` selects the StorageClass. Unset takes the cluster default.
- The workspace reaches the microVM as a raw block device. The launcher formats a new workspace as ext4, and the guest agent mounts it at `/workspace` (`internal/launcher/disks.go`, `internal/guestagent/root_linux.go`).

**Encryption.** `setec` adds no encryption to the workspace volume. Encryption at rest belongs to the StorageClass. Select a StorageClass whose CSI driver encrypts its volumes, for example an encrypted EBS, Ceph or LUKS class. `setec` does not check this setting (`api/v1alpha1/sandbox_types.go`, `WorkspaceSpec.StorageClassName`).

## Snapshots

The node agent writes snapshot artifacts through one interface, `StorageBackend` (`internal/snapshot/storage/interface.go`). Two backends exist: the local disk of the node (`internal/snapshot/storage/local_disk.go`) and an S3-compatible store (`internal/snapshot/storage/s3.go`).

**Encryption.** `setec` always encrypts snapshot artifacts, which is a different rule than for the workspace. Each artifact has its own data key under AES-256-GCM. A node key seals each data key, and the seal binds the key to the artifact. Destroying an artifact destroys its key first (`internal/snapshot/atrest/atrest.go`, `internal/snapshot/storage/encrypted.go`).

See [snapshots](../snapshots.md) and [session checkpoints](../session-checkpoints.md) for the user view.
