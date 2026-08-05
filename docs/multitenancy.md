# Multi-tenancy

Setec Phase 2 adds opt-in multi-tenancy built on three Kubernetes-native
primitives: namespace labels for tenant identity, `ResourceQuota` for
quota enforcement, and `NetworkPolicy` for per-sandbox isolation. This
document explains how to enable each layer and how they compose.

## Model

A tenant is any principal the cluster administrator decides to isolate.
Typical choices:

- A CI job runner (one tenant per team).
- A hosted product's customer (one tenant per customer account).
- A classroom account (one tenant per student).

Setec does not define or store tenant identity. It consumes whatever the
cluster already provides via namespace labels, ServiceAccounts, and (for
gRPC frontend callers) mTLS client certificates.

## Enabling tenant enforcement

Install the chart with `multiTenancy.enabled=true`:

```bash
helm upgrade --install setec charts/setec \
  --set multiTenancy.enabled=true \
  --set multiTenancy.tenantLabelKey=setec.zeroroot.ai/tenant
```

When this flag is on, the operator refuses to reconcile a Sandbox in any
namespace that lacks the configured label. The Sandbox is left in
`Pending` with `reason=TenantLabelMissing` and a Warning Event is
recorded.

Label each tenant namespace:

```bash
kubectl label namespace team-a setec.zeroroot.ai/tenant=team-a
```

The label value is opaque to Setec apart from DNS-1123 validation: any
string that looks like a valid DNS label works.

## Resource quotas

Apply a standard `ResourceQuota` to constrain per-namespace consumption.
The operator does not install quotas; combine them with tenant labels:

```yaml
apiVersion: v1
kind: ResourceQuota
metadata:
  name: team-a-quota
  namespace: team-a
spec:
  hard:
    requests.cpu: "8"
    requests.memory: 16Gi
    count/sandboxes.setec.zeroroot.ai: "50"
```

When the quota would be exceeded the backing Pod is not scheduled. The
Sandbox stays `Pending` until the quota frees up; the operator never
throws away the CR.

## Network policies

Every Sandbox gets a NetworkPolicy, and the operator writes it **before**
it creates the Pod. If the policy cannot be applied the Pod is not
created: the Sandbox stays `Pending` and emits a `NetworkPolicyPending`
Event. There is no configuration — no mode, no omitted `spec.network`, no
absent class default — that produces a Sandbox with no policy. An
unstated posture resolves to `none`.

Each effective `mode` maps to a specific shape. All three deny ingress.

- `none`: both `Ingress` and `Egress` PolicyTypes with empty rule lists.
  Every connection is denied, including DNS.
- `external-only`: egress to `0.0.0.0/0` on **every port**, with the
  operator's reserved ranges subtracted via `ipBlock.except`, plus DNS to
  the configured resolvers. This is the posture for workloads that must
  reach arbitrary external endpoints. The rule is deliberately not
  port-scoped: confinement here is by address space, not by port.
- `egress-allow-list`: one TCP rule per `allow` entry, scoped to that
  entry's port, built on the entry's `cidr` (default `0.0.0.0/0`) with
  the same reserved-range subtraction, plus DNS to the configured
  resolvers.

Hostnames in the allow list are recorded as
`setec.zeroroot.ai/allow-<port>` annotations for audit and are NOT
resolved to CIDRs. Resolving them in the operator would bake a DNS answer
into a long-lived object and put a resolver in the reconcile path.
Callers that genuinely know a destination range set `allow[].cidr`
instead; operators who want DNS-aware filtering layer a CNI that provides
it.

### Reserved ranges and resolvers

Two operator flags define the posture:

- `--reserved-cidrs` — address ranges no Sandbox may reach. Subtracted
  from every permissive rule. Default: RFC1918, link-local
  (`169.254.0.0/16`, which covers the cloud instance-metadata address),
  carrier-grade NAT, loopback and multicast. Add this cluster's Service
  and Pod CIDRs. An empty list is rejected at startup.
- `--sandbox-resolvers` — the DNS servers Sandboxes may query. The same
  list is written into each Pod's `dnsConfig` with `dnsPolicy: None`, so
  a Sandbox resolves names through these addresses rather than through
  cluster DNS and cannot enumerate in-cluster Services by name.

A `SandboxClass` may re-open specific reserved ranges for its own
Sandboxes via `spec.egressExemptCIDRs`. An `allow` entry whose `cidr`
lies entirely inside a still-reserved range is dropped rather than
rendered, and recorded on the `setec.zeroroot.ai/suppressed-allow`
annotation.

**Self-hosted installs must retune the reserved list.** If your
authorised scope is private address space, the default reserved list
denies exactly what you meant to permit. Narrow `--reserved-cidrs` to
your own control-plane ranges rather than clearing it.

Network policies are owned by the Sandbox. Deleting the Sandbox
garbage-collects the policy automatically.

### Enforcement is the CNI's job

A NetworkPolicy is a request to the CNI, not a mechanism in itself. On a
cluster whose CNI does not implement `networking.k8s.io/v1` — or has
policy enforcement switched off — every policy in this document renders
correctly and enforces nothing, and neither the operator nor the Helm
chart can detect it. Confirm enforcement against the running cluster
rather than against the manifests.

## SandboxClass-based policy enforcement

`SandboxClass` is a cluster-scoped resource administrators author once
and tenants reference by name. A class carries:

- `runtime.backend`, `runtime.fallback`, `runtime.params`: runtime backend
  selection — `kata-fc`, `kata-qemu`, `gvisor`, or `runc` (dev-only) —
  plus an optional fallback chain and backend-specific tuning. The
  legacy `vmm` + `runtimeClassName` fields are accepted for
  back-compat and translated by the defaulting webhook. See
  [`crd-reference.md`](./crd-reference.md#sandboxclass) for the full schema.
- `kernelImage`, `rootfsImage`: image overrides for kata-fc / kata-qemu
  backends (ignored for gvisor and runc).
- `defaultResources`, `maxResources`: per-Sandbox resource ceilings.
- `allowedNetworkModes`: the subset of `Network.mode` values the
  class permits. Checked against the *effective* mode, so a Sandbox that
  omits `spec.network` and inherits `defaultNetworkMode` must satisfy it
  too.
- `defaultNetworkMode`, `defaultEgressAllow`: the posture applied when a
  Sandbox declares no `spec.network`. Unset resolves to `none`.
- `egressExemptCIDRs`: ranges this class may reach despite the
  cluster-wide reserved list.
- `nodeSelector`: additive node-selector merged into every Pod.
- `tolerations`: additive tolerations appended to every Pod, letting
  Sandboxes schedule onto a tainted NodePool (e.g. a Karpenter pool
  reserved for sandbox-host nodes via a `NoSchedule` taint).
- `default: true`: marks the class as the cluster-default. Zero or one
  classes may carry this flag.

Example:

```yaml
apiVersion: setec.zeroroot.ai/v1alpha1
kind: SandboxClass
metadata:
  name: standard
spec:
  runtime:
    backend: kata-fc
  maxResources:
    vcpu: 4
    memory: 8Gi
  allowedNetworkModes:
    - none
    - egress-allow-list
  default: true
```

The validating webhook rejects any Sandbox that violates the class
ceilings or picks a disallowed network mode. The reconciler performs
the same check as defense in depth, so manually-created CRs that skip
admission still produce clear `ConstraintViolated` Events.

## gRPC frontend

The optional frontend carries tenant identity in its mTLS client
certificate. When the chart installs the frontend with
`tlsClientCASecretName` set, the server extracts the tenant from
the client cert SAN and resolves it to the correct namespace via the
tenant label mapping. Tenants cannot reach other tenants' Sandboxes
through the frontend — every RPC applies the same namespace check.
