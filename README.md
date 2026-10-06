<!-- SPDX-License-Identifier: Apache-2.0 -->
<p align="center">
  <img src="docs/assets/logo-128.png" alt="Setec" width="128" height="128">
</p>

<h1 align="center">Setec</h1>

<p align="center"><strong>microVM isolation as a Kubernetes primitive.</strong></p>

<p align="center">The sandbox layer behind <a href="https://zeroroot.ai">zeroroot.ai</a>, the zero-trust agent factory. Useful standalone.</p>

<p align="center">
  <a href="https://github.com/zeroroot-ai/setec/actions/workflows/ci.yml"><img alt="CI" src="https://img.shields.io/github/actions/workflow/status/zeroroot-ai/setec/ci.yml?branch=main&label=ci"></a>
  <a href="https://github.com/zeroroot-ai/setec/releases"><img alt="Latest release" src="https://img.shields.io/github/v/release/zeroroot-ai/setec?include_prereleases&sort=semver"></a>
  <a href="./LICENSE"><img alt="License: Apache-2.0" src="https://img.shields.io/badge/license-Apache--2.0-blue"></a>
  <a href="https://api.scorecard.dev/projects/github.com/zeroroot-ai/setec"><img alt="OSSF Scorecard" src="https://api.scorecard.dev/projects/github.com/zeroroot-ai/setec/badge"></a>
  <a href="https://github.com/zeroroot-ai/setec/actions/workflows/codeql.yml"><img alt="CodeQL" src="https://img.shields.io/github/actions/workflow/status/zeroroot-ai/setec/codeql.yml?branch=main&label=codeql"></a>
  <img alt="Kubernetes" src="https://img.shields.io/badge/kubernetes-1.35%2B-blue">
</p>

---

Setec is a Kubernetes operator that runs each workload in its own [Firecracker](https://firecracker-microvm.github.io/) microVM. Declare a `Sandbox` custom resource and the operator starts one machine in one launcher Pod for you, complete with lifecycle control, a programmatic gRPC frontend, snapshot / restore, fork, and a warm pool. Cloud-agnostic, self-hostable, Apache 2.0.

> **Status: pre-release / alpha.** The CRD is `v1alpha1`. Breaking changes are possible before `v1`.

## Highlights

- **Single-CRD API.** `kubectl apply -f sandbox.yaml` and you have a sandbox. No separate CLI, no dashboard, no SaaS.
- **One runtime: a Firecracker machine in a launcher Pod.** The launcher Pod is not privileged and mounts nothing from the host. A KVM device plugin offers `/dev/kvm` and `/dev/net/tun` of each node, and the scheduler places a launcher Pod only on a node that offers them. See [`docs/design/runtime.md`](docs/design/runtime.md).
- **Signed image disks.** A disk builder turns each image digest into a squashfs disk and signs it. Each launcher refuses a disk that the install key did not sign.
- **Snapshots, restore, fork and pause/resume.** Capture the state of a machine through the `Snapshot` resource, restore it into a new Sandbox, fork a running Sandbox into several, and pause or resume a live Sandbox. Each restore gets fresh entropy and a new machine identity before it runs (ADR-0145).
- **Warm pool.** A class keeps warm bases: snapshots of a machine that booted the image and ran no workload. A Sandbox of the class loads a base instead of a boot.
- **Per-sandbox identity.** Each Sandbox and each fork gets its own signing key, and the workload asks for a short-lived token that a verifier checks through the frontend.
- **Multi-tenant.** Tenant identity from namespace labels or mTLS; per-Sandbox `NetworkPolicy`; tenant scoping on the gRPC frontend.
- **Observability shipped.** Prometheus metrics and OpenTelemetry traces emitted by default; Grafana dashboard and alert rules ship with the chart.
- **gRPC frontend.** `SandboxService` with mTLS for programmatic consumers. See [examples](examples/).
- **Cloud-agnostic.** Any Kubernetes cluster whose amd64 worker nodes expose `/dev/kvm`.
- **Small surface.** Seven small binaries: the operator, the node agent, the frontend, the KVM device plugin, the disk builder, the launcher, and the in-guest `setec-guest-agent` (the init process of each machine, bundled into the launcher image).

## Quick install

```bash
helm install setec oci://ghcr.io/zeroroot-ai/charts/setec \
  --namespace setec-system \
  --create-namespace \
  --set launcher.diskRepo=registry.example.com/setec-disks \
  --set launcher.diskBuilder.signingSecret=setec-disk-signing \
  --set 'launcher.diskBuilder.publicKeys={<base64 ed25519 public key>}' \
  --set 'sandboxNamespaces={default}'
```

Local install from a checked-out tree: use `./charts/setec` in place of the OCI reference.

Prerequisites: a Kubernetes 1.35+ cluster with **x86-64 (amd64) worker nodes that expose `/dev/kvm`** (bare metal, or a VM with nested virtualization), an OCI registry for the signed image disks, and a disk signing key. arm64 is not supported (docs/design/runtime.md): all setec images are published `linux/amd64` single-arch, and each launcher Pod pins `kubernetes.io/arch=amd64`. [`docs/quickstart.md`](docs/quickstart.md) makes the signing key, and [`docs/prerequisites.md`](docs/prerequisites.md) has the full check-list.

## Why isolation

Containers share a kernel. For trusted workloads that's fine. For workloads you do not trust — code your LLM just generated, a test suite from an outside pull request, a fuzzer you are pointing at a parser — a boundary between the workload and the host kernel is the only honest answer. Each Setec sandbox runs in its own Linux kernel inside a Firecracker virtual machine. Escape requires breaking the guest kernel, then Firecracker, then KVM, then the host kernel.

Setec makes the boundary declarative, reusable, and operable by anyone who already knows Kubernetes.

## Example: a complete Sandbox

```yaml
apiVersion: setec.zeroroot.ai/v1alpha1
kind: Sandbox
metadata:
  name: hello
  namespace: default
spec:
  # python:3.12-slim, by digest: the disk of the machine belongs to one digest.
  image: docker.io/library/python@sha256:02108f5d322dd89f1c9e552442c25acb0543dfdbc455693a5599624f20d9155d
  command:
    - python
    - -c
    - "print('hello from an isolated sandbox')"
  resources:
    vcpu: 1
    memory: 512Mi
  lifecycle:
    timeout: 5m
```

```bash
kubectl apply -f hello.yaml
kubectl get sandbox hello -w
kubectl logs hello-vm
kubectl delete sandbox hello
```

## Next steps

- **New here:** the 15-minute narrative walkthrough in [`docs/getting-started.md`](docs/getting-started.md).
- **In a hurry:** the terse [quickstart](docs/quickstart.md).
- **Writing a consumer:** three reference programs under [`examples/`](examples/) covering AI code execution, CI sandboxing, and security research.
- **Operating a cluster:** the [docs hub](docs/README.md) groups guides, reference, and operations pages.

## Development

Setec follows the standard [kubebuilder](https://kubebuilder.io/) v4 layout. Most workflows are Makefile targets:

```bash
make generate     # regenerate deepcopy code
make manifests    # regenerate CRD manifests
make build        # build the operator and the guest agent
make test         # run unit + envtest suites
make lint         # run golangci-lint
make helm-lint    # lint the Helm chart
make e2e          # E2E suite on real Firecracker machines (requires KVM nodes)
```

Non-trivial changes go through a short design-before-code cycle described in [`CONTRIBUTING.md`](CONTRIBUTING.md).

## Community

- [`CONTRIBUTING.md`](CONTRIBUTING.md) &mdash; dev setup, commit style, DCO, PR process.
- [`CODE_OF_CONDUCT.md`](CODE_OF_CONDUCT.md) &mdash; Contributor Covenant 2.1.
- [`GOVERNANCE.md`](GOVERNANCE.md) &mdash; roles, decision-making, maintainership.
- [`SECURITY.md`](SECURITY.md) &mdash; private vulnerability reporting and response timeline.
- [`CHANGELOG.md`](CHANGELOG.md) &mdash; release history.

## License

Apache 2.0. Full text in [`LICENSE`](LICENSE).

---

The name is a 1990s-movie reference. The goal is not to be cute; it is for hardware-isolated workloads to be boring infrastructure.

## License and history

Apache License 2.0. See [LICENSE](LICENSE). Copyright Zero Root AI.

Issue and pull request numbers cited in comments and documents dated before 2026-09-05 refer to the tracker before the history reset, archived offline. They do not resolve on GitHub.
