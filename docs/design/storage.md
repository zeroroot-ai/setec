<!-- SPDX-License-Identifier: Apache-2.0 -->
# Storage

A Sandbox has three kinds of storage. Each one has its own rule for size and encryption.

## Scratch

The root filesystem of the workload is read-only. The one writable path is `/tmp`, an `emptyDir` volume that lives and dies with the Pod (`internal/podspec/builder.go`, `scratchVolumeName`).

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
