# Setec Helm Chart

Setec is a Kubernetes-native operator that orchestrates Firecracker microVMs
via Kata Containers, exposing a high-level `Sandbox` custom resource for
ephemeral, isolated workload execution.

This chart installs a single controller-manager Deployment, the `Sandbox`
CustomResourceDefinition, and the minimum ClusterRole needed for the operator
to reconcile `Sandbox` resources into Kata-runtime Pods.

## Prerequisites

- Kubernetes 1.30 or later. The Sandbox-namespace host guard is a
  `ValidatingAdmissionPolicy`, which reached
  `admissionregistration.k8s.io/v1` in 1.30. It is not an optional extra:
  without it a `hostNetwork` Pod steps around the namespace default-deny
  NetworkPolicy, so the chart states 1.30 as its floor rather than
  rendering containment conditionally.
- At least one Node with KVM access (bare-metal Linux or a VM with nested
  virtualization enabled). Setec runs Firecracker microVMs, which require
  `/dev/kvm`.
- [Kata Containers](https://github.com/kata-containers/kata-containers)
  installed on the cluster. The recommended path is
  [`kata-deploy`](https://github.com/kata-containers/kata-containers/tree/main/tools/packaging/kata-deploy),
  which installs the Kata binaries, the `kata-fc` RuntimeClass, and labels
  Kata-capable Nodes.
- `helm` 3.8 or later.

Setec is cloud-agnostic and makes no assumptions about the underlying
infrastructure. Any conformant Kubernetes distribution whose worker Nodes
expose `/dev/kvm` will work.

## CRD handling

The `Sandbox` CRD is shipped in `crds/` inside this chart. Helm's built-in
CRD handling means:

- `helm install` installs the CRD before rendering templates.
- `helm upgrade` does NOT modify the CRD. If the CRD schema changes between
  chart versions, apply the new CRD manually before upgrading (see
  [Upgrading](#upgrading) below).
- `helm uninstall` does NOT delete the CRD. Existing `Sandbox` resources are
  preserved. Removing the CRD is an explicit, opt-in step (see
  [Uninstalling](#uninstalling)).

This matches the requirement that operator removal must not cascade into
user-authored resources.

## Installing

```bash
helm install setec ./charts/setec \
  --namespace setec-system \
  --create-namespace
```

Or, if you prefer the chart to manage the namespace itself:

```bash
helm install setec ./charts/setec \
  --namespace setec-system \
  --set createNamespace=true
```

Verify the operator is running:

```bash
kubectl -n setec-system get deployment setec
kubectl -n setec-system logs deployment/setec
```

The operator's `/readyz` body includes a `kata_runtime_available` field; if
`false`, install `kata-deploy` and wait for the DaemonSet to label Nodes.

## Upgrading

```bash
# 1. Apply any CRD schema changes shipped with the new chart version.
kubectl apply -f charts/setec/crds/setec.zeroroot.ai_sandboxes.yaml

# 2. Upgrade the release.
helm upgrade setec ./charts/setec --namespace setec-system
```

A rolling update of the operator Deployment does not disrupt existing
`Sandbox` reconciliation — the new replica takes leadership (or becomes the
sole replica) and continues where the previous one left off, using the
Kubernetes API as the source of truth.

## Uninstalling

Remove the operator without touching user `Sandbox` resources:

```bash
helm uninstall setec --namespace setec-system
```

To fully remove Setec, including the CRD (which also deletes every `Sandbox`
in the cluster because the CRD owns them), run the follow-up command:

```bash
kubectl delete crd sandboxes.setec.zeroroot.ai
```

If you used `createNamespace=true` and want the namespace gone too:

```bash
kubectl delete namespace setec-system
```

## Values

| Key | Default | Description |
|-----|---------|-------------|
| `image.repository` | `ghcr.io/zeroroot-ai/setec` | Container image repository. |
| `image.tag` | `"0.1.0"` | Image tag; falls back to `.Chart.AppVersion` when empty. |
| `image.pullPolicy` | `IfNotPresent` | Image pull policy. |
| `imagePullSecrets` | `[]` | Pull secrets for private registries. |
| `nameOverride` | `""` | Override the chart name in generated resource names. |
| `fullnameOverride` | `""` | Override the full resource name prefix. |
| `createNamespace` | `false` | If true, the chart renders the target Namespace. |
| `namespace` | `setec-system` | Target namespace for the Deployment and ServiceAccount. |
| `replicas` | `1` | Number of controller-manager Pods. Use with `leaderElect: true` for HA. |
| `resources.requests.cpu` | `100m` | CPU request. |
| `resources.requests.memory` | `128Mi` | Memory request. |
| `resources.limits.cpu` | `500m` | CPU limit. |
| `resources.limits.memory` | `512Mi` | Memory limit. |
| `runtimeClassName` | `kata-fc` | Kata RuntimeClass the operator attaches to Sandbox Pods. |
| `nodeSelectorLabel` | `katacontainers.io/kata-runtime` | Node label key for the startup prerequisite check. |
| `leaderElect` | `false` | Enable controller-runtime leader election. Required for replicas > 1. |
| `metricsPort` | `8080` | TCP port for the Prometheus metrics endpoint. |
| `healthPort` | `8081` | TCP port for `/healthz` and `/readyz`. |
| `serviceAccount.create` | `true` | Render a ServiceAccount alongside the Deployment. |
| `serviceAccount.name` | `""` | Name override for the ServiceAccount. |
| `podSecurityContext.runAsNonRoot` | `true` | Forbid running as root. |
| `podSecurityContext.seccompProfile.type` | `RuntimeDefault` | Use the runtime's default seccomp profile. |
| `containerSecurityContext.allowPrivilegeEscalation` | `false` | Forbid privilege escalation. |
| `containerSecurityContext.readOnlyRootFilesystem` | `true` | Mount the root filesystem read-only. |
| `containerSecurityContext.runAsNonRoot` | `true` | Forbid running as root (container-level). |
| `containerSecurityContext.runAsUser` | `65532` | UID used by the distroless nonroot base image. |
| `containerSecurityContext.capabilities.drop` | `[ALL]` | Drop every Linux capability. |
| `nodeSelector` | `{}` | Constrain the operator Pod to matching Nodes. |
| `tolerations` | `[]` | Toleration list for the operator Pod. |
| `affinity` | `{}` | Affinity / anti-affinity rules for the operator Pod. |

## Example Sandbox

After the chart is installed, apply a minimal Sandbox:

```yaml
apiVersion: setec.zeroroot.ai/v1alpha1
kind: Sandbox
metadata:
  name: hello
  namespace: default
spec:
  image: docker.io/library/python:3.12-slim
  command: ["python", "-c", "print('hello from a Firecracker microVM')"]
  resources:
    vcpu: 1
    memory: 512Mi
```

```bash
kubectl apply -f sandbox.yaml
kubectl get sbx
kubectl logs hello-vm
```

## Security

- The operator image is built from `gcr.io/distroless/static-debian12:nonroot`
  and contains no shell or package manager.
- The Pod runs as UID 65532 with a read-only root filesystem and drops every
  Linux capability.
- The ClusterRole grants only the verbs the operator actually calls: CRUD on
  `sandboxes` and `pods`, status/finalizer writes on `sandboxes`, `events`
  create/patch, and read-only access to `nodes` and `runtimeclasses`.

### runtime-agent node writes

The runtime-agent DaemonSet probes each node and publishes what it found so
the operator can schedule Sandboxes onto capable nodes. That publication is
a Node write, and Node writes are cluster-wide primitives, so the grant is
narrowed on three axes:

- **No `nodes/status`.** The probe detail used to be a `SetecRuntimes` node
  condition, which needed `nodes/status: patch` on every node — a verb whose
  holder can rewrite any node's allocatable capacity and readiness. Nothing
  read the condition, so the detail is now the
  `setec.zeroroot.ai/runtime-probe` **annotation** and the grant is gone.
  `kubectl get node -o yaml` shows the same JSON.
- **Non-root, verified.** The agent image has always been distroless-nonroot,
  but the DaemonSet asserted `runAsNonRoot: false`, so nothing checked. It
  now runs with `runAsNonRoot: true`, an explicit UID/GID of 65532 and
  `seccompProfile: RuntimeDefault`. Every probe is an `os.Stat` against a
  read-only mount, so nothing about this needs privilege.
- **`nodes: patch` narrowed by admission.** The agent still needs to write
  its capability labels, and RBAC cannot scope `patch` to particular label
  keys or to the agent's own node. `runtimeAgent.nodeGuard` installs a
  `ValidatingAdmissionPolicy` that holds the agent's ServiceAccount to the
  `setec.zeroroot.ai/runtime.` label prefix plus the one probe annotation,
  rejects any other change to the object including all of `spec`, and — on
  clusters that bind node identity into ServiceAccount tokens (1.31+) —
  rejects a write aimed at any node but its own.

  Node-identity enforcement is opportunistic by default so the chart still
  installs on clusters without node-bound tokens. Set
  `runtimeAgent.nodeGuard.requireNodeIdentity: true` to make it mandatory
  once every node is on 1.31+; with it on and the claim absent, every probe
  write is rejected and the operator sees no capable nodes.

## Troubleshooting

- `kubectl describe sandbox <name>` shows the most recent events, including
  `RuntimeUnavailable` warnings when the `kata-fc` RuntimeClass is missing.
- The operator's `/readyz` endpoint returns a JSON body with
  `kata_runtime_available` and `kata_capable_nodes` booleans. Port-forward
  the health port (default `8081`) and curl `/readyz` to inspect.
- If Pods stay `Pending`, check the Node labels Kata-capable Nodes are
  labelled with (default: `katacontainers.io/kata-runtime`) and confirm
  `kata-deploy` has completed rolling out.

## Phase 2: Multi-tenancy, Observability, Webhook, Node-Agent, Frontend

Phase 2 adds five opt-in features. A default install has every one disabled,
and therefore behaves identically to Phase 1. Enable one at a time and
verify the expected new manifests appear via `helm template`.

- `multiTenancy.enabled=true` — the operator rejects Sandboxes in namespaces
  missing the configured `multiTenancy.tenantLabelKey` (default
  `setec.zeroroot.ai/tenant`). Combine with `ResourceQuota` and the node-agent
  `NetworkPolicy` enforcement for full per-tenant isolation.
- `observability.enabled=true` plus `observability.otlpEndpoint` enables
  OpenTelemetry trace export. Traces go over TLS by default using the pod's
  system root CAs; set `observability.otelTLS.caSecretName` to mount a
  private CA bundle (the Secret must contain a single `ca.crt` key). Setting
  `observability.otelTLS.enabled=false` falls back to plaintext — the
  operator emits a loud warning on startup and this is NOT a production
  configuration. `/metrics` is always on; set `metricsEnabled=false` to
  turn it off behind a network policy.
- `webhook.enabled=true` installs the `ValidatingWebhookConfiguration` and the
  webhook `Service` that routes admission requests to port 9443 inside the
  operator pod. Supply a `Secret` at `/tmp/k8s-webhook-server/serving-certs`
  or enable `webhook.certManager.enabled=true` and supply a cert-manager
  `IssuerRef`.
- `nodeAgent.enabled=true` installs the DaemonSet targeting the
  `nodeAgent.nodeSelector` (default `katacontainers.io/kata-runtime=true`).
  Provide `thinpoolDataDevice` and `thinpoolMetadataDevice` block devices per
  node. The agent exposes Prometheus metrics on port 9090. It runs privileged
  (SYS_ADMIN only) because device-mapper control requires it.
- `frontend.enabled=true` installs the `setec-frontend` Deployment + ClusterIP
  Service. **Both `tlsCertSecretName` and `tlsClientCASecretName` are
  required** — mTLS is mandatory for the frontend and the chart refuses to
  render without them. The frontend does NOT bypass Kubernetes admission;
  every call still flows through the webhook.
- `sandboxClasses.enabled=true` (the default) templates the `SandboxClass`
  set tenants launch against. The chart ships two: `tool`
  (`defaultNetworkMode: external-only`, marked cluster-default) and
  `connector` (`defaultNetworkMode: none`). A Sandbox that omits
  `spec.sandboxClassName` resolves to the class marked default; a Sandbox
  that omits `spec.network` inherits that class's `defaultNetworkMode`.
  Every class must state a `defaultNetworkMode` or the chart refuses to
  render.

### Sandbox egress posture

`netpol.reservedCIDRs` is the address space no Sandbox may reach. It is
subtracted from every permissive egress rule the operator generates, and
neither the chart nor the operator will start with it empty.

**Add your cluster's Service and Pod CIDRs.** The default covers RFC1918,
link-local (including the cloud instance-metadata address), CGNAT,
loopback and multicast — but the chart cannot discover the ranges your
own control plane sits on.

**Self-hosted installs must retune it.** If the authorised scope for your
workloads is private address space, this default denies exactly what you
meant to permit. Narrow the list to your own control-plane ranges rather
than clearing it.

`netpol.resolvers` are the DNS servers Sandboxes may query. The same
addresses are written into every Sandbox Pod's `dnsConfig` with
`dnsPolicy: None`, so Sandboxes do not use cluster DNS and cannot
enumerate in-cluster Services by name.

`sandboxNamespaces` names the namespaces Sandboxes run in, and is
required (an empty list fails the render). It does two things:

1. Renders a **namespace-wide** deny-all `NetworkPolicy` — `podSelector:
   {}`, every Pod, not only labelled Sandbox Pods. The per-Sandbox
   policies select on `setec.zeroroot.ai/sandbox`, so they confine Pods
   the operator built; a Pod created in the namespace by any other route
   is selected by no policy and is therefore unrestricted. This is what
   removes that state. The operator re-applies the same policy at
   reconcile time (`--namespace-baseline-deny`), so namespaces created
   after install are covered too.
2. Binds the operator's Pod- and NetworkPolicy-**write** RBAC to those
   namespaces with a `RoleBinding` rather than cluster-wide. Reads stay
   cluster-wide for the controller cache. Cluster-wide `pods: create` is
   a cluster-admin path on its own; set
   `rbac.allowClusterWideSandboxWrite: true` to take it deliberately.

A namespace listed here is a Sandbox-only namespace: the baseline denies
every Pod in it, no label-based exemption is offered (any label an
exempt Pod wears an adversary can wear too), and the chart refuses to
render if the list names the operator's own namespace.

The baseline does not stop a Pod being created — that is RBAC — and it
does not apply to `hostNetwork: true` Pods, which are outside
NetworkPolicy enforcement. Pod Security Admission does not close that
either: `baseline`, the weakest level forbidding `hostNetwork`, also
forbids the `NET_ADMIN` that Sandbox Pods add on purpose, so enforcing it
rejects every legitimate Sandbox.

`sandboxHostGuard` (on by default) is what closes it. It renders a
`ValidatingAdmissionPolicy` plus a binding scoped to `sandboxNamespaces`
that denies, on Pod `CREATE` and `UPDATE`:

| Denied | Why it defeats the baseline |
|---|---|
| `hostNetwork` | Pod runs in the node network namespace, where NetworkPolicy is not enforced |
| `hostPID` | Pod sees every process on the node |
| `hostIPC` | Pod shares the node IPC namespace |
| `hostPath` volumes | Pod mounts the node filesystem |
| `hostPort` | Pod is published on the node address, past the namespace ingress policy |
| `privileged` containers | Container holds the node |

That is the host-access half of PSA `baseline` without the capability
rule Setec cannot adopt. It is in-tree CEL evaluated by the API server —
no controller, no webhook endpoint, and therefore no outage that turns
the check off — with `failurePolicy: Fail`, so a policy the API server
cannot evaluate rejects the write rather than admitting it unchecked.

`ValidatingAdmissionPolicy` reached `admissionregistration.k8s.io/v1` in
Kubernetes 1.30, which is the chart's `kubeVersion` floor. The Sandbox
Pods the operator itself builds never request any of the above, so the
policy is invisible to normal operation; it only ever fires on a Pod that
arrived by some other route.

None of this enforces anything unless the cluster's CNI implements
`networking.k8s.io/v1` NetworkPolicy. Verify that against the running
cluster; the chart cannot detect it.

### Phase 1 to Phase 2 migration

Upgrading from a Phase 1 chart to Phase 2 is non-breaking. Running Sandboxes
keep their existing Pods; the CRD change adds fields only. To enable each
feature incrementally:

1. `helm upgrade` with no value changes — verify existing Sandboxes keep
   running and no new reconcile errors appear in operator logs.
2. Install the `SandboxClass` CRD (shipped in `crds/`) and apply at least
   one class, then opt in to `webhook.enabled=true`.
3. Enable multi-tenancy only after labelling each tenant namespace with
   `setec.zeroroot.ai/tenant=<tenant>`.
4. Turn on `nodeAgent.enabled=true` after provisioning `thinpool`-ready
   block devices on worker nodes.
5. Enable `frontend.enabled=true` last; it is the thinnest layer.

## Karpenter scale-to-zero metal pool (EKS)

`karpenter.enabled=true` renders a Karpenter `EC2NodeClass` + `NodePool`
(Karpenter >= 1.0 CRDs required, installed out of band) that provision
Graviton bare-metal nodes (`c6gd.metal` / `m6gd.metal`, cheapest first) from
the baked kata-fc AMI (`packer/eks-kata-fc-ami`) on demand and scale to
zero when no kata Sandbox is running. Nodes carry the
`setec.zeroroot.ai/runtime.kata-fc=true` label and a `kata=true:NoSchedule`
taint; SandboxClasses targeting the pool must set the matching
`spec.tolerations`. When enabled, `karpenter.amiSelectorTerms`,
`karpenter.subnetSelectorTerms`, `karpenter.securityGroupSelectorTerms`,
and exactly one of `karpenter.role` / `karpenter.instanceProfile` are
required. Full wiring, cost model, and the spot-metal caveat:
[`docs/runtime-backends/eks.md`](../../docs/runtime-backends/eks.md).
