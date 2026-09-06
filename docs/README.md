<!-- SPDX-License-Identifier: Apache-2.0 -->
# Setec Documentation

Setec is a Kubernetes-native operator that runs workloads inside Firecracker microVMs via Kata Containers, cloud-agnostic and self-hostable. For the 30-second pitch, the install command, and a short example, see the [project README](../README.md).

This page is the hub. Every doc in this directory is linked below, grouped by what you are trying to do.

## Getting Started

- [Quickstart](./quickstart.md) &mdash; terse command list. Install, run one Sandbox, tear it down.
- [Getting Started](./getting-started.md) &mdash; the same territory as the quickstart but narrative, with prose explaining what is happening at each step and what you should observe.
- [Prerequisites](./prerequisites.md) &mdash; KVM, kernel, Kata Containers, Firecracker, and Kubernetes requirements on the host.

## User Guides

- [Multi-tenancy](./multitenancy.md) &mdash; tenant labels, per-tenant policies, namespace scoping.
- [Snapshots](./snapshots.md) &mdash; point-in-time capture, restore, and the pre-warm pool.
- [Observability](./observability.md) &mdash; metrics, traces, dashboard, and alerting.
- [gRPC Frontend API](./frontend-api.md) &mdash; the external API used by programmatic consumers.
- [Node Agent](./node-agent.md) &mdash; what runs on each node and how it interacts with the operator.

## Reference

- [CRD Reference](./crd-reference.md) &mdash; schema documentation for `Sandbox`, `SandboxClass`, and `Snapshot`.

## Operations

- [Prerequisites](./prerequisites.md) &mdash; per-backend, per-platform host requirements.
- [Runtime Backends](./runtime-backends/README.md) &mdash; the four backends (`kata-fc`, `kata-qemu`, `gvisor`, `runc`), isolation / CVE-surface / overhead matrix, and managed-K8s playbooks for [EKS](./runtime-backends/eks.md), [AKS](./runtime-backends/aks.md), [GKE](./runtime-backends/gke.md).
- [Kata + Firecracker Integration](./kata-firecracker-integration.md) &mdash; deep dive on the default `kata-fc` backend's internals.
- [Dev Smoke Test](./dev-smoke-test.md) &mdash; the scripted run maintainers perform before tagging a release.
- [Developer Notes](./developer-notes.md) &mdash; contributor-facing naming and layout conventions.

## Community

- [Contributing](../CONTRIBUTING.md) &mdash; dev setup, commit style, DCO, pull request process.
- [Code of Conduct](../CODE_OF_CONDUCT.md) &mdash; Contributor Covenant 2.1.
- [Governance](../GOVERNANCE.md) &mdash; roles, decision-making, escalation.
- [Security Policy](../SECURITY.md) &mdash; how to report vulnerabilities privately.
- [Maintainers](../MAINTAINERS) &mdash; current maintainer roster.

## Examples

Consumer-side example programs live under [`../examples/`](../examples/). They show three representative patterns: running LLM-generated code, running untrusted CI jobs, and running security-research tools.
