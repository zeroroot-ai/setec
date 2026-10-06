# Setec Helm Chart

Setec is a Kubernetes-native operator that runs each `Sandbox` in its own
Firecracker microVM. One machine runs in each launcher Pod.

This chart installs the controller-manager Deployment, the CustomResourceDefinitions,
the KVM device plugin, and the minimum RBAC that the operator needs to
reconcile each `Sandbox` into a launcher Pod.

## Prerequisites

- Kubernetes 1.35 or later. Each launcher Pod mounts the signed disk of its
  image as an image volume, which needs 1.35. The Sandbox-namespace host
  guard is a `ValidatingAdmissionPolicy`, which needs 1.30.
- At least one **x86-64 (amd64)** Node with KVM access (bare-metal Linux or
  a VM with nested virtualization enabled). Each Sandbox is a Firecracker
  microVM, which needs `/dev/kvm`. The KVM device plugin offers `/dev/kvm`
  and `/dev/net/tun` of each Node as the resources `setec.zeroroot.ai/kvm`
  and `setec.zeroroot.ai/tun`. A Node without `/dev/kvm` gets no launcher
  Pod. arm64 is not supported (docs/design/runtime.md): each published
  setec image is single-arch `linux/amd64`, the DaemonSets hardcode a
  `kubernetes.io/arch: amd64` nodeSelector, and each launcher Pod carries
  a matching required node affinity.
- An OCI registry for the signed image disks (`launcher.diskRepo`), a
  Secret with the disk signing seed (`launcher.diskBuilder.signingSecret`),
  and its public keys (`launcher.diskBuilder.publicKeys`). The chart refuses
  to render without them.
- `helm` 3.8 or later.

The launcher is the only runtime. The Kata, gVisor and runc backends and
the node installer were removed (setec#198). A `SandboxClass` that names
one of them is refused at admission, and a `Sandbox` of such a class fails
with the reason `UnsupportedBackend`.

Setec is cloud-agnostic and makes no assumptions about the underlying
infrastructure. Any conformant Kubernetes distribution whose worker Nodes
expose `/dev/kvm` works.

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
  --create-namespace \
  --set launcher.diskRepo=registry.example.com/setec-disks \
  --set launcher.diskBuilder.signingSecret=setec-disk-signing \
  --set 'launcher.diskBuilder.publicKeys={<base64 public key>}' \
  --set 'sandboxNamespaces={sandbox-workloads}'
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
| `launcher.image.repository` | `ghcr.io/zeroroot-ai/setec-launcher` | Launcher image, with Firecracker, the guest kernel and the guest agent. |
| `launcher.diskRepo` | `""` | Repository of the signed image disks. Required. |
| `launcher.diskBuilder.signingSecret` | `""` | Secret with the ed25519 seed that signs each disk. Required. |
| `launcher.diskBuilder.publicKeys` | `[]` | Public keys that each launcher accepts for a disk. Required. |
| `launcher.warmPool.namespace` | `""` | Namespace of the warm-pool bases. Empty turns the warm pool off. |
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
  create/patch, and read-only access to `nodes`.
- A launcher Pod is not privileged. It mounts nothing from the host, gets
  `/dev/kvm` and `/dev/net/tun` from the device plugin, and has one
  capability, `NET_ADMIN`.
- The node agent calls no Kubernetes API, so it has no ServiceAccount token
  and no RBAC.

## Troubleshooting

- `kubectl describe sandbox <name>` shows the most recent events.
- If a launcher Pod stays `Pending`, check that the device plugin reports
  `setec.zeroroot.ai/kvm` on the Node:
  `kubectl get node <name> -o jsonpath='{.status.allocatable}'`.
- A `Sandbox` that fails with the reason `UnsupportedBackend` has a class
  that names a removed backend. Set `spec.runtime.backend` of the class to
  `launcher`, or leave it empty.

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
  `nodeAgent.nodeSelector` plus a hardcoded `kubernetes.io/arch=amd64`
  selector (docs/design/runtime.md). The agent takes and stages the
  snapshots of the launcher machines in the work volumes of the launcher
  Pods, under `nodeAgent.kubeletPodsDir`. It exposes Prometheus metrics on
  port 9090. It runs as root and privileged, because it reads and writes
  those volumes on the host.
- `frontend.enabled=true` installs the `setec-frontend` Deployment + ClusterIP
  Service. In the default file credential mode, **both `tlsCertSecretName`
  and `tlsClientCASecretName` are required** — mTLS is mandatory for the
  frontend and the chart refuses to render without them (in SPIFFE mode the
  identities come from the Workload API instead; see "Credential modes"
  below). The frontend does NOT bypass Kubernetes admission; every call
  still flows through the webhook. `frontend.clients` enrolls each Gibson
  cluster as a named client, with the SPIFFE ID of its daemon, and is
  required. The frontend refuses a caller that is not enrolled. Each
  request carries a tenant. The frontend makes one namespace for each pair
  of client and tenant on the first call of the pair, with its two
  RoleBindings. The ValidatingAdmissionPolicy `-frontend-scope` limits the
  frontend to those writes, and the host guard binds to each such
  namespace by its `setec.zeroroot.ai/sandbox-namespace=true` label.
  `sandboxNamespaces` lists only the namespaces that hold Sandboxes made
  by other means, and it can be empty when the frontend is on.
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
loopback and multicast, in both IPv4 and IPv6 — but the chart cannot
discover the ranges your own control plane sits on. A reserved prefix
only subtracts from an egress block of its own family, so the list must
keep at least one IPv4 and one IPv6 entry. The render and the operator
both refuse a list with a family missing.

**Self-hosted installs must retune it.** If the authorized scope for your
workloads is private address space, this default denies exactly what you
meant to permit. Narrow the list to your own control-plane ranges rather
than clearing it.

`netpol.resolvers` are the DNS servers Sandboxes may query. The same
addresses are written into every Sandbox Pod's `dnsConfig` with
`dnsPolicy: None`, so Sandboxes do not use cluster DNS and cannot
enumerate in-cluster Services by name. A resolver that sits inside
`netpol.reservedCIDRs`, such as the kube-dns ClusterIP, reaches only the
Sandboxes of a class whose `egressAllowSelectors` grant port 53. Every
other class is pointed at the public resolvers alone.

`sandboxClasses.classes[].spec.egressAllowSelectors` is how a class
reaches an in-cluster Service (setec#76). The CNI evaluates egress after
kube-proxy translates the ClusterIP to a backend Pod, so an `ipBlock` for
a ClusterIP never matches. Each entry is `{namespaceSelector,
podSelector, ports}` and renders as one `NetworkPolicy` egress rule with
those selectors as the peer. A port is a number or a container port
name. The chart refuses to render an entry with no selector or no port.
The shipped classes carry none. `values.yaml` shows the two entries a
kind test profile sets, kube-dns on 53 and the platform edge on its
`https` port.

`sandboxNamespaces` names the namespaces Sandboxes run in, and is
required (an empty list fails the render). It does two things:

1. Renders a **namespace-wide** deny-all `NetworkPolicy` — `podSelector:
   {}`, every Pod, not only labeled Sandbox Pods. The per-Sandbox
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

### Credential modes (`credentials.mode`)

The chart renders three mTLS surfaces: the frontend gRPC server, the
node-agent snapshot gRPC server, and the operator's node-agent dialer.
`credentials.mode` selects how all three obtain and verify identities —
one install-wide switch, so a values file cannot produce a frontend on
SPIFFE with a node-agent still on files.

**`file` (default).** Secret-mounted certificates. A chart install that
specifies nothing renders exactly what it rendered before the switch
existed: `frontend.tlsCertSecretName` / `frontend.tlsClientCASecretName`
feed the frontend, `snapshots.mTLS.*` feeds the operator↔node-agent
channel, and both are required where the component is enabled.

**`spiffe`.** Identities come from the SPIFFE Workload API — a SPIRE
agent socket on each node, given as a bare absolute path in
`credentials.spiffe.socketPath` (default
`/run/spire/agent-sockets/api.sock`). The chart mounts the socket's
directory read-only via `hostPath` into each component and renders
`--spiffe-socket` on each component. The frontend authorizes the SPIFFE
ID of each entry of `frontend.clients`. The node-agent and the operator
get one `--spiffe-authorized-id` per entry in the matching
`credentials.spiffe.authorizedIDs` list:

| List | Authorizes | Required when |
|---|---|---|
| `nodeAgentClients` | callers of the node-agent (the operator) | `nodeAgent.enabled=true` + `snapshots.enabled=true` |
| `nodeAgentServers` | node-agent server IDs the operator accepts | `snapshots.enabled=true` |

**Federation.** Each install has its own trust domain (see Authentication in `docs/frontend-api.md`).
`credentials.spiffe.trustDomain` names the domain of this fleet and is
required in SPIFFE mode with the frontend on. An enrolled client in a
different domain needs a `federation` block: `bundleEndpointURL`,
`bundleEndpointProfile` (`https_spiffe` or `https_web`) and, for
`https_spiffe`, `endpointSPIFFEID`. The chart renders one
`ClusterFederatedTrustDomain` for each such client, so SPIRE fetches the
bundle of that domain and serves it to the frontend. The render fails
when a foreign client has no bundle source. The `ClusterSPIFFEID` of the
frontend must list each client domain under `federatesWith`. The install
owns that registration. The frontend refuses a client whose bundle has
not arrived, and keeps serving every other client.

The lists are per trust relationship on purpose: an ID authorized to
call the frontend is not thereby trusted as a node-agent, or vice
versa. An empty list that is in use **fails the render** — "accept
everyone" cannot be reached by omitting configuration, matching the
binaries' own refusal to start with an empty allow-list.

There is no fallback between the modes: a SPIFFE component that cannot
reach its Workload API fails to boot rather than quietly reverting to
files, and the socket `hostPath` uses `type: Directory` so a node
without a SPIRE agent fails Pod creation loudly. `snapshots.mTLS.certManager`
is a file-mode mechanism; enabling it in spiffe mode fails the render
rather than rendering unused Certificate objects.

`hack/verify-chart-credentials.sh` (CI: "Helm credential-mode
assertions") asserts both modes render what this section claims.

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
4. Turn on `nodeAgent.enabled=true` together with `snapshots.enabled=true`.
5. Enable `frontend.enabled=true` last; it is the thinnest layer.
