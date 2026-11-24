<!-- SPDX-License-Identifier: Apache-2.0 -->
# Network policy of the setec namespace

This page says which network traffic the Pods of the setec namespace may use, and which Kubernetes object enforces it. It describes the chart on `main`. The files that prove each statement are named in the text.

## What the policy states

Each Pod of the setec namespace denies all traffic, except DNS. One policy for each component then allows the traffic that the component needs.

| Pod | Allowed in | Allowed out |
|---|---|---|
| operator | the admission webhook port (9443) from the API server and `systemPolicy.webhookCallers`, metrics from the scrapers | the API server, the node agent gRPC port when snapshots are on, the trace collector when `observability.enabled` is true |
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
| DNS | the Pods or CIDRs of `systemPolicy.dns` (kube-dns by default) | the same value |
| API server | named by entity (`kube-apiserver`) | CIDRs of its endpoints (`systemPolicy.networkPolicy.apiServer`) |
| Image registries | host names (`launcher.imageSources[].hosts`) | CIDRs and ports (`systemPolicy.networkPolicy.egress.imageSources`) |
| Disk registry | host name, or the address when it is an IP | CIDRs and ports (`egress.diskRegistry`), or the address when it is an IP |
| Sigstore trust root | host names (`systemPolicy.sigstoreHosts`) | CIDRs and ports (`egress.sigstore`), optional |
| Trace collector | host name, address, or the Pods of `systemPolicy.otlp` | CIDRs and ports (`egress.otlp`), the address of an IP endpoint, or the Pods of `systemPolicy.otlp` |
| Snapshot store | host name, the address of an IP endpoint, or the Pods of `systemPolicy.snapshotStore` | CIDRs and ports (`egress.snapshotStore`), the address of an IP endpoint, or the Pods of `systemPolicy.snapshotStore` |
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

## A Sandbox waits until the policy is in force

The operator writes the NetworkPolicy of a Sandbox before its Pod exists. The network plugin applies it after the Pod runs, and the time differs for each plugin. On k3s with its policy controller the wait was a few seconds. In that time the policy is not in force. A check or a workload in that window sees open traffic where the policy denies it, or a block where the policy allows.

The launcher closes the window. It holds the machine off the Pod network, and the Pod not Ready, until a probe shows that the plugin enforces the policy. The probe runs in the network namespace of the Pod, so the plugin treats it as the traffic of the machine. It starts with the Pod network and runs while the machine boots, so a plugin that is quick adds no delay.

| Check | How | Must be |
|---|---|---|
| Block | a TCP connect to each blocked destination | a failure |
| Allow | a DNS query to a resolver of the Pod, or a TCP connect to an allowed destination | an answer from at least one |

- **Block.** The operator picks the destination. It is the Kubernetes API Service when the policy blocks it: always in mode `none`, and in the other modes when its address is in the reserved ranges of the class. The destination must accept a connection when no policy blocks it, because a plugin may answer a blocked connection with a refusal. `netpol.gate.blocked` adds destinations. The check is hard. The wait ends with a failure when a blocked destination stays reachable.
- **Allow.** The resolvers of the Pod are allowed in each mode but `none`. The launcher waits up to 10 seconds after the block is proven for an answer. It then goes on and logs that no destination answered, because a cluster may reach no resolver for a reason that is not the policy, such as an air gap. `netpol.gate.allowed` adds destinations.
- **No probe.** A Sandbox with no destination to check (the API Service address is outside the reserved ranges, and no entry is set) gets no gate. The condition is then absent.

The result is on the Sandbox as the condition `NetworkPolicyEnforced`.

| Status | Reason | Meaning |
|---|---|---|
| `Unknown` | `WaitingForPolicy` | the Pod runs and the launcher waits for the proof |
| `True` | `PolicyProven` | the probe passed, and the Pod is Ready |
| `False` | `NetworkPolicyNotEnforced` | the wait ended without proof. The machine never got network access, and the Sandbox is `Failed` with the same reason. The message names the destination that failed |

`netpol.gate.wait` bounds the wait (30 seconds by default). `netpol.gate.enabled: false` turns the gate off. A Sandbox then starts at once and has no condition. The gate works with each `systemPolicy.engine` and with each plugin that enforces NetworkPolicy, because it measures the effect and reads no state of a plugin. The proof is for the destinations that it checks. It does not prove each rule of the policy.

The code is in `internal/launcher/gate.go` (the probe and the marker), `internal/netpol/gate.go` (the destinations), `internal/podspec/launcher.go` (the spec and the annotation), and `internal/status/policy.go` (the condition).

## Decision

Setec first required Cilium on every cluster (decision D76), and rendered the Cilium objects always. That rule failed an install on a cluster with another network plugin, and it forced such an install to remove the objects after the render. D76 is changed:

- Cilium stays the default and the engine of the tested path.
- `networkpolicy` is the second supported engine. It renders the tightest policy that a NetworkPolicy can state, with the three limits above. It does not turn the policy off.
- No engine means no policy. The chart has no setting that renders none.
- A cluster that has neither Cilium nor a NetworkPolicy plugin has no enforcement. Its Pods are not isolated from each other, whatever the chart renders. Setec does not detect this for its own Pods. The policy gate of a Sandbox does detect it: no destination is blocked, so the Sandbox fails with the reason `NetworkPolicyNotEnforced`.

## Where the chart checks it

- `hack/check-system-policy.py` reads a render of either engine. It fails when the default deny is missing, when a component has no allow, when a Job reaches the world, or when a rule has no peer or no port.
- `hack/verify-chart-security.sh` renders both engines, runs the checker on each, and runs it on a failing fixture for each rule. It also checks that the `cilium` engine refuses a cluster with no Cilium CRDs, and that the `networkpolicy` engine renders no Cilium object on such a cluster.
