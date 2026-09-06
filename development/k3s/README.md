# Local k3s + Kata + Setec dev environment

Single-host bring-up of Setec (with real Firecracker microVMs via Kata Containers) on bare-metal Debian, suitable for local integration testing. Detailed walk-through is in [README full doc](#detailed-walk-through) below.

> **Production this is not.** PKI is a self-signed dev CA, the cluster is single-node, and operator + sandbox workloads co-locate. For production / multi-tenant / scheduled-uptime patterns see `setec-eks-dev-env`.

## TL;DR

```bash
# One-time, on a fresh bare-metal Debian host with KVM:
make up

# Verify each phase:
make smoke-kata             # microVM boots
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

### Phase 1 — k3s + Kata

`scripts/10-install-k3s.sh` installs k3s as a single-node systemd unit with Traefik disabled (we ship our own ingress nothing for this dev cluster) and exports a kubeconfig at `kubeconfig/`, with the API server URL rewritten to your host's primary LAN IP so the kubeconfig works from the Kind cluster's container network too.

`scripts/20-install-kata.sh` installs `kata-deploy` (Helm) into `kube-system`, waits for the DaemonSet, and verifies the `kata-fc` RuntimeClass appears.

`make smoke-kata` runs a one-shot Pod with `runtimeClassName: kata-fc` and asserts the kernel string differs from the host kernel — proving real microVM boot, not silent runc fallback.

### Phase 2 — Setec install

`scripts/30-generate-pki.sh` produces a dev CA under `pki/` plus a server cert (CN `setec-frontend.setec-system.svc`, with SANs covering the LAN IP, `host.docker.internal`, and loopback) and a client cert (CN `gibson-dev`).

`scripts/40-install-setec.sh` creates two namespaces:
- `setec-system` (privileged PSS) — Setec operator + frontend
- `gibson-dev` (the tenant namespace) — labelled `setec.zeroroot.ai/tenant=gibson-dev` so the frontend resolves the `gibson-dev` client CN to it

It then materialises the TLS Secret and runs `helm upgrade --install setec ../../charts/setec -f values-local.yaml`. A NodePort wrapper Service is applied separately at `manifests/setec-nodeport.yaml` (the Setec chart does not yet expose a NodePort knob; we keep this concern out of the chart and bolt it on via a sibling Service in dev only).

`make smoke-setec` invokes the existing `examples/ai-code-exec` client from your host with the dev client cert, launches a `python:3.12-slim` sandbox printing `hello from microvm`, asserts exit code 0.

### Phase 3 — Cross-cluster reachability

The Gibson Kind cluster needs to dial `host.docker.internal:30051` to reach Setec on k3s. This requires `extraHosts: host-gateway` in the Kind cluster config. The change is one line:

```yaml
# enterprise/deploy/helm/gibson/kind-config.yaml
nodes:
  - role: control-plane
    extraHosts:                         # <-- add this block
      - host-gateway                    # <-- add this line
    # ... existing kubeadmConfigPatches, extraPortMappings ...
```

Apply by re-creating the cluster:

```bash
kind delete cluster --name=gibson
make -C enterprise/deploy/helm/gibson kind-create
```

> **CLAUDE.md compliance:** this patch is documented but not auto-applied. GitOps-driven; you apply it.

After the Kind cluster has `host-gateway`, `make smoke-cross-cluster` applies the dev client TLS Secret to the Gibson namespace and runs a tiny Job that dials Setec end-to-end. **Zero Gibson code involved** — this isolates the network/auth path from any Gibson integration.

### Phase 4 — Gibson integration

`make smoke-integration` pulls the published `gibson-executor` image, imports it into k3s containerd, applies the Gibson chart values overlay (`enterprise/deploy/helm/gibson/values-sandboxed-tools.yaml`), waits for the daemon to restart with the new config, invokes the `hello` tool against the daemon's tool-call gRPC, and asserts the response. The Sandbox CR lifecycle in `gibson-dev` namespace is verified, and the Jaeger trace ID is printed for manual verification of the `harness.CallToolProto → setec.launch → setec.wait` span tree.

## What lives where

```
opensource/setec/development/k3s/
├── Makefile                         # one-command-per-phase entry points
├── README.md                        # this file
├── .gitignore                       # excludes pki/ and *.generated.yaml
├── values-local.yaml                # Setec chart overlay for single-node dev
├── manifests/
│   ├── setec-nodeport.yaml          # NodePort wrapper Service (port 30051)
│   └── gibson-kind/
│       ├── setec-client-tls.yaml.tpl   # template; bash wrapper substitutes PKI bytes
│       └── setec-smoke-job.yaml     # cross-cluster smoke Job (Phase 3)
├── pki/                             # dev CA + certs (gitignored)
├── kubeconfig                       # k3s kubeconfig (gitignored)
└── scripts/                         # numbered for ordering
    ├── 00-preflight.sh
    ├── 10-install-k3s.sh
    ├── 20-install-kata.sh
    ├── 30-generate-pki.sh
    ├── 40-install-setec.sh
    ├── 50-smoke-kata.sh
    ├── 60-smoke-setec.sh
    ├── 65-smoke-cross-cluster.sh
    ├── 70-smoke-integration.sh
    └── 99-uninstall.sh
```

## Cleanup

`make down` runs `scripts/99-uninstall.sh` which:
1. `helm uninstall setec` and `helm uninstall kata-deploy` (best-effort)
2. Runs `/usr/local/bin/k3s-uninstall.sh` (the official k3s uninstaller)
3. Removes `pki/` and `kubeconfig` from the working tree

After `make down` the host is in its pre-install state and the Gibson Kind cluster is unaffected.

## Known dev-only deviations from EKS topology

| Concern         | EKS (`setec-eks-dev-env`)            | Local k3s (this directory)              |
|-----------------|--------------------------------------|------------------------------------------|
| Cluster nodes   | dedicated bare-metal sandbox pool    | single shared node                       |
| Operator schedu | system pool (taint-isolated)         | co-located on the only node              |
| TLS material    | SPIRE-issued                         | self-signed dev CA under `pki/`          |
| Frontend expose | LB + DNS + Let's Encrypt             | NodePort 30051 (no DNS, no public TLS)   |
| Tenancy         | per-customer namespace               | single tenant `gibson-dev`               |

These are all explicit dev-only simplifications; production patterns live in the EKS spec.
