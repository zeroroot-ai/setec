# Sandbox CRD Reference

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
| `resources.memory` | resource.Quantity | yes | — | RAM allocated to the microVM (e.g. `512Mi`, `2Gi`). |
| `network` | object | no | class default, else `{mode: none}` | Egress policy for the microVM; see [`spec.network`](#specnetwork) below. |
| `network.mode` | enum `external-only` \| `egress-allow-list` \| `none` | yes (when `network` set) | `none` | Egress posture. Every mode is enforced by a generated NetworkPolicy. |
| `network.allow` | []object | no | `[]` | Permitted egress destinations. Meaningful only when `network.mode: egress-allow-list`. |
| `network.allow[].host` | string (`minLength: 1`) | yes | — | DNS name or IP address permitted as an egress target. Resolved to `ipBlock` peers on the generated rule and re-resolved periodically; also recorded on a `setec.zeroroot.ai/allow-<port>` annotation. A name that does not resolve is **dropped** from the policy (recorded on `setec.zeroroot.ai/unresolved-allow`), never widened to `0.0.0.0/0`. |
| `network.allow[].port` | int32 (`1`–`65535`) | yes | — | Destination TCP port permitted for this host. |
| `network.allow[].cidr` | string | no | — | Address block this entry is pinned to, replacing resolution of `host` for that rule. Set it when the destination range is genuinely known, or when the name does not resolve from inside the cluster. |
| `lifecycle` | object | no | `{}` | Lifecycle selection and runtime constraints applied to the Sandbox. |
| `lifecycle.mode` | enum `ephemeral` \| `session` | no | `ephemeral` | Which lifecycle the Sandbox follows (ADR-0006). `ephemeral` is today's run-to-completion behavior, unchanged. `session` is long-lived with a durable `/workspace` PVC and explicit teardown. **Immutable** — the admission webhook rejects any update that changes the effective mode. See [`spec.lifecycle.mode`](#speclifecyclemode). |
| `lifecycle.workspace` | object | no (session only) | `{}` | Durable per-session workspace volume configuration. Rejected at admission unless `lifecycle.mode: session`. |
| `lifecycle.workspace.size` | resource.Quantity | no | `10Gi` | Requested capacity of the workspace PVC. Must be > 0. |
| `lifecycle.workspace.storageClassName` | string | no | cluster default | StorageClass the workspace PVC is provisioned from. Any CSI driver works. **Encryption at rest is this StorageClass's responsibility** — point it at a class whose driver encrypts volumes; Setec adds no encryption layer of its own. |
| `lifecycle.timeout` | Go duration string (`metav1.Duration`) | no | unset (unbounded) | Maximum wall-clock runtime. When exceeded, the controller terminates the Pod and marks the Sandbox `Failed` with reason `Timeout`. For sessions the timeout spans the whole session, measured from the first VM start. Examples: `30m`, `8h`. |

### `spec.resources`

Both `vcpu` and `memory` are required. The operator translates these into
the Pod's container resource requests and limits; Kata honors them as the
Firecracker microVM's CPU and memory envelope.

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
  authorised scope for your workloads *is* private address space, the
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

A Sandbox declares one of two lifecycles (ADR-0006). The mode is
immutable for the life of the object; to change it, delete the Sandbox
and create a new one.

**`ephemeral` (default).** Run-to-completion: one workload, auto-destroy
on exit, stateless. A Sandbox with no `lifecycle` block, or with
`mode: ephemeral`, behaves exactly as before the mode existed.

**`session`.** Long-lived, ended only by explicit teardown (deleting the
Sandbox, or `Kill` on the gRPC frontend):

- **Durable workspace.** The operator creates a dedicated `ReadWriteOnce`
  CSI PVC named `<sandbox>-workspace` *before* the Pod and mounts it at
  `/workspace`. Data written there survives VM restart and node loss —
  on node failure the CSI driver re-attaches the claim to the failover
  node (ADR-0007). Any CSI driver works; there is no cloud-specific
  storage dependency.
- **VM restart, not completion.** The workload exiting (any exit code)
  does not finish a session. The controller deletes the dead Pod and
  recreates it; the fresh microVM re-mounts the workspace and continues.
  Status transiently shows `Pending` with reason `SessionVMRestarting`.
  `lifecycle.timeout` still fails the Sandbox terminally, keeping the
  restart loop bounded.
- **Teardown wipes the workspace.** Deleting the Sandbox triggers the
  `setec.zeroroot.ai/workspace-teardown` finalizer: the Pod is deleted,
  then the workspace PVC is deleted; the CSI driver destroys the volume
  and every byte of session data with it. One session per VM and per
  workspace — nothing is reusable across sessions (ADR-0005
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
| `phase` | enum `Pending` \| `Running` \| `Completed` \| `Failed` | High-level lifecycle state. Terminal phases (`Completed`, `Failed`) never roll back. |
| `reason` | string | Short, machine-readable explanation for the current phase. Populated on `Failed` with values such as `Timeout`, `ImagePullFailure`, `RuntimeUnavailable`, `ContainerExitedNonZero`; on a session Sandbox, `Pending`/`SessionVMRestarting` marks a VM being replaced after exit. |
| `exitCode` | *int32 | Exit status of the workload container once the Sandbox is terminal. `nil` while the Sandbox is `Pending` or `Running`. |
| `podName` | string | Name of the backing Pod created by the controller. Defaults to `<sandbox-name>-vm`. |
| `startedAt` | `metav1.Time` | Time the underlying Pod first transitioned to `Running`. |
| `lastTransitionTime` | `metav1.Time` | Timestamp of the most recent phase change. |
| `warmStart` | object | Outcome of the one-shot pre-warm pool attempt (ADR-0004) for Sandboxes whose class declares `preWarmPoolSize > 0` and whose image equals the class `preWarmImage`. `outcome` is `PoolRestored` (started from a claimed pool entry, `entryID` set) or `ColdBoot` (`reason` = `miss` or `error`). `nil` when no attempt applied. A `ColdBoot` outcome is a fallback, never a failure. |

## Phase state machine

```
               +---------+
(create) ----> | Pending | ---- Pod Running ----> +---------+
               +---------+                       | Running |
                    |                             +---------+
                    |                                  |
         Pod fails to start                  +---------+---------+
         (ImagePullBackOff,                  |                   |
          RuntimeUnavailable, ...)      exit code 0         exit != 0,
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

- `spec.runtime.backend` — enum: `kata-fc`, `kata-qemu`, `gvisor`, `runc`.
  Required unless the deprecated `spec.vmm` is present (in which case
  Setec's defaulting webhook translates it to `runtime.backend`).
- `spec.runtime.fallback` — optional ordered list of backends to try
  when `spec.runtime.backend` has no eligible Node. Example:
  `[gvisor, runc]` under `backend: kata-fc` means "prefer microVM,
  fall back to gvisor, then to runc on dev clusters".
- `spec.runtime.params` — optional backend-specific tuning (e.g.
  `kata-fc.snapshotEnabled: true`, `gvisor.platform: ptrace|kvm`).
  Schema validated by the `SandboxClass` webhook; empty keys default
  to upstream defaults.
- `spec.vmm` — **deprecated** enum: `firecracker`, `qemu`,
  `cloud-hypervisor`. Retained for back-compat; the defaulting webhook
  maps `firecracker→kata-fc`, `qemu→kata-qemu`. Set `spec.runtime.*`
  instead on new classes.
- `spec.runtimeClassName` — **deprecated**. Use `spec.runtime.backend`
  and let Setec pick the `RuntimeClass` per backend.
- `spec.kernelImage`, `spec.rootfsImage` — optional OCI refs the node
  agent prefetches (kata-fc / kata-qemu only; ignored for gvisor / runc).
- `spec.defaultResources`, `spec.maxResources` — `{vcpu, memory}`
  blocks that set the default and ceiling for tenant Sandboxes.
- `spec.allowedNetworkModes` — subset of
  `[external-only, egress-allow-list, none]`. Empty list means all modes
  allowed. The check runs against the **effective** mode, so a Sandbox
  that omits `spec.network` and inherits `defaultNetworkMode` must
  satisfy this list too.
- `spec.defaultNetworkMode` — the posture applied to Sandboxes in this
  class that declare no `spec.network`. Unset resolves to `none`.
- `spec.defaultEgressAllow` — class-level allow-list applied with
  `defaultNetworkMode: egress-allow-list`.
- `spec.egressExemptCIDRs` — ranges this class may reach despite the
  operator's cluster-wide `--reserved-cidrs`. Entries are subtracted
  from the reserved list before it is rendered into `ipBlock.except`.
  Use it only for a specific in-cluster endpoint a class genuinely
  needs; every entry re-opens address space for every Sandbox in the
  class.
- `spec.nodeSelector` — additive per-Sandbox node selector, merged with
  the backend's own `NodeAffinity` from `setec.zeroroot.ai/runtime.<backend>=true`.
- `spec.tolerations` — additive `[]corev1.Toleration` appended to every
  Sandbox Pod produced under this class. Required when the target
  NodePool carries a taint (e.g. a Karpenter pool reserved for
  sandbox-host nodes) — without a matching toleration the Pod stays
  `Pending` forever.
- `spec.default` — boolean. Exactly zero or one class may carry this.

### Validation rules (enforced by the SandboxClass webhook)

- `spec.runtime.backend` must be in the cluster's enabled-backend set
  (`runtime.<backend>.enabled=true` in Helm values). Attempting to use
  a disabled backend fails admission.
- A backend marked `devOnly` in Helm values (`runtimes.runc.devOnly=true`
  by default) is rejected unless the `default` namespace carries the label
  `setec.zeroroot.ai/allow-dev-runtimes=true`. That label is the cluster
  operator's written consent to namespace-only isolation.
- The same `devOnly` mark also bars the backend from
  `defaults.runtime.backend` and `defaults.runtime.fallback`, and that
  rule is **absolute** rather than gated by the consent label
  (GHSA-q7hq-f8hm-wmjr). The cluster defaults apply to Sandboxes in every
  namespace and never pass through the SandboxClass webhook, so no single
  namespace's label would be the right consent to ask for. The chart
  refuses to render and the operator refuses to start. To run such a
  backend cluster-wide, set `runtimes.<backend>.devOnly=false` — a
  deliberate statement that its isolation is acceptable, rather than a
  side-effect of naming it in the defaults block.
- `spec.vmm` and `spec.runtime.backend` are mutually exclusive; if both
  are provided, admission fails. Migration: set one and delete the other.

### Example (multi-backend with fallback)

```yaml
apiVersion: setec.zeroroot.ai/v1alpha1
kind: SandboxClass
metadata:
  name: standard
spec:
  runtime:
    backend: kata-fc
    fallback:
      - kata-qemu
      - gvisor
    params:
      kata-fc:
        snapshotEnabled: true
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

### Example (dev-only runc class)

```yaml
apiVersion: setec.zeroroot.ai/v1alpha1
kind: SandboxClass
metadata:
  name: dev-fast
spec:
  runtime:
    backend: runc
  defaultResources:
    vcpu: 1
    memory: 512Mi
```

(Requires Helm `runtime.runc.enabled=true` + `runtime.runc.devOnly=true`.)

### kubectl usage

```bash
# Shortest alias
kubectl get sbxcls

# Printer columns show Backend, Default, Max-VCPU, Max-Memory, Age.
kubectl get sandboxclasses.setec.zeroroot.ai
```

### Sandbox.status.runtime.chosen

When a Sandbox schedules, the controller writes the actual backend it
landed on to `status.runtime.chosen`. For fallback chains this lets you
distinguish "scheduled on kata-fc as requested" from "fell back to
kata-qemu because no kata-fc-capable Node was Ready". A
`FallbackExhausted` phase means no backend in the chain had an eligible
Node; `NoEligibleNode` means the primary `spec.runtime.backend` had
none and there was no fallback configured.

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
  imageRef: ghcr.io/org/app:1.2.3
  kernelVersion: "6.1.0"
  vmm: firecracker
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

Four additive fields on `SandboxClassSpec`:

- `preWarmPoolSize` (int; default 0)
- `preWarmImage` (string; required when pool size is non-zero)
- `preWarmTTL` (Go duration; default 24h at runtime)
- `maxPauseDuration` (Go duration; optional)
