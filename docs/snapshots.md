# Snapshots, Restore, and Pause/Resume (Phase 3)

Phase 3 adds first-class Firecracker snapshot and restore to Setec,
exposed as Kubernetes-native primitives. Users capture a running
microVM's state, restore from that state into a new Sandbox, pause
and resume Sandboxes without tearing down VM state, and configure
per-SandboxClass pools of pre-warmed microVMs for sub-100ms cold
starts.

All Phase 3 features are opt-in via Helm values. A default install
renders Phase 2-equivalent manifests.

## Concepts

- **Snapshot**: a namespaced CRD representing saved microVM state
  (CPU registers, memory, metadata). Created by the operator when a
  Sandbox requests `snapshot.create=true`; consumed by later
  Sandboxes via `spec.snapshotRef`.
- **Pause / Resume**: `Sandbox.spec.desiredState` flips between
  `Running` and `Paused`. A paused microVM consumes near-zero CPU and
  retains memory until resumed.
- **Pre-warm pool**: a SandboxClass may declare
  `spec.preWarmPoolSize=N` to keep N paused microVMs per eligible
  node, ready for on-demand restore.

## Enabling Phase 3

Set `snapshots.enabled=true` at install time:

```yaml
snapshots:
  enabled: true
  backend: local-disk
  localDisk:
    root: /var/lib/setec/snapshots
    fillThreshold: 0.85
  kataSocketPattern: "/run/kata-containers/%s/firecracker.socket"
  mTLS:
    operatorCertSecret: setec-nodeagent-client-tls
    nodeAgentCertSecret: setec-nodeagent-server-tls
    caSecret: setec-nodeagent-ca
    certManager:
      enabled: true
      issuerRef:
        kind: ClusterIssuer
        name: selfsigned
```

The node-agent DaemonSet must also be enabled
(`nodeAgent.enabled=true`) because snapshot persistence happens on
the node where the VM lives.

## Creating a snapshot

Set `spec.snapshot.create=true` with a `spec.snapshot.name` on any
running Sandbox:

```yaml
apiVersion: setec.zeroroot.ai/v1alpha1
kind: Sandbox
metadata:
  name: workload-a
  namespace: tenant-alpha
spec:
  image: ghcr.io/org/app:1.2.3
  command: ["/app"]
  resources: {vcpu: 2, memory: 2Gi}
  snapshot:
    create: true
    name: workload-a-state
    afterCreate: Running
    ttl: 168h
```

`afterCreate` accepts `Running` (default), `Paused`, or
`Terminated`. A `Terminated` snapshot deletes the source Sandbox
after the state is persisted.

After creation the Snapshot appears under
`kubectl get snapshot -n tenant-alpha`:

```
NAME               PHASE   CLASS      SIZE       NODE     AGE
workload-a-state   Ready   standard   2147483648 node-a   30s
```

## Restoring from a snapshot

Launch a Sandbox with `spec.snapshotRef.name` pointing at the
Snapshot:

```yaml
apiVersion: setec.zeroroot.ai/v1alpha1
kind: Sandbox
metadata:
  name: workload-a-restored
  namespace: tenant-alpha
spec:
  sandboxClassName: standard
  snapshotRef:
    name: workload-a-state
  resources: {vcpu: 2, memory: 2Gi}
```

The operator pins the Pod to the snapshot's node and invokes
`NodeAgentService.RestoreSandbox` via gRPC. Cross-namespace
references are rejected at admission time.

## Pause and Resume

Flip `spec.desiredState`:

```bash
kubectl patch sandbox workload-a -p '{"spec":{"desiredState":"Paused"}}' --type=merge
# ... later
kubectl patch sandbox workload-a -p '{"spec":{"desiredState":"Running"}}' --type=merge
```

Pause does NOT persist state to disk. Evicting the Pod loses the
paused state; snapshot first if you need durability across
evictions.

`SandboxClass.spec.maxPauseDuration` bounds how long a Sandbox may
remain Paused before the operator transitions it to Failed with
`reason=PauseTimeoutExceeded`.

## Pre-warmed pool

Declare a pool on a SandboxClass:

```yaml
apiVersion: setec.zeroroot.ai/v1alpha1
kind: SandboxClass
metadata:
  name: fast
spec:
  vmm: firecracker
  runtimeClassName: kata-fc
  preWarmPoolSize: 8
  preWarmImage: ghcr.io/org/app:1.2.3
  preWarmTTL: 24h
```

The node-agent on each eligible node maintains 8 paused microVMs
running the pool image. Before booting a pool entry, the node-agent
prefetches the OCI image into the node's containerd content store via
the real containerd client (see `--containerd-socket` and
`--containerd-namespace` flags). Registry credentials can be supplied
via `--containerd-auth-file` pointing at a Docker config.json. A
pulled image already present in the store produces a cache hit and no
network traffic.

When a Sandbox with matching class and image lands, the operator
claims a pool entry and the cold-start latency drops to well under
100ms. Pool entries older than `preWarmTTL` are recycled automatically.
Pull failures are classified into typed sentinels and surfaced via the
`setec_node_image_prefetch_errors_total{reason}` counter (reasons:
`containerd_unreachable`, `image_not_found`, `auth_required`,
`pull_failed`) so operators can alert on non-transient misconfiguration.

Pool entries are invisible as Snapshot CRs — they are node-agent
internal state. The `setec_prewarm_pool_entries{node,sandbox_class}`
gauge exposes fill level per node.

## Storage backend

Phase 3 ships one backend: local-disk. State files live under
`/var/lib/setec/snapshots/<namespace>-<snapshot>/state.bin` with
mode 0600 and a hex SHA256 sidecar at `state.bin.sha256`. Delete
overwrites the state file with zeros before unlinking — a pragmatic
defense-in-depth measure, not cryptographic erasure.

Future backends (object-store, content-addressable) slot in behind
the `storage.StorageBackend` interface without operator changes.

## Operational considerations

- **Disk fill**: `snapshots.localDisk.fillThreshold` (default 0.85)
  refuses new snapshots when the filesystem exceeds the threshold.
  A Sandbox requesting a snapshot on a nearly-full node fails fast
  with `reason=InsufficientStorage` and is never paused. Filesystem
  encryption at rest is recommended but not enforced by Setec.
- **GC policy**: the SnapshotReconciler deletes Snapshots whose
  TTL has elapsed AND whose reference count is zero. A Snapshot
  with live Sandbox references is never deleted automatically.
- **Per-tenant quota**: set a `count/snapshots.setec.zeroroot.ai` counter on
  a namespace `ResourceQuota` to cap snapshots per tenant. The
  admission webhook enforces the quota at create time.
- **mTLS**: the operator-to-node-agent channel is always mTLS —
  mandatory, with no fallback. Both the operator and node-agent
  refuse to start without their TLS cert/key/client-ca triple, and
  the Helm chart always renders the corresponding Secret mounts.

## Metrics reference

Phase 3 adds two collectors to the existing Prometheus suite:

- `setec_snapshot_duration_seconds{operation,sandbox_class}` —
  histogram of snapshot operation durations. `operation` is one of
  `create`, `restore`, `delete`, `pause`, `resume`.
- `setec_prewarm_pool_entries{node,sandbox_class}` — gauge of
  currently-paused pool entries per node/class.

## Troubleshooting

- **Sandbox stuck Pending with reason SnapshotUnavailable**: the
  referenced Snapshot is missing or not yet Ready. Run
  `kubectl get snapshot` to confirm.
- **Sandbox Failed with reason RestoreFailed**: the snapshot file
  failed its SHA256 integrity check, the kernel version no longer
  matches, or the node-agent could not speak to Firecracker. Check
  the Sandbox Events and the node-agent logs.
- **NodeAgentUnreachable**: the operator could not dial the
  node-agent via its DNS endpoint. Verify the headless
  `<release>-node-agent` Service exists and the DaemonSet pods are
  Ready.

See the kata-firecracker integration doc for details on how Setec
drives the underlying VMM.
