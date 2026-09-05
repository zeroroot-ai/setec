# Kata + Firecracker Integration

Phase 3's snapshot and restore features operate directly against
Firecracker's REST API socket rather than routing through a Kata
Containers snapshot API. This document explains why, how the
integration works today, and the upstream path forward.

## Why direct Firecracker access

Firecracker natively supports snapshot and restore with a compact
state+memory format. Kata Containers exposes a Firecracker VMM but,
as of the versions available when Phase 3 was drafted, does NOT
propagate snapshot/restore semantics through its runtime API. The
Kata project has an open issue tracking the work, but no shipped
release exposes the capability.

Setec cannot wait for upstream Kata to catch up, so the node-agent
does the Firecracker work itself:

1. When the operator calls `CreateSnapshot`, the node-agent resolves
   the target Pod's Firecracker API socket (the path is predictable:
   Kata exposes it under `/run/kata-containers/<sandbox-id>/firecracker.socket`).
2. The node-agent speaks the documented Firecracker REST API —
   `PATCH /vm` for pause/resume and `PUT /snapshot/{create,load}` —
   directly over the Unix socket.
3. The resulting state and memory files are persisted via the
   pluggable `StorageBackend` interface (default: local-disk).

## Socket path resolution

The socket path is a Helm-value-configurable format string:

```yaml
snapshots:
  kataSocketPattern: "/run/kata-containers/%s/firecracker.socket"
```

`%s` is substituted with the Pod UID (which Kata uses as the
sandbox id). Custom Kata distributions may use a different layout;
override the pattern to match.

The node-agent container mounts `/run` from the host so the sockets
are reachable. The mount is scoped to `/run/kata-containers`
whenever the cluster operator layers on a mountPropagation restriction.

## Feature detection (planned)

When Kata ships a mature snapshot API, Setec will prefer it. The
planned detection logic:

1. At startup, the node-agent shells out to `kata-runtime version`.
2. If the returned version is greater than or equal to the minimum
   release that supports snapshots, a `UseKataSnapshotAPI=true`
   internal flag is set.
3. The Coordinator routes `CreateSnapshot` / `RestoreSandbox`
   through Kata's gRPC surface instead of the raw Firecracker
   socket.

Until that lands, the direct-socket path is the only option.

## Graceful disable when Kata is too old

If `kata-runtime` reports a version Setec knows has broken
Firecracker support (for example, a release where the socket path
layout changed but the snapshot code was not updated), the
node-agent:

1. Logs a warning at startup.
2. Annotates its Node with a condition `SetecSnapshotsUnavailable`.
3. Emits an Event on every Sandbox whose spec uses snapshot
   features.
4. The operator's webhook rejects Sandboxes that target such Nodes
   with `spec.snapshot.create=true` or `spec.snapshotRef`.

The cluster remains fully usable for non-snapshot workloads.

## Security posture

The node-agent container mounts:

- `/run/kata-containers/` (required for socket access) — read-write
  because Firecracker's UDS requires write access for the client.
- The configured `snapshots.localDisk.root` on the host — owned by
  the node-agent runtime user with mode 0700.

The node-agent drops all Linux capabilities except those required
for the thin-pool management it already does (`SYS_ADMIN`). It does
NOT need `NET_ADMIN`, `SYS_PTRACE`, or privileged-container mode
specifically for snapshot work. The privileged flag remains set for
the thin-pool path; operators who disable thin-pool management can
run the node-agent unprivileged.

## Upstream references

- Firecracker snapshot documentation:
  https://firecracker-microvm.github.io/doc/snapshotting/snapshot-support/
- Kata Containers runtime:
  https://katacontainers.io/
- Firecracker REST API reference (the OpenAPI spec Setec's client
  targets):
  https://firecracker-microvm.github.io/doc/api_requests/

When Kata's snapshot support matures, Setec will contribute the
feature-detection logic upstream rather than maintain it as a fork.
