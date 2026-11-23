<!-- SPDX-License-Identifier: Apache-2.0 -->
# Network policy of the setec namespace

This page says which network traffic the Pods of the setec namespace may use, and which Kubernetes object enforces it. It describes the chart on `main`. The files that prove each statement are named in the text.

## What the policy states

Each Pod of the setec namespace denies all traffic, except DNS. One policy for each component then allows the traffic that the component needs.

| Pod | Allowed in | Allowed out |
|---|---|---|
| operator | the admission webhook port (9443) from the API server and `systemPolicy.webhookCallers`, metrics from the scrapers | the API server, the node agent gRPC port when snapshots are on |
| frontend | gRPC from `systemPolicy.frontendCallers`, metrics from the scrapers | the API server |
| node agent | gRPC from the operator when snapshots are on, metrics from the scrapers | the snapshot store when `snapshots.s3.enabled` is true |
| device plugin | nothing | nothing (it talks to the kubelet over a unix socket) |
| disk builder Job | nothing | the image registries and the disk registry |
| image verify Job | nothing | the image registries, the disk registry when it is an image source, and the Sigstore trust root |

The Sandbox namespaces have their own default deny. It is a plain NetworkPolicy for every Pod of the namespace, and the operator adds one allow policy for each Sandbox. Both engines render it the same way, so a Sandbox namespace needs no Cilium. `charts/setec/templates/sandbox-namespace-baseline-netpol.yaml`, `internal/netpol/`.

## Two engines

`systemPolicy.engine` selects the objects that carry the policy.

| | `cilium` (default) | `networkpolicy` |
|---|---|---|
| Objects | `CiliumClusterwideNetworkPolicy`, `CiliumNetworkPolicy` | `networking.k8s.io/v1` `NetworkPolicy` |
| Template | `charts/setec/templates/system-namespace-policy.yaml` | `charts/setec/templates/system-namespace-networkpolicy.yaml` |
| Cluster needs | Cilium as the network plugin | a network plugin that enforces NetworkPolicy |
| API server | named by entity (`kube-apiserver`) | CIDRs of its endpoints (`systemPolicy.networkPolicy.apiServer`) |
| Image registries | host names (`launcher.imageSources[].hosts`) | CIDRs and ports (`systemPolicy.networkPolicy.egress.imageSources`) |
| Disk registry | host name, or the address when it is an IP | CIDRs and ports (`egress.diskRegistry`), or the address when it is an IP |
| Sigstore trust root | host names (`systemPolicy.sigstoreHosts`) | CIDRs and ports (`egress.sigstore`), optional |
| Snapshot store | host name, or the address of an IP endpoint | CIDRs and ports (`egress.snapshotStore`), or the address of an IP endpoint |
| Webhook callers | the API server, the nodes, and `webhookCallers` | `apiServer.cidrs`, `webhookSourceCIDRs`, and `webhookCallers` |

### What the networkpolicy engine cannot state

A NetworkPolicy matches a peer by Pod label, namespace label or address block. It has no host name and no API server entity. Three limits follow.

1. A Job reaches an address range, not a host. A range that holds more than the registry also lets the Job reach the other hosts in that range. Name the narrowest range that holds the registry. A registry with no stable range, such as a public one, needs a range of all public addresses (`0.0.0.0/0`). The chart then leaves out `netpol.reservedCIDRs`, so the Job cannot reach the cluster networks. The Job can still reach any public address on the listed ports. Only the `cilium` engine limits the Job to named hosts.
2. The API server is a set of addresses. The install lists them. When the control plane moves, the install changes the list.
3. The source of an admission webhook call depends on the network plugin. The API server may call from its own address, from the address of its node, or from the gateway of the Pod network of the node. The install lists the ones that apply in `webhookSourceCIDRs`. With `webhook.failurePolicy` set to `Fail`, a dropped call rejects every Sandbox.

The render fails, and names the value, when the `networkpolicy` engine lacks a destination that the install needs. A rule with no port or no peer allows every port or every peer in a NetworkPolicy, so the chart refuses an empty list of ports and never renders a rule without peers.

### What the cilium engine needs

The chart refuses to render Cilium objects when the cluster has no Cilium CRDs. The check reads `.Capabilities.APIVersions` for `cilium.io/v2/CiliumNetworkPolicy` and `cilium.io/v2/CiliumClusterwideNetworkPolicy`. `helm install` and `helm upgrade` read the kinds from the cluster. `helm template` reads no cluster. Give it the flags:

```bash
helm template setec charts/setec \
  --api-versions cilium.io/v2/CiliumNetworkPolicy \
  --api-versions cilium.io/v2/CiliumClusterwideNetworkPolicy ...
```

A tool that renders for a live cluster passes the kinds of that cluster in the same way.

## Decision

Setec first required Cilium on every cluster (decision D76), and rendered the Cilium objects always. That rule failed an install on a cluster with another network plugin, and it forced such an install to remove the objects after the render. D76 is changed:

- Cilium stays the default and the engine of the tested path.
- `networkpolicy` is the second supported engine. It renders the tightest policy that a NetworkPolicy can state, with the three limits above. It does not turn the policy off.
- No engine means no policy. The chart has no setting that renders none.
- A cluster that has neither Cilium nor a NetworkPolicy plugin has no enforcement. Its Pods are not isolated from each other, whatever the chart renders. Setec does not detect this.

## Where the chart checks it

- `hack/check-system-policy.py` reads a render of either engine. It fails when the default deny is missing, when a component has no allow, when a Job reaches the world, or when a rule has no peer or no port.
- `hack/verify-chart-security.sh` renders both engines, runs the checker on each, and runs it on a failing fixture for each rule. It also checks that the `cilium` engine refuses a cluster with no Cilium CRDs, and that the `networkpolicy` engine renders no Cilium object on such a cluster.
