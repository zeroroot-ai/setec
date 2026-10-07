<!-- SPDX-License-Identifier: Apache-2.0 -->
# Setec Documentation

Setec is a Kubernetes-native operator that runs each workload in its own Firecracker microVM, one machine in each launcher Pod, cloud-agnostic and self-hostable. For the 30-second pitch, the install command, and a short example, see the [project README](../README.md).

This page is the hub. Every doc in this directory is linked below, grouped by what you are trying to do.

## Getting Started

- [Quickstart](./quickstart.md) &mdash; terse command list. Install, run one Sandbox, tear it down.
- [Getting Started](./getting-started.md) &mdash; the same territory as the quickstart but narrative, with prose explaining what is happening at each step and what you should observe.
- [Prerequisites](./prerequisites.md) &mdash; KVM, kernel, registry and Kubernetes requirements.

## Design

- [Architecture](./architecture.md) &mdash; the parts of `setec`, its objects, and the path of one Sandbox.
- [Isolation](./design/isolation.md) &mdash; the Pod, the network, the namespace, and the checks on a restored Sandbox.
- [Lifecycles](./design/lifecycles.md) &mdash; ephemeral and session Sandboxes, idle eviction, suspend, limits, and the warm pool.
- [Storage](./design/storage.md) &mdash; the scratch volume, the session workspace, the snapshot store, and the encryption rule of each.
- [Runtime](./design/runtime.md) &mdash; the launcher runtime, its parts, and the node preparation.
- [Threat model](./design/threat-model.md) &mdash; what the machine boundary protects, and who may call `setec`.

## User Guides

- [Multi-tenancy](./multitenancy.md) &mdash; tenant labels, per-tenant policies, namespace scoping.
- [Snapshots](./snapshots.md) &mdash; point-in-time capture, restore, fork and pause/resume.
- [Observability](./observability.md) &mdash; metrics, traces, dashboard, and alerting.
- [gRPC Frontend API](./frontend-api.md) &mdash; the external API used by programmatic consumers.
- [Node Agent](./node-agent.md) &mdash; what runs on each node and how it interacts with the operator.

## Reference

- [CRD Reference](./crd-reference.md) &mdash; schema documentation for `Sandbox`, `SandboxClass`, and `Snapshot`.

## Operations

- [Prerequisites](./prerequisites.md) &mdash; host and cluster requirements.
- [Idle footprint](./idle-footprint.md) &mdash; how to measure the memory of a long-lived Sandbox.
- [Developer Notes](./developer-notes.md) &mdash; contributor-facing naming and layout conventions.

## Community

- [Contributing](../CONTRIBUTING.md) &mdash; dev setup, commit style, DCO, pull request process.
- [Code of Conduct](../CODE_OF_CONDUCT.md) &mdash; Contributor Covenant 2.1.
- [Governance](../GOVERNANCE.md) &mdash; roles, decision-making, escalation.
- [Security Policy](../SECURITY.md) &mdash; how to report vulnerabilities privately.
- [Maintainers](../MAINTAINERS) &mdash; current maintainer roster.

## Examples

Consumer-side example programs live under [`../examples/`](../examples/). They show three representative patterns: running LLM-generated code, running untrusted CI jobs, and running security-research tools.
