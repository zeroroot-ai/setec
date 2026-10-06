# Sandbox CRD Reference

> **Status (2026-09-29): not available until after launch.** Snapshot
> restore (`spec.snapshotRef`), session memory checkpoints
> (`spec.sessionCheckpoint`) and the pre-warm pool do not work yet.
> Snapshot creation, pause/resume and TTL expiry work. The API still
> accepts the fields: a Sandbox that names a `snapshotRef` boots fresh
> instead of restoring, and a checkpointed session cannot resume. The
> reasons and the open design are in
> [setec#105](https://github.com/zeroroot-ai/setec/issues/105) (restore
> and checkpoints) and
> [setec#103](https://github.com/zeroroot-ai/setec/issues/103) (pool).

`Sandbox` is the sole custom resource Setec defines. This document is the
authoritative field reference. It is derived from the generated
`config/crd/bases/setec.zeroroot.ai_sandboxes.yaml` and the Go types in
`api/v1alpha1/sandbox_types.go`.

- **Group / version / kind:** `setec.zeroroot.ai/v1alpha1` / `Sandbox`
- **Scope:** Namespaced
- **Short name:** `sbx`
- **Printer columns:** `Phase`, `Image`, `Age`, `Exit-Code` (wide view)

## Example

```yaml
apiVersion: setec.zeroroot.ai/v1alpha1
kind: Sandbox
metadata:
  name: example
  namespace: default
spec:
  image: docker.io/library/python:3.12-slim
  command:
    - python
    - -c
    - "print('hi')"
  env:
    - name: FOO
      value: bar
  resources:
    vcpu: 2
    memory: 2Gi
  network:
    mode: egress-allow-list
    allow:
      - host: example.com
        port: 443
  lifecycle:
    timeout: 30m
status:
  phase: Running
  podName: example-vm
  startedAt: "2026-04-15T12:00:05Z"
  lastTransitionTime: "2026-04-15T12:00:05Z"
```

## `spec` fields

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `image` | string (`minLength: 1`) | yes | — | OCI image reference the microVM will run; the kubelet pulls it with its default policy. |
| `command` | []string (`minItems: 1`) | yes | — | Entrypoint executed inside the microVM; arguments are passed verbatim with no shell interpretation. |
| `env` | []corev1.EnvVar | no | `[]` | Environment variables exposed to the workload, following the standard Kubernetes `EnvVar` schema. |
| `resources` | object | yes | — | CPU and memory budget for the microVM; see [`spec.resources`](#specresources) below. |
| `resources.vcpu` | int32 (`1`–`32`) | yes | — | Number of virtual CPUs allocated to the microVM. |
| `resources.memory` | resource.Quantity | yes | — | RAM allocated to the microVM (e.g. `512Mi`, `2Gi`). The API refuses more than `64Gi`. A class can lower the ceiling with `maxResources.memory`. |
| `resources.scratch` | resource.Quantity | no | class `defaultResources.scratch`, else `10Gi` | Size limit of the scratch volume at `/tmp`. The Pod gets an ephemeral-storage limit of this value plus `1Gi`. The value must not exceed the class `maxResources.scratch`, else `10Gi`. The kubelet stops a Sandbox that writes past the limit. |
| `network` | object | no | class default, else `{mode: none}` | Egress policy for the microVM; see [`spec.network`](#specnetwork) below. |
| `network.mode` | enum `external-only` \| `egress-allow-list` \| `none` | yes (when `network` set) | `none` | Egress posture. Every mode is enforced by a generated NetworkPolicy. |
| `network.allow` | []object | no | `[]` | Permitted egress destinations. Meaningful only when `network.mode: egress-allow-list`. |
| `network.allow[].host` | string (`minLength: 1`) | yes | — | DNS name or IP address permitted as an egress target. Resolved to `ipBlock` peers on the generated rule and re-resolved periodically; also recorded on a `setec.zeroroot.ai/allow-<port>` annotation (`allow-<port>-<endPort>` for a range, with a `udp-` prefix for UDP). A name that does not resolve is **dropped** from the policy (recorded on `setec.zeroroot.ai/unresolved-allow`), never widened to `0.0.0.0/0`. |
| `network.allow[].port` | int32 (`1`–`65535`) | one of `port`, `ports` | — | The one destination TCP port permitted for this host. An entry sets exactly one of `port` and `ports`. |
| `network.allow[].ports` | []object (max 32) | one of `port`, `ports` | — | Destination ports and port ranges permitted for this host. Use it for a range, for more than one port, or for UDP. |
| `network.allow[].ports[].protocol` | enum `TCP` \| `UDP` | no | `TCP` | Transport protocol of the entry. |
| `network.allow[].ports[].port` | int32 (`1`–`65535`) | yes | — | Destination port, or the first port of the range when `endPort` is set. |
| `network.allow[].ports[].endPort` | int32 (`1`–`65535`) | no | — | Last port of the range. It must not be lower than `port`. `port: 1` with `endPort: 65535` permits every port of the protocol. |
| `network.allow[].cidr` | string | no | — | Address block this entry is pinned to, replacing resolution of `host` for that rule. Set it when the destination range is genuinely known, or when the name does not resolve from inside the cluster. |
| `lifecycle` | object | no | `{}` | Lifecycle selection and runtime constraints applied to the Sandbox. |
| `lifecycle.mode` | enum `ephemeral` \| `session` | no | `ephemeral` | Which lifecycle the Sandbox follows (docs/design/lifecycles.md). `ephemeral` is today's run-to-completion behavior, unchanged. `session` is long-lived with a durable `/workspace` PVC and explicit teardown. **Immutable** — the admission webhook rejects any update that changes the effective mode. See [`spec.lifecycle.mode`](#speclifecyclemode). |
| `lifecycle.workspace` | object | no (session only) | `{}` | Durable per-session workspace volume configuration. Rejected at admission unless `lifecycle.mode: session`. |
| `lifecycle.workspace.size` | resource.Quantity | no | `10Gi` | Requested capacity of the workspace PVC. Must be > 0. |
| `lifecycle.workspace.storageClassName` | string | no | cluster default | StorageClass the workspace PVC is provisioned from. Any CSI driver works. **Encryption at rest is this StorageClass's responsibility** — point it at a class whose driver encrypts volumes; Setec adds no encryption layer of its own. |
| `lifecycle.timeout` | Go duration string (`metav1.Duration`) | no | unset (unbounded) | Maximum wall-clock runtime. When exceeded, the controller terminates the Pod and marks the Sandbox `Failed` with reason `Timeout`. For sessions the timeout spans the whole session, measured from the first VM start. Examples: `30m`, `8h`. |

### `spec.resources`

Both `vcpu` and `memory` are required. The operator translates these into
the size of the Firecracker microVM. The launcher Pod asks for the same CPU,
and for the memory plus 256Mi for Firecracker, the launcher and the page
cache of a snapshot write.

### `spec.network`

Every Sandbox gets a NetworkPolicy. There is no mode, and no combination
of an omitted `spec.network` with an absent SandboxClass default, that
leaves a Sandbox unpoliced: an unstated posture resolves to `none`. The
operator writes the policy **before** it creates the Pod, and if the
policy cannot be applied the Pod is not created at all.

All three modes deny ingress — nothing dials into a Sandbox.

| Mode | Egress |
|------|--------|
| `external-only` | `0.0.0.0/0` on **all ports**, with the operator's reserved ranges subtracted via `ipBlock.except`. Public destinations stay reachable on arbitrary ports; the cluster's own address space does not. |
| `egress-allow-list` | One TCP rule per `allow` entry, scoped to that entry's port, naming that entry's `cidr` or the addresses its `host` resolves to, with the same reserved-range subtraction. An entry whose host cannot be resolved contributes no rule. |
| `none` | Nothing. Empty ingress and egress rule lists with both policy types listed. |

`external-only` and `egress-allow-list` additionally permit UDP and TCP 53
to exactly the resolvers the operator was started with (`--sandbox-resolvers`).
The Pod itself is configured with `dnsPolicy: None` and those same
addresses, so a Sandbox resolves names through them rather than through
cluster DNS and cannot enumerate in-cluster Services by name.

The reserved ranges come from the operator's `--reserved-cidrs` flag. The
default covers private (RFC1918), link-local — which includes the cloud
instance-metadata address — carrier-grade NAT, loopback and multicast
space. A cluster operator adds their Service and Pod CIDRs.

Two consequences worth stating plainly:

- **Self-hosted installs must retune the reserved list.** If the
  authorized scope for your workloads *is* private address space, the
  default reserved list denies exactly what you meant to allow. Narrow
  `--reserved-cidrs` to your own control-plane ranges rather than
  clearing it; an empty list is rejected at startup.
- **Reserved ranges are enforced, not advisory.** An `allow` entry whose
  `cidr` falls entirely inside a reserved range is dropped rather than
  rendered, and the dropped entry is recorded on the
  `setec.zeroroot.ai/suppressed-allow` annotation. A SandboxClass may
  deliberately re-open a range for its own Sandboxes via
  `spec.egressExemptCIDRs`.

Rules are IPv4. Traffic that matches no rule is denied, so on a
dual-stack cluster IPv6 egress is denied outright.

Sandbox deletion garbage-collects the NetworkPolicy via its
OwnerReference.

Enforcement is the CNI's job. A NetworkPolicy on a cluster whose CNI does
not implement `networking.k8s.io/v1` is inert, and nothing in this
operator can detect that. Verify enforcement on the cluster itself.

### `spec.lifecycle.mode`

A Sandbox declares one of two lifecycles (docs/design/lifecycles.md). The mode is
immutable for the life of the object; to change it, delete the Sandbox
and create a new one.

**`ephemeral` (default).** Run-to-completion: one workload, auto-destroy
on exit, stateless. A Sandbox with no `lifecycle` block, or with
`mode: ephemeral`, behaves exactly as before the mode existed.

The auto-destroy is a finished-TTL: after the Sandbox reaches
`Completed` or `Failed` the controller keeps it for a bounded window
(10 minutes by default) so its creator can still `Wait` for the
outcome and `StreamLogs` the captured output, then deletes it. The
window opens only on a terminal phase. A live Sandbox, in `Running`
or any other non-terminal phase, is never deleted by this clock no
matter how long it has run. Only `spec.lifecycle.timeout` bounds a
running ephemeral Sandbox.

**`session`.** Long-lived, ended only by explicit teardown (deleting the
Sandbox, or `Kill` on the gRPC frontend):

- **Durable workspace.** The operator creates a dedicated `ReadWriteOnce`
  CSI PVC named `<sandbox>-workspace` *before* the Pod and mounts it at
  `/workspace`. Data written there survives VM restart and node loss —
  on node failure the CSI driver re-attaches the claim to the failover
  node (docs/design/storage.md). Any CSI driver works; there is no cloud-specific
  storage dependency. The claim is `volumeMode: Block`: Firecracker has
  no virtio-fs, and a raw block device is the one volume type it can
  attach to the guest. The launcher formats a new workspace as ext4
  (once, never reformatting an existing file system), and the guest agent
  mounts it, so the workload sees an ordinary writable directory at
  `/workspace` (docs/design/storage.md, setec#91, setec#193).
- **VM restart, not completion.** The workload exiting (any exit code)
  does not finish a session. The controller deletes the dead Pod and
  recreates it; the fresh microVM re-mounts the workspace and continues.
  Status transiently shows `Pending` with reason `SessionVMRestarting`.
  `lifecycle.timeout` still fails the Sandbox terminally, keeping the
  restart loop bounded.
- **Reattach by handle.** The session's handle (the frontend's
  `sandbox_id`, `<namespace>/<name>/<uid>`) is the whole reattach
  credential: the frontend `Attach` RPC resolves it from cluster state
  alone, so a caller reattaches to the same live session after a
  disconnect or a frontend restart. Ephemeral Sandboxes reject
  `Attach`. See `docs/frontend-api.md`.
- **Idle eviction, active exemption.** When the Sandbox's class sets
  `spec.sessionIdleTimeout`, a `Running` session with no recorded
  activity for that long is evicted (`Failed`, `reason=IdleTimeout`;
  Pod deleted; workspace kept until teardown). Activity is the
  `setec.zeroroot.ai/last-activity` annotation, stamped by the frontend
  on `Attach` and heartbeaten while a client stream is open — an active
  session is never idle-reaped.
- **Teardown wipes the workspace.** Deleting the Sandbox triggers the
  `setec.zeroroot.ai/workspace-teardown` finalizer: the Pod is deleted,
  then the workspace PVC is deleted; the CSI driver destroys the volume
  and every byte of session data with it. One session per VM and per
  workspace — nothing is reusable across sessions (docs/design/isolation.md
  invariant 3). Pair `storageClassName` with an encrypting StorageClass
  so at-rest deletion is also a cryptographic erase.

### `spec.lifecycle.timeout`

Accepts any duration string recognized by `metav1.Duration`
(e.g., `30s`, `10m`, `8h`). Invalid strings are rejected at admission.
When `timeout` elapses while the Sandbox is `Running`, the controller
deletes the backing Pod; status converges to `Failed` with
`reason=Timeout` on the next reconcile.

## `status` fields

`status` is written by the controller and should not be edited by users.

| Field | Type | Description |
|-------|------|-------------|
| `phase` | enum `Pending` \| `Running` \| `Completed` \| `Failed` \| `Paused` \| `Snapshotting` \| `Restoring` \| `Suspended` | High-level lifecycle state. Terminal phases (`Completed`, `Failed`) never roll back. `Suspended` (session + class `sessionCheckpoint` only) means the microVM was checkpointed to the portable store and released; no Pod exists while suspended, and the workspace PVC plus the checkpoint survive. |
| `reason` | string | Short, machine-readable explanation for the current phase. Populated on `Failed` with values such as `Timeout`, `IdleTimeout` (session idle eviction, docs/design/lifecycles.md), `ImagePullFailure`, `UnsupportedBackend`, `ContainerExitedNonZero`, `ClassNotFound` (see [Orphaned Sandboxes](#orphaned-sandboxes-classnotfound)); on a session Sandbox, `Pending`/`SessionVMRestarting` marks a VM being replaced after exit. |
| `exitCode` | *int32 | Exit status of the workload container once the Sandbox is terminal. `nil` while the Sandbox is `Pending` or `Running`. |
| `podName` | string | Name of the backing Pod created by the controller. Defaults to `<sandbox-name>-vm`. |
| `startedAt` | `metav1.Time` | Time the underlying Pod first transitioned to `Running`. |
| `lastTransitionTime` | `metav1.Time` | Timestamp of the most recent phase change. |
| `warmStart` | object | Outcome of the one-shot pre-warm pool attempt (docs/design/lifecycles.md) for Sandboxes whose class declares `preWarmPoolSize > 0` and whose image equals the class `preWarmImage`. `outcome` is `PoolRestored` (started from a warm base, `entryID` set) or `ColdBoot` (`reason` = `miss` or `error`). `nil` when no attempt applied. A `ColdBoot` outcome is a fallback, never a failure. |
| `checkpoint` | object | Session memory-checkpoint bookkeeping (session + class `sessionCheckpoint` only). `ref`/`backend`/`sequence`/`takenAt`/`sizeBytes` describe the single retained checkpoint (a new one replaces its predecessor; a restore consumes it). `pendingRestore` marks a fresh VM that must restore from `ref`. `lastRecovery` reports how the most recent VM (re)start recovered: `ResumedFromCheckpoint` (process continued) or `RestartedFromWorkspace` (the distinct degraded condition — the process restarted against the durable workspace; no data lost). While `Suspended`, `status.reason` is one of `SuspendedIdle`, `UserSuspended`, or `CheckpointOnDrain`. |

## Phase state machine

```
               +---------+
(create) ----> | Pending | ---- Pod Running ----> +---------+
               +---------+                       | Running |
                    |                             +---------+
                    |                                  |
         Pod fails to start                  +---------+---------+
         (ImagePullBackOff,                  |                   |
          UnsupportedBackend, ...)      exit code 0         exit != 0,
                    |                        |              timeout,
                    v                        v              container crash
               +---------+             +-----------+        |
               | Failed  | <-----------| Completed |        |
               +---------+             +-----------+        |
                    ^------------------------------------------+
```

- `Pending` → `Running`: triggered by the Pod transitioning to `Running`.
- `Running` → `Completed`: container exits with code `0`.
- `Running` → `Failed`: container exits non-zero, timeout elapses, or the
  Pod fails to start after the grace period.
- `Pending` → `Failed`: the Pod cannot be scheduled or the workload image
  cannot be pulled within the grace period.

Terminal phases are absorbing — once `Completed` or `Failed`, the Sandbox
stays there until deleted.

### Orphaned Sandboxes (`ClassNotFound`)

A Sandbox whose `spec.sandboxClassName` does not resolve follows a bounded
three-stage ramp instead of waiting forever:

| Age | State | Why |
| --- | --- | --- |
| < 5m | `Pending` / `ClassNotFound` | The only transient case: the Sandbox and its class were created together and the operator's cache has not observed the class yet. |
| ≥ 5m | `Failed` / `ClassNotFound` (terminal) | The class was never created, or it was deleted. No amount of waiting brings it back, and no Pod is ever built, so nothing is unschedulable and no autoscaler reacts. |
| ≥ 1h 5m | deleted | An orphan holds no result and never ran. Nothing else can collect it — Kubernetes garbage collection needs an in-cluster owner, and an orphan has none. |

Deletion is deliberately aimed only at that population: terminal, reason
`ClassNotFound`, and the class still unresolvable on the reconcile that
collects it. A Sandbox that reached `Completed`, or `Failed` for any other
reason, is never touched by this path — it carries a result its creator owns,
and its class being cleaned up afterwards says nothing about it.

Each stage records a Warning Event, so `kubectl describe sandbox` explains a
genuine misconfiguration for the whole hour before the object is collected.

## kubectl usage

```bash
# Shortest alias
kubectl get sbx

# Explain any field
kubectl explain sandbox.spec.resources
kubectl explain sandbox.status

# Tail events and phase transitions
kubectl describe sandbox <name>
kubectl get sandbox <name> -w
```

## SandboxClass

`SandboxClass` is a cluster-scoped resource introduced in Phase 2.
Administrators author classes; tenants reference them by name in
`Sandbox.spec.sandboxClassName`.

### Schema

- `spec.runtime.backend` — `launcher` or empty. Empty means `launcher`:
  the launcher is the only runtime (setec#198). Each Sandbox of the class
  is one Firecracker microVM in one launcher Pod.

### Validation rules (enforced by the SandboxClass webhook)

- `spec.runtime.backend` must be `launcher` or empty. A class that names a
  removed backend (`kata-fc`, `kata-qemu`, `gvisor`, `runc`) fails
  admission with a message that names setec#198. A class that skipped
  admission cannot run a Sandbox either: the operator fails the Sandbox
  with the reason `UnsupportedBackend`.
- A class with `spec.preWarmPoolSize` needs `spec.preWarmImage` by digest
  and `spec.defaultResources`.
- `spec.requests.cpu` and `spec.requests.memory`, when set, must be
  positive and must not exceed `spec.maxResources` when the class
  states a ceiling.
- `spec.sessionCheckpoint.interval`, when set, must be positive;
  `spec.sessionCheckpoint.backend` must be `s3` (or empty). On the
  Sandbox side, `spec.desiredState: Suspended` is rejected unless the
  Sandbox is a session AND its class sets `spec.sessionCheckpoint`.

### Example

```yaml
apiVersion: setec.zeroroot.ai/v1alpha1
kind: SandboxClass
metadata:
  name: standard
spec:
  runtime:
    backend: launcher
  defaultResources:
    vcpu: 2
    memory: 2Gi
  maxResources:
    vcpu: 8
    memory: 16Gi
  allowedNetworkModes:
    - none
    - egress-allow-list
  default: true
```

### kubectl usage

```bash
# Shortest alias
kubectl get sbxcls

# Printer columns show Backend, Default, Max-VCPU, Max-Memory, Age.
kubectl get sandboxclasses.setec.zeroroot.ai
```

### Sandbox.status.runtime.chosen

When a Sandbox schedules, the controller writes `launcher` to
`status.runtime.chosen`.

A launcher Pod asks for the extended resources `setec.zeroroot.ai/kvm` and
`setec.zeroroot.ai/tun`. If no Node offers them, the Pod stays
unschedulable and the Sandbox stays `Pending`, with the scheduler's own
explanation on the Pod. A cluster autoscaler provisions in response to
that Pod. Check that the node pool exposes `/dev/kvm`, and that the
SandboxClass tolerates the taint of the pool.

## Phase 3 extensions

### Snapshot

Namespaced resource representing a saved microVM state (CPU
registers, memory, metadata). Created by the operator when a
Sandbox requests `snapshot.create=true`; consumed by later Sandboxes
via `spec.snapshotRef`.

Short name: `snap`.

```yaml
apiVersion: setec.zeroroot.ai/v1alpha1
kind: Snapshot
metadata:
  name: my-state
  namespace: tenant-a
spec:
  sourceSandbox: workload-a
  sandboxClass: standard
  imageRef: ghcr.io/org/app@sha256:<digest>
  kernelVersion: "6.1.0"
  ttl: 168h
  storageBackend: local-disk
  storageRef: "tenant-a-my-state"
  size: 2147483648
  sha256: "..."
  node: node-a
status:
  phase: Ready
  referenceCount: 1
  lastTransitionTime: "2026-04-15T12:00:00Z"
```

Printer columns: NAME, PHASE, CLASS, SIZE, NODE, AGE.

### Sandbox extensions

Three additive fields on `SandboxSpec`:

- `desiredState` (`Running` | `Paused`, default `Running`)
- `snapshot` (optional block: `create`, `name`, `afterCreate`, `ttl`)
- `snapshotRef` (optional block: `name`)

`SandboxPhase` enum gains `Paused`, `Snapshotting`, `Restoring`.
`SandboxStatus` gains `pausedAt`.

### SandboxClass extensions

Three additive fields on `SandboxClassSpec`:

- `preWarmPoolSize` (int; default 0). The number of warm bases of the
  class: snapshots of a machine that booted `preWarmImage` and ran no
  workload. A Sandbox of the class with that image and size loads a base
  instead of a boot.
- `preWarmImage` (string, by digest; required when pool size is non-zero)
- `maxPauseDuration` (Go duration; optional, must be positive when set —
  the webhook rejects zero or negative values). Bounds how long a
  Sandbox may hold a paused microVM (`phase=Paused`). Past the cap the
  reconciler transitions the Sandbox to Failed with
  `reason=PauseTimeoutExceeded`, emits a `PauseTimeoutExceeded` Warning
  Event, and deletes the VM Pod. Sessions in a class with
  `sessionCheckpoint` enabled suspend instead (`phase=Suspended`,
  `reason=SuspendedPauseTimeout`): checkpoint retained, microVM
  released, resumed when `spec.desiredState` returns to `Running`. The
  `Suspended` phase itself is not bounded by this cap — a suspended
  session holds no microVM. Unset means pauses are unbounded.
