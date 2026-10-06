# Node agent

The node agent is a privileged DaemonSet on each fleet node. It serves
the `NodeAgentService` gRPC API that the operator calls to snapshot,
restore, pause and resume the Firecracker machine of a launcher Pod on
the node (`internal/nodeagent/grpcserver`). It also writes and reads
session checkpoints in the S3-compatible store, and it exposes a
Prometheus `/metrics` endpoint.

## Prerequisites

- A Linux node that exposes `/dev/kvm`. The agent exits on a node
  without it.
- The Pod directory of the kubelet on the host
  (`nodeAgent.kubeletPodsDir`, default `/var/lib/kubelet/pods`). The
  agent finds the work volume of each launcher Pod under it.

## Installation

Enable it through the Helm chart:

```bash
helm upgrade --install setec charts/setec \
  -f my-launcher-values.yaml \
  --set nodeAgent.enabled=true \
  --set snapshots.enabled=true
```

`nodeAgent.nodeSelector` is empty by default, so the agent runs on each
node. Set it to the label of the fleet nodes to keep the agent off the
other nodes. The agent runs privileged because it reads and writes the
work volumes of the launcher Pods under the kubelet directory, as root.

## Snapshot state

The agent writes the state of a snapshot under `--snapshot-root`
(default `/var/lib/setec/snapshots`). Each snapshot is encrypted at
rest with its own data key. The data keys are sealed with the node key
(`--snapshot-key-file`) and kept in `--snapshot-dek-dir`, outside the
snapshot root. The agent refuses a new snapshot when the used fraction
of the snapshot filesystem is above `--snapshot-fill-threshold`
(default 0.85).

## Credential modes

The node-agent's gRPC surface (`--grpc-listen-addr`, default `:50052`)
is mTLS with TLS 1.3 as the floor and a mandatory, verified client
certificate. It runs in exactly one credential mode, selected the same
way and with the same failure semantics as the frontend — configuring
both or neither is a startup error naming the cause, and there is no
fallback between them.

**File mode (default).** `--tls-cert`, `--tls-key` and
`--tls-client-ca`. A caller is accepted if the configured CA issued its
certificate — any caller, not a particular one.

**SPIFFE mode.** `--spiffe-socket` points at the SPIFFE Workload API
(for example `unix:///run/spire/agent-sockets/api.sock`), and one or
more `--spiffe-authorized-id` flags list the full SPIFFE IDs allowed to
call this node-agent. The agent's own SVID and the trust bundle come
from the socket and rotate in-process. An empty allow-list is a startup
error: there is no accept-everyone setting. A node-agent that cannot
reach its Workload API fails to boot rather than reverting to files.

The selected mode is stated on stderr at startup
(`node-agent: credential mode: file`), so a pod's logs answer the
question without a manifest diff.

From the Helm chart the mode is `credentials.mode` — install-wide, so
the node-agent, the frontend and the operator's node-agent dialer flip
together. In spiffe mode the chart renders `--spiffe-socket` from
`credentials.spiffe.socketPath` and the allow-list from
`credentials.spiffe.authorizedIDs.nodeAgentClients` (empty fails the
render); the operator's dialer takes its accepted server IDs from
`credentials.spiffe.authorizedIDs.nodeAgentServers`. See the chart
README "Credential modes".

## Troubleshooting

- The agent exits at once: check that `/dev/kvm` exists on the node.
- A snapshot fails with a fill error: free space on the filesystem of
  `--snapshot-root`, or raise `--snapshot-fill-threshold`.
- A session checkpoint fails: check `--s3-bucket` and the credentials
  of the agent (`docs/session-checkpoints.md`).
