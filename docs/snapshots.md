# Snapshots, Restore, and Pause/Resume (Phase 3)

Setec captures the state of a running Firecracker machine as a
`Snapshot`, restores that state into a new Sandbox, forks one Sandbox
into several, and pauses and resumes a Sandbox. A SandboxClass can keep
a warm pool: snapshots of a machine that booted the image of the class,
which a new Sandbox loads instead of a boot. The launcher loads each
snapshot (docs/design/runtime.md).

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
- **Warm pool**: a SandboxClass may declare
  `spec.preWarmPoolSize=N` to keep N warm bases, each on another node.
  A base is a full snapshot of a machine that booted
  `spec.preWarmImage` and ran no workload.

## Enabling Phase 3

Set `snapshots.enabled=true` at install time:

```yaml
snapshots:
  enabled: true
  backend: local-disk
  localDisk:
    root: /var/lib/setec/snapshots
    fillThreshold: 0.85
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

With `certManager.enabled: true` the chart issues the whole mTLS
channel from one trust root. The `issuerRef` above is the bootstrap
issuer that signs a CA `Certificate` into `caSecret`. A namespaced
`Issuer` of kind `ca` reads that Secret and issues both leaves, so the
operator and the node-agent verify each other against one CA. The
workloads mount only the `ca.crt` key of `caSecret`. The CA private key
stays in the Secret.

With `certManager.enabled: false` you create all three Secrets out of
band from one CA and set `caProvided: true` to confirm the CA exists.
The chart refuses to render without that confirmation, because a
missing non-optional Secret wedges the pods with no useful error.

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
remain Paused — a paused microVM keeps its full memory reservation.
Past the cap the operator transitions the Sandbox to Failed with
`reason=PauseTimeoutExceeded` and deletes its VM Pod. Sessions in a
class with `sessionCheckpoint` enabled suspend instead of failing:
checkpoint retained, microVM released, `phase=Suspended` with
`reason=SuspendedPauseTimeout`, resumed when `spec.desiredState`
returns to `Running` (docs/design/lifecycles.md). Unset means pauses are unbounded;
the webhook rejects zero or negative values.

## Warm pool

Declare a pool on a SandboxClass:

```yaml
apiVersion: setec.zeroroot.ai/v1alpha1
kind: SandboxClass
metadata:
  name: fast
spec:
  runtime:
    backend: launcher
  defaultResources:
    vcpu: 2
    memory: 2Gi
  preWarmPoolSize: 3
  preWarmImage: ghcr.io/org/app@sha256:<digest>
  preWarmImageSignature:
    issuer: https://token.actions.githubusercontent.com
    identity: https://github.com/org/app/.github/workflows/release.yml@refs/tags/v1.2.3
```

A base is a full Snapshot of a launcher machine that booted the pool
image with the default resources of the class and ran no workload
(`internal/controller/warm_pool.go`). The operator keeps
`preWarmPoolSize` Ready bases, each on another node, in the namespace
of the pool. A pool keeps its bases for as long as Sandboxes ask for its
image, and it drops them after seven days with no such Sandbox.

The admission webhook refuses a class with `preWarmPoolSize > 0` and
no `preWarmImage` with a digest, no `defaultResources`, or no
`preWarmImageSignature`. A base belongs to one image digest, and it boots
with the default resources.

### The signature of the pool image

The operator builds no base from an image that it cannot check. Before
the first base, a Job of the disk builder checks the cosign signature of
`preWarmImage` (`internal/diskbuilder/imagesig`). cosign v3 attaches
the signature as a Sigstore bundle, an OCI referrer of the image.
`preWarmImageSignature` names the signer in one of two ways:

- `issuer` and `identity`: a keyless signature. The certificate of the
  signature must carry exactly this OIDC issuer and identity, for
  example the release workflow of the image owner. The check reads the
  Sigstore public-good trusted root through TUF, so the Job needs to
  reach `tuf-repo-cdn.sigstore.dev`.
- `publicKey`: a PEM public key, for an image signed with a key. The
  check needs no outside service, so it suits an air-gapped install.

An image with no signature of the named signer gets the condition
`ImageNotVerified=True` on the class, and the pool drops each base of
the class. The check runs again when its Job expires, and when the image
or the signer changes.

### Warm-start flow

A Sandbox can warm start when its class keeps a pool, it asks for the
pool image with the default resources of the class, and it names no
snapshot. Before the operator creates the Pod, it selects a Ready base
and records it in the `setec.zeroroot.ai/warm-base` annotation. The Pod
lands on the node of the base. The launcher loads the base instead of
a boot, gives the guest a new identity and fresh entropy, and then
starts the workload of the Sandbox. Restored guests get the same
fail-closed checks as a named-snapshot restore.

A warm start that finds no Ready base boots the Sandbox cold. A failed
load never fails the Sandbox. The `setec_warmstart_total{outcome}`
counter records `restored`, `miss` and `error`. The
`setec_warm_pool_ready_bases` and `setec_warm_pool_target_bases`
gauges show the fill level of each class. Setting `preWarmPoolSize: 0` or deleting the class drains the pool.

## Storage backend

Snapshots have two backends. The local disk of a node holds a
snapshot that loads on that node only. The S3-compatible store
(`snapshots.s3`) holds a kept snapshot and a session checkpoint, which
load on any node. On the local disk, state files live under
`/var/lib/setec/snapshots/<namespace>-<snapshot>/state.bin` with
mode 0600 and a hex SHA256 sidecar at `state.bin.sha256`.

Every artifact is **encrypted at rest** — always, with no opt-out
(docs/design/isolation.md invariant 5). Each snapshot gets its own AES-256-GCM data
key. The data key is sealed with a node-local key file
(`snapshots.keysDir`, default `/var/lib/setec/keys`) and stored
OUTSIDE the artifact tree, so a copy or backup of the snapshot
directory carries ciphertext only, with no key material. A base of the
warm pool is a Snapshot, so it gets the same treatment.

Delete destroys the sealed data key first — zero-overwrite, sync,
unlink — and then reclaims the ciphertext. The key destruction IS the
erasure: even on a copy-on-write filesystem where the ciphertext
overwrite is defeated, the artifact is cryptographically erased the
moment its only key is gone.

Snapshots written by a pre-encryption setec release have no sealed
key and are treated as destroyed: restore refuses them, delete still
reclaims them. Rebuild pools and re-create snapshots after upgrade;
there is no plaintext read path.

Both backends implement the `storage.StorageBackend` interface
(`internal/snapshot/storage/interface.go`), and the encryption wrapper
composes over each one.

## Session memory checkpoints (S3-compatible backend)

Session Sandboxes (docs/design/lifecycles.md L2) add a second, PORTABLE storage
composition: memory checkpoints on an **S3-compatible object store**
(real S3 on EKS, MinIO or any S3-compatible endpoint when
self-hosted — docs/design/storage.md). Enable it on the node-agent via the chart:

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

- **Suspend-on-idle** — the `sessionIdleTimeout` deadline (the idle
  signal) checkpoints the VM and releases it (`phase: Suspended`,
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
used — the same single-restore rule every snapshot obeys (docs/design/isolation.md).
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
hardening invariants (docs/design/threat-model.md; full detail in `SECURITY.md`):

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
  (`snapshots.entropyReseed: require`): after every snapshot load the
  launcher pushes fresh entropy to the guest agent over vsock and
  refuses to report the restore successful until the guest
  acknowledges it with a digest-verified ack. The guest agent is the
  `/init` of the initrd in the launcher image, so a Sandbox image needs
  no agent of its own. `snapshots.entropyReseed: off` is the explicit
  opt-out: restored clones then rely on the passive virtio-rng
  mechanism only. See `docs/design/isolation.md`.

## Metrics reference

Phase 3 adds these collectors to the existing Prometheus suite:

- `setec_snapshot_duration_seconds{operation,sandbox_class}` —
  histogram of snapshot operation durations. `operation` is one of
  `create`, `restore`, `delete`, `pause`, `resume`.
- `setec_warmstart_total{outcome,sandbox_class}` — operator counter of
  warm-start attempts; `outcome` is `restored`, `miss`, or `error`.
- `setec_warm_pool_ready_bases{sandbox_class}` and
  `setec_warm_pool_target_bases{sandbox_class}` — gauges of the Ready
  bases of each warm pool and of the number that the pool keeps.

The launcher reseeds and uniquifies each loaded guest itself, and it
writes the result as restore evidence that the invariant gate reads. A
failed step fails the restore closed, and the Sandbox Events name it.

## Troubleshooting

- **Sandbox stuck Pending with reason SnapshotUnavailable**: the
  referenced Snapshot is missing or not yet Ready. Run
  `kubectl get snapshot` to confirm.
- **Sandbox Failed with reason RestoreFailed**: the snapshot file
  failed its SHA256 integrity check, the kernel version no longer
  matches, or the node-agent could not speak to Firecracker. Check
  the Sandbox Events and the node-agent logs.
- **NodeAgentUnreachable**: the operator could not find or dial the
  node-agent Pod on the target node. The operator resolves that Pod
  through the API (label `app.kubernetes.io/component=node-agent`,
  field `spec.nodeName=<node>`), so confirm a node-agent Pod exists on
  the node and is Running and Ready, and that `--nodeagent-namespace`
  matches the namespace the DaemonSet runs in.

See [the runtime design](design/runtime.md) for how setec drives
Firecracker in a launcher Pod.
