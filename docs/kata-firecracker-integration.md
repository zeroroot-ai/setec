# Kata + Firecracker Integration

Phase 3's snapshot and restore features operate directly against
Firecracker's REST API socket rather than routing through a Kata
Containers snapshot API. This document explains why, how the
integration works today, and how Setec finds the nodes that can run it.

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

## How Setec detects Kata with Firecracker

Setec does not read a Kata version. No Setec component runs
`kata-runtime`, and the installer payload does not ship that binary.
The runtime-agent DaemonSet does the detection. It probes each node at
startup and again on an interval (default: five minutes). Each probe
only reads files. It does not run a subprocess.

The `kata-fc` probe (`internal/runtimeagent/probe/kata_fc.go`) reports
the backend as available only when all three of these conditions are
true:

1. The device node `/dev/kvm` exists.
2. A KVM kernel module is loaded. The probe looks for
   `/sys/module/kvm_intel`, `/sys/module/kvm_amd`, or the built-in
   `/sys/module/kvm` entry on arm64 hosts.
3. The containerd configuration registers a CRI runtime handler named
   `kata-fc`.

For the third condition, the probe reads the containerd configuration
files and follows each `imports` entry. It does not look for the Kata
shim binary. containerd finds the shim from the `runtime_type` of the
handler. If the agent cannot read any containerd configuration, or
cannot read a file that an `imports` entry names, the probe reports
the handler as `unverifiable` instead of `absent`. In both cases the
backend is unavailable.

The runtime-agent writes the result to its own Node:

- The label `setec.zeroroot.ai/runtime.kata-fc` gets the value `"true"`
  or `"false"`.
- The annotation `setec.zeroroot.ai/runtime-probe` holds the probe
  result as JSON. For `kata-fc`, it gives the reason for a failure and
  the details `kvm_module` and `containerd_handler`.

The operator gives each `kata-fc` Sandbox Pod a required node affinity
on `setec.zeroroot.ai/runtime.kata-fc=true`. Thus a `kata-fc` Sandbox
runs only on a node that passed all three checks.

## What detection does not check

The probe does not check the Kata version, the Firecracker version, or
the layout of the Firecracker socket path. There is no separate
snapshot capability check. The node-agent always uses the direct
Firecracker socket path for snapshots. Setec does not set a Node
condition, emit an Event, or reject a Sandbox in the webhook because of
the Kata version.

If your Kata distribution puts the Firecracker socket in a different
location, set `snapshots.kataSocketPattern` to match it (see
[Socket path resolution](#socket-path-resolution)).

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
  https://github.com/firecracker-microvm/firecracker/blob/main/docs/snapshotting/snapshot-support.md
- Kata Containers runtime:
  https://katacontainers.io/
- Firecracker REST API reference (the OpenAPI spec Setec's client
  targets):
  https://github.com/firecracker-microvm/firecracker/blob/main/src/firecracker/swagger/firecracker.yaml
