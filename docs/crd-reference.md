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
| `network` | object | no | `{mode: full}` | Egress policy for the microVM; see [`spec.network`](#specnetwork) below. |
| `network.mode` | enum `full` \| `egress-allow-list` \| `none` | yes (when `network` set) | `full` | Egress posture. Enforcement of `egress-allow-list` and `none` is deferred to a later phase; the field is accepted today. |
| `network.allow` | []object | no | `[]` | Permitted egress destinations. Meaningful only when `network.mode: egress-allow-list`. |
| `network.allow[].host` | string (`minLength: 1`) | yes | — | DNS name or IP address permitted as an egress target. |
| `network.allow[].port` | int32 (`1`–`65535`) | yes | — | Destination TCP port permitted for this host. |
| `lifecycle` | object | no | `{}` | Runtime constraints applied to the Sandbox. |
| `lifecycle.timeout` | Go duration string (`metav1.Duration`) | no | unset (unbounded) | Maximum wall-clock runtime. When exceeded, the controller terminates the Pod and marks the Sandbox `Failed` with reason `Timeout`. Examples: `30m`, `8h`. |

### `spec.resources`

Both `vcpu` and `memory` are required. The operator translates these into
the Pod's container resource requests and limits; Kata honors them as the
Firecracker microVM's CPU and memory envelope.

### `spec.network`

All three modes (`full`, `egress-allow-list`, `none`) are enforced by a
generated NetworkPolicy owned by the Sandbox. `full` creates no
NetworkPolicy (default pod networking). `egress-allow-list` creates a
policy that permits egress only to the declared `network.allow`
entries plus cluster DNS. `none` creates a policy with empty ingress
and empty egress rules, isolating the Sandbox from every endpoint
including cluster DNS. Sandbox deletion garbage-collects the
NetworkPolicy via its OwnerReference.

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
| `reason` | string | Short, machine-readable explanation for the current phase. Populated on `Failed` with values such as `Timeout`, `ImagePullFailure`, `RuntimeUnavailable`, `ContainerExitedNonZero`. |
| `exitCode` | *int32 | Exit status of the workload container once the Sandbox is terminal. `nil` while the Sandbox is `Pending` or `Running`. |
| `podName` | string | Name of the backing Pod created by the controller. Defaults to `<sandbox-name>-vm`. |
| `startedAt` | `metav1.Time` | Time the underlying Pod first transitioned to `Running`. |
| `lastTransitionTime` | `metav1.Time` | Timestamp of the most recent phase change. |

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
- `spec.allowedNetworkModes` — subset of `[full, egress-allow-list, none]`.
  Empty list means all modes allowed.
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
- `spec.runtime.backend=runc` is rejected unless Helm flag
  `runtime.runc.devOnly=true` is also set. This prevents accidental
  production use of namespace-only isolation.
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
