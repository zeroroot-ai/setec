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

Each `Sandbox.spec.network.mode` maps to a specific NetworkPolicy shape:

- `full` (default): no policy generated. The namespace default policy
  applies.
- `none`: a NetworkPolicy selecting the Sandbox Pod is created with
  both `Ingress` and `Egress` PolicyTypes and empty rule lists. Every
  connection is denied.
- `egress-allow-list`: the policy denies all ingress, permits DNS
  (UDP/TCP 53 to any CIDR), and adds one egress rule per
  `allow` entry (TCP to the requested port against `0.0.0.0/0`).
  Hostnames in the allow list are recorded as
  `setec.zeroroot.ai/allow-<port>` annotations but are NOT resolved to CIDRs —
  operators who need hostname-based filtering should layer a CNI
  with DNS-aware policy (e.g. Cilium) or pre-resolve.

Network policies are owned by the Sandbox. Deleting the Sandbox
garbage-collects the policy automatically.

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
  class permits.
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
