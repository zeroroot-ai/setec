# Local k3s + Setec dev environment

Single-host bring-up of Setec (each Sandbox a real Firecracker microVM in a launcher Pod) on bare-metal Debian, suitable for local integration testing. Detailed walk-through is in [README full doc](#detailed-walk-through) below.

> **Production this is not.** SPIRE runs with the dev trust domain `dev.local`, the cluster is single-node, and operator + sandbox workloads co-locate. For production / multi-tenant / scheduled-uptime patterns see `setec-eks-dev-env`.

## TL;DR

```bash
# One-time, on a fresh bare-metal Debian host with KVM:
make up

# Verify each phase:
make smoke-setec            # Setec works end-to-end
make smoke-cross-cluster    # Pod-in-Kind can reach Setec-on-k3s over mTLS
make smoke-integration      # Gibson dispatches a tool through Setec (requires Phase D wiring)

# Tear down:
make down
```

## Prerequisites

- Debian or Ubuntu host (tested on Debian 12)
- Root or `sudo` for k3s install
- `/dev/kvm` accessible to your user (you must be in the `kvm` group: `sudo usermod -aG kvm $USER && newgrp kvm`)
- CPU with `vmx` (Intel) or `svm` (AMD) virtualisation extensions
- `docker`, `kubectl`, `helm`, `gh`, `openssl`, `jq` on `PATH`
- Existing Gibson Kind cluster (only required for `smoke-cross-cluster` and `smoke-integration`)

## Detailed walk-through

### Phase 0 — Preflight

`make up` first runs `scripts/00-preflight.sh`, which checks every prerequisite and exits with a clear error if anything is missing. Run it standalone first if you want to confirm before `make up` starts mutating anything:

```bash
./scripts/00-preflight.sh
```

### Phase 1 — k3s and the disk registry

`scripts/10-install-k3s.sh` installs k3s as a single-node systemd unit with Traefik disabled (we ship our own ingress nothing for this dev cluster) and exports a kubeconfig at `kubeconfig/`, with the API server URL rewritten to your host's primary LAN IP so the kubeconfig works from the Kind cluster's container network too.

`scripts/20-install-disk-registry.sh` starts a local OCI registry on the host LAN IP, port 5000, and writes a k3s `registries.yaml` entry so containerd pulls from it over HTTP. The disk builder pushes each signed image disk there, and each launcher Pod mounts its disk as an image volume. k3s is 1.35 or later, because image volumes need it.

`scripts/40-install-setec.sh` makes a dev ed25519 disk signing key under `pki/`, stores its seed in the Secret `setec-disk-signing`, and passes the public key to the chart. The KVM device plugin of the chart offers `/dev/kvm` and `/dev/net/tun` to the launcher Pods.

### Phase 2 — Setec install

`scripts/30-install-spire.sh` installs SPIRE (the hardened charts, at the versions that the platform pins) with the trust domain `dev.local`. setec has one credential source, the SPIFFE Workload API: the frontend, the node-agent and the operator take their SVIDs from the SPIRE agent on the node. The script then runs `scripts/35-mint-client-svid.sh`, which mints the SVID of the dev client `spiffe://dev.local/ns/gibson/sa/gibson-daemon` into `pki/` (`client.crt`, `client.key` and the trust bundle `ca.crt`). An SVID lives a few hours, so each smoke script mints a fresh one.

`scripts/40-install-setec.sh` creates two namespaces:
- `setec-system` (privileged PSS) — Setec operator + frontend
- `gibson-dev` (the tenant namespace) — labeled `setec.zeroroot.ai/client=dev` and `setec.zeroroot.ai/tenant=gibson-dev`. The dev client SVID carries the SPIFFE ID of the enrolled client `dev`, and a request with tenant `gibson-dev` resolves to this namespace.

It then runs `helm upgrade --install setec ../../charts/setec -f values-local.yaml`. A NodePort wrapper Service is applied separately at `manifests/setec-nodeport.yaml` (the Setec chart does not yet expose a NodePort knob; we keep this concern out of the chart and bolt it on via a sibling Service in dev only).

`make smoke-setec` invokes the existing `examples/ai-code-exec` client from your host with the dev client SVID, and checks the SPIFFE ID of the frontend instead of a hostname. It launches a `python:3.12-slim` sandbox printing `hello from microvm`, asserts exit code 0.

### Phase 3 — Cross-cluster reachability

The Gibson Kind cluster needs to dial `host.docker.internal:30051` to reach Setec on k3s. This requires `extraHosts: host-gateway` in the Kind cluster config. The change is one line:

```yaml
# helm/kind-config.yaml in the zeroroot-ai/charts repo
nodes:
  - role: control-plane
    extraHosts:                         # <-- add this block
      - host-gateway                    # <-- add this line
    # ... existing kubeadmConfigPatches, extraPortMappings ...
```

Apply by re-creating the cluster:

```bash
# Run from a checkout of zeroroot-ai/charts.
kind delete cluster --name=gibson
kind create cluster --config helm/kind-config.yaml --name gibson
```

> **CLAUDE.md compliance:** this patch is documented but not auto-applied. GitOps-driven; you apply it.

After the Kind cluster has `host-gateway`, `make smoke-cross-cluster` applies the dev client SVID as a Secret to the Gibson namespace and runs a tiny Job that dials Setec end-to-end. **Zero Gibson code involved** — this isolates the network/auth path from any Gibson integration.

### Phase 4 — Gibson integration

`make smoke-integration` pulls the published `gibson-executor` image, imports it into k3s containerd, applies the smoke Job in `manifests/gibson-kind/`, and asserts the response the `hello` tool returns over the tool-call gRPC. The Gibson daemon must already run with sandboxed tool execution enabled and its tenant set; the Gibson chart in the `zeroroot-ai/charts` repo carries those values. The Sandbox CR lifecycle in `gibson-dev` namespace is verified, and the Jaeger trace ID is printed for manual verification of the `harness.CallToolProto → setec.launch → setec.wait` span tree.

## What lives where

```
development/k3s/
├── Makefile                         # one-command-per-phase entry points
├── README.md                        # this file
├── .gitignore                       # excludes pki/ and *.generated.yaml
├── values-local.yaml                # Setec chart overlay for single-node dev
├── manifests/
│   ├── setec-nodeport.yaml          # NodePort wrapper Service (port 30051)
│   └── gibson-kind/
│       ├── setec-client-tls.yaml.tpl   # template; bash wrapper substitutes the SVID bytes
│       └── setec-smoke-job.yaml     # cross-cluster smoke Job (Phase 3)
├── pki/                             # dev client SVID + disk signing key (gitignored)
├── kubeconfig                       # k3s kubeconfig (gitignored)
└── scripts/                         # numbered for ordering
    ├── 00-preflight.sh
    ├── 10-install-k3s.sh
    ├── 20-install-disk-registry.sh
    ├── 30-install-spire.sh
    ├── 35-mint-client-svid.sh
    ├── 40-install-setec.sh
    ├── 60-smoke-setec.sh
    ├── 65-smoke-cross-cluster.sh
    ├── 70-smoke-integration.sh
    └── 99-uninstall.sh
```

## Cleanup

`make down` runs `scripts/99-uninstall.sh` which:
1. `helm uninstall setec` (best-effort)
2. Runs `/usr/local/bin/k3s-uninstall.sh` (the official k3s uninstaller)
3. Stops the disk registry container
4. Removes `pki/`, `kubeconfig` and `disk-repo` from the working tree

After `make down` the host is in its pre-install state and the Gibson Kind cluster is unaffected.

## Known dev-only deviations from EKS topology

| Concern         | EKS (`setec-eks-dev-env`)            | Local k3s (this directory)              |
|-----------------|--------------------------------------|------------------------------------------|
| Cluster nodes   | dedicated bare-metal sandbox pool    | single shared node                       |
| Operator schedu | system pool (taint-isolated)         | co-located on the only node              |
| TLS material    | SPIRE-issued                         | SPIRE-issued, trust domain `dev.local`   |
| Frontend expose | LB + DNS + Let's Encrypt             | NodePort 30051 (no DNS, no public TLS)   |
| Tenancy         | per-customer namespace               | single tenant `gibson-dev`               |

These are all explicit dev-only simplifications; production patterns live in the EKS spec.
