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

The admission webhook enforces coherence of the declarative trio:

- `preWarmPoolSize > 0` requires `preWarmImage` (the node-agent bakes
  pool entries from the class image).
- `preWarmTTL`, when set, must be a positive duration.
- An active pool requires the `kata-fc` backend — pool restore drives
  the Kata VM's Firecracker socket, which no other backend exposes.

### Warm-start flow

When an ephemeral Sandbox of a pool-declaring class (running exactly
the class `preWarmImage`, without an explicit `spec.snapshotRef`)
first transitions to `Running`, the operator makes a single warm-start
attempt: it dials the node-agent on the Pod's node, atomically claims
a matching pool entry (`ClaimPoolEntry` RPC), and restores the paused
VM state into the Pod's Firecracker socket. Restored guests receive
the same fail-closed entropy reseed as named-snapshot restores.

The outcome is recorded once in `status.warmStart`:

- `outcome: PoolRestored` with `entryID` — the Sandbox started from
  the pool, inside a real `kata-fc` Pod (CNI, NetworkPolicy, and
  observability all apply as usual).
- `outcome: ColdBoot` with `reason: miss` or `reason: error` — no
  compatible entry, an unreachable node-agent, or a failed restore.
  The Sandbox continues its normal cold boot; a warm-start failure
  never fails the Sandbox.

Events `WarmStartRestored` / `WarmStartColdBoot` narrate the attempt
on the Sandbox. A claimed entry is consumed even when its restore
fails — pool state is never restored twice (ADR-0005) — and the pool
reconciler reprovisions the missing entry on its next tick. Deleting
the SandboxClass (or setting `preWarmPoolSize: 0`) drains the pool;
no operator-managed template objects exist anywhere in the flow.

## Storage backend

Phase 3 ships one backend: local-disk. State files live under
`/var/lib/setec/snapshots/<namespace>-<snapshot>/state.bin` with
mode 0600 and a hex SHA256 sidecar at `state.bin.sha256`.

Every artifact is **encrypted at rest** — always, with no opt-out
(ADR-0005 invariant 5). Each snapshot gets its own AES-256-GCM data
key. The data key is sealed with a node-local key file
(`snapshots.keysDir`, default `/var/lib/setec/keys`) and stored
OUTSIDE the artifact tree, so a copy or backup of the snapshot
directory carries ciphertext only, with no key material. Pre-warm pool
entries get the same treatment: `setec-pool-vm` encrypts the entry's
state/memory pair in place and seals the per-entry key against the
entry's identity and provenance.

Delete destroys the sealed data key first — zero-overwrite, sync,
unlink — and then reclaims the ciphertext. The key destruction IS the
erasure: even on a copy-on-write filesystem where the ciphertext
overwrite is defeated, the artifact is cryptographically erased the
moment its only key is gone.

Snapshots written by a pre-encryption setec release have no sealed
key and are treated as destroyed: restore refuses them, delete still
reclaims them. Rebuild pools and re-create snapshots after upgrade;
there is no plaintext read path.

Future backends (object-store, content-addressable) slot in behind
the `storage.StorageBackend` interface without operator changes; the
encryption wrapper composes over any of them, and keys stay on the
node.

## Session memory checkpoints (S3-compatible backend)

Session Sandboxes (ADR-0006 L2) add a second, PORTABLE storage
composition: memory checkpoints on an **S3-compatible object store**
(real S3 on EKS, MinIO or any S3-compatible endpoint when
self-hosted — ADR-0007). Enable it on the node-agent via the chart:

```yaml
snapshots:
  s3:
    enabled: true
    endpoint: http://minio.minio.svc:9000   # empty = real S3
    bucket: setec-checkpoints
    pathStyle: true                          # required by MinIO
    credentialsSecret: setec-s3-creds        # AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY;
                                             # empty = IRSA / default chain
```

and per SandboxClass:

```yaml
spec:
  sessionCheckpoint:
    interval: 10m   # optional periodic cadence; omit for on-event only
    backend: s3
  sessionIdleTimeout: 15m
```

With `sessionCheckpoint` set, a session Sandbox gets:

- **Suspend-on-idle** — the `sessionIdleTimeout` deadline (setec#193's
  idle signal) checkpoints the VM and releases it (`phase: Suspended`,
  reason `SuspendedIdle`) instead of hard-failing it. A reattach (the
  frontend's `Attach` stamps the last-activity annotation) or an
  explicit `desiredState: Running` resumes it transparently — on
  whichever node the scheduler picks.
- **Explicit suspend** — `spec.desiredState: Suspended` (reason
  `UserSuspended`); resume with `desiredState: Running`.
- **Checkpoint-on-drain** — a cordoned node or an evicted VM Pod
  triggers an immediate checkpoint (reason `CheckpointOnDrain`) and
  the session resumes on another node with process state intact; the
  workspace PVC re-attaches alongside.
- **Periodic checkpoints** — `interval` bounds how much process replay
  a node death costs. Because the durable workspace already provides
  continuous DATA safety, checkpoints serve process continuity only
  and should stay infrequent.
- **Degraded recovery, surfaced** — a VM lost with no usable
  checkpoint restarts from the durable workspace and says so:
  `status.checkpoint.lastRecovery: RestartedFromWorkspace` plus a
  `SessionRestartedFromWorkspace` warning event. Data is never lost;
  only process state since the last checkpoint is.

A session keeps AT MOST one live checkpoint: a new one replaces (and
destroys) its predecessor, and a restore CONSUMES the checkpoint it
used — the same single-restore rule every snapshot obeys (ADR-0005).
`status.checkpoint` records the ref, sequence, timestamps, and the
last recovery outcome.

Key handling differs from node-local snapshots on purpose: see
SECURITY.md ("Two sealing domains"). The per-checkpoint data key is
sealed under a **cluster-scoped per-session KEK** held in a Kubernetes
Secret (`<sandbox>-session-kek`), created at session start and deleted
at session end — that deletion cryptographically erases every
checkpoint the session ever wrote, wherever the bucket is replicated.

## Operational considerations

- **Disk fill**: `snapshots.localDisk.fillThreshold` (default 0.85)
  refuses new snapshots when the filesystem exceeds the threshold.
  A Sandbox requesting a snapshot on a nearly-full node fails fast
  with `reason=InsufficientStorage` and is never paused. Snapshot
  artifacts are always encrypted at rest by Setec itself (see
  "Storage backend" above); whole-filesystem encryption remains a
  sensible additional layer for everything else on the node.
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

## Operator → node-agent credentials

The operator drives snapshots by dialling each node-agent over mTLS.
It runs in exactly one credential mode, selected the same way and with
the same failure semantics as every setec server surface — configuring
both or neither is a startup error naming the cause, and there is no
fallback between them.

**File mode (default).** `--nodeagent-tls-cert`, `--nodeagent-tls-key`
and `--nodeagent-ca`. The operator presents a client certificate and
accepts any node-agent whose certificate the configured CA issued and
whose name matches the dial target.

**SPIFFE mode.** `--nodeagent-spiffe-socket` plus one or more
`--nodeagent-spiffe-authorized-id` flags. The operator's identity comes
from the Workload API and rotates in-process, and — the part that
differs from a server surface — it authorizes the *node-agent's* SPIFFE
ID rather than checking a hostname. An X509-SVID carries no DNS name, so
the identity check replaces the hostname check rather than being added
alongside it; chaining to the trust bundle is not sufficient on its own.
An empty allow-list is a startup error.

The selected mode is logged at startup
(`Resolved node-agent client credentials mode=file`).

## Snapshot security

Snapshots are shared across warm-pool claims, so Setec enforces three
hardening invariants (ADR-0052; full detail in `SECURITY.md`):

- **No secrets in a Snapshot.** Pool entries are booted with no secret
  material, and a CI scan-gate (`no-secrets-in-snapshot`, backed by the
  `setec-snapshot-scan` CLI) fails the build if a snapshot artifact contains
  secret-shaped material. Inject per-lease secrets via the Sandbox Pod `env`
  POST-restore, never into the snapshotted VM.
- **Default-deny egress per SandboxClass.** Set
  `SandboxClass.spec.defaultNetworkMode: none` (or `egress-allow-list` with
  `spec.defaultEgressAllow`) so a Sandbox that declares no `spec.network`
  inherits a closed egress posture instead of unrestricted egress:

  ```yaml
  apiVersion: setec.zeroroot.ai/v1alpha1
  kind: SandboxClass
  metadata:
    name: hardened
  spec:
    vmm: firecracker
    defaultNetworkMode: egress-allow-list
    defaultEgressAllow:
      - {host: mirror.internal, port: 443}
  ```

- **Entropy reseed on restore** is enforced fail-closed by default
  (`snapshots.entropyReseed: require`): after every `LoadSnapshot` the
  node-agent pushes fresh entropy to the in-guest `setec-guest-agent`
  over vsock and refuses to report the restore successful until the
  guest acknowledges it with a digest-verified ack. Guest images must
  bundle `setec-guest-agent` (published as
  `ghcr.io/zeroroot-ai/setec-guest-agent`; also `make build-guest-agent`).
  `snapshots.entropyReseed: off` is the explicit opt-out for agent-less
  images — restored clones then rely on the passive virtio-rng
  mechanism only. See `SECURITY.md` ("Entropy reseed on restore").

## Metrics reference

Phase 3 adds these collectors to the existing Prometheus suite:

- `setec_snapshot_duration_seconds{operation,sandbox_class}` —
  histogram of snapshot operation durations. `operation` is one of
  `create`, `restore`, `delete`, `pause`, `resume`.
- `setec_prewarm_pool_entries{node,sandbox_class}` — gauge of
  currently-paused pool entries per node/class, exported by the
  node-agent after every pool reconcile tick.
- `setec_prewarm_pool_claims_total{outcome}` — node-agent counter of
  pool claim attempts; `outcome` is `restored`, `miss`, or
  `restore_failed`.
- `setec_warmstart_total{outcome,sandbox_class}` — operator counter of
  warm-start attempts; `outcome` is `restored`, `miss`, or `error`.
- `setec_node_entropy_reseed_total{outcome}` — counter of post-restore
  entropy reseed attempts on the node-agent, `outcome` is `success` or
  `failure`. A `failure` always corresponds to a restore that failed
  closed (the sandbox was never handed over).

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
