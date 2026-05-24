<!-- SPDX-License-Identifier: Apache-2.0 -->
# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog 1.1.0](https://keepachangelog.com/en/1.1.0/), and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.104.1](https://github.com/zero-day-ai/setec/compare/v0.104.0...v0.104.1) (2026-05-24)


### Bug Fixes

* **ci:** tidy examples go.mod and fix modernize lint errors ([#46](https://github.com/zero-day-ai/setec/issues/46)) ([e0041e2](https://github.com/zero-day-ai/setec/commit/e0041e26d9739be7e08121031a12f137e5cee85b))

## [0.104.0](https:\/\/github.com\/zero-day-ai\/setec\/compare\/v0.X.Y...v0.104.0) (2026-05-17)

Polyrepo zero-dot-x reset (PRD zero-day-ai\/.github#25, board #14). The v1.x line was cut prematurely; nothing in the platform is at 1.0 maturity yet. The v1.0.0 tag + release has been deleted; this repo lands at the polyrepo-wide v0.104.0 marker. Going forward, `bump-minor-pre-major: true` ensures `feat!:` commits bump minor not major.
## [0.2.0](https://github.com/zero-day-ai/setec/compare/v0.1.0...v0.2.0) (2026-05-10)


### Features

* migrate setec to controller-runtime events API (drop SA1019 nolints) ([#17](https://github.com/zero-day-ai/setec/issues/17)) ([8b3119a](https://github.com/zero-day-ai/setec/commit/8b3119aff51a4ea09a4e9b4de2d9380ae084d7e0))

## [0.1.0](https://github.com/zero-day-ai/setec/compare/v0.0.2...v0.1.0) (2026-05-10)


### Features

* install release-please and pr-title-lint ([#13](https://github.com/zero-day-ai/setec/issues/13)) ([b092d9f](https://github.com/zero-day-ai/setec/commit/b092d9f3022bb4e3c773d8a957d960455feadd70))

## [Unreleased]

Nothing yet. Open a pull request to append.

## [0.1.0] - YYYY-MM-DD

First public release. Phases 1, 2, and 3 ship together as v0.1.0. See [`docs/launch/v0.1.0-release-notes.md`](./docs/launch/v0.1.0-release-notes.md) for the full release announcement.

### Added (Phase 1: MVP)

- `Sandbox` CRD (`setec.zero-day.ai/v1alpha1`) describing a microVM workload.
- `setec-operator` (Kubebuilder-scaffolded controller manager) that reconciles `Sandbox` resources into pods with the `kata-fc` `RuntimeClass`.
- `SandboxReconciler` with phase transitions (Pending -> Running -> Completed/Failed/TimedOut).
- Pure `podspec.Build` translator, `status.Derive` phase deriver, `prereq.Check` cluster pre-flight.
- Minimal RBAC (`ClusterRole` + `ClusterRoleBinding`) generated via controller-gen.
- Helm chart (`charts/setec/`) with CRD, RBAC, and hardened operator `Deployment`.
- End-to-end suite (`test/e2e/`) covering six Sandbox scenarios on a Kata-capable host.
- Documentation: README, quickstart, CRD reference, prerequisites, manual smoke-test checklist.
- CI workflows (`ci.yml`, `release.yml`) with lint, test, manifests, helm, docker, signed multi-arch image build on tag.

### Added (Phase 2: multi-tenancy and observability)

- `SandboxClass` CRD with constraint and policy fields, plus its reconciler and validator.
- Tenant identity extraction from namespace labels and mTLS client certificates (`internal/tenancy`).
- NetworkPolicy translator (`internal/netpol`) emitting per-Sandbox NetworkPolicies matching the declared network intent.
- Prometheus metrics (`setec_sandbox_total`, `setec_sandbox_duration_seconds`, `setec_sandbox_cold_start_seconds`, `setec_sandbox_active`) and OTEL tracing (`setec-operator` service name).
- Validating admission webhook (`internal/webhook`) enforcing tenant-label presence, class constraints, and network mode.
- `node-agent` DaemonSet providing devicemapper thin-pool management, image cache, and per-node metrics.
- gRPC frontend (`cmd/frontend`) implementing `setec.v1alpha1.SandboxService` (Launch / StreamLogs / Wait / Kill) with mTLS and tenant scoping.
- Helm chart additions: DaemonSet, Frontend Deployment + Service, webhook `ValidatingWebhookConfiguration`, default `SandboxClass`.
- Phase 2 E2E scenarios, CNI detection step, and dev smoke-test checklist additions.
- Phase 2 docs: multi-tenancy, observability, node-agent, frontend API.

### Added (Phase 3: snapshots, pause/resume, pre-warm pool)

- `Snapshot` CRD (`setec.zero-day.ai/v1alpha1`) with finalizer, TTL, and ref-count semantics.
- `Sandbox.spec.snapshot`, `Sandbox.spec.snapshotRef`, `Sandbox.spec.desiredState` fields.
- `SandboxClass.spec.preWarmPoolSize`, `PreWarmImage`, `PreWarmTTL` fields.
- `NodeAgentService` gRPC (operator to node-agent) for snapshot create/restore/pause/resume.
- Narrow Firecracker HTTP-over-Unix-socket client (`internal/firecracker`).
- `StorageBackend` interface and `LocalDiskBackend` with SHA256 integrity and fill-threshold protection.
- Snapshot orchestrator (`internal/snapshot.Coordinator`) and pure `internal/snapshot.Validator`.
- `internal/nodeagent/pool.Manager` with TTL-based recycling, Claim, Release, Query, and ReconcilePools.
- `SnapshotReconciler` finalizer, TTL enforcement, refcount tracking; Sandbox reconciler extensions for Phase 3 branches.
- Admission extensions for `snapshotRef` and standalone `Snapshot` resources.
- Node-agent main wiring for gRPC server, storage backend, and pool.
- Phase 3 docs: snapshots, Kata + Firecracker integration.
- Phase 3 E2E scenarios (previously-skipped scenarios un-skipped in Phase 4 once the launcher lands).

### Added (Phase 4: launch readiness)

- Community documents: `CONTRIBUTING.md`, `CODE_OF_CONDUCT.md`, `SECURITY.md`, `GOVERNANCE.md`, `MAINTAINERS`, this `CHANGELOG.md`.
- GitHub issue forms (bug report, feature request), security redirect, PR template, `CODEOWNERS`.
- Hand-authored logo (`docs/assets/logo.svg`) with PNG renders at 32/128/512, favicon, and 1280x640 social preview.
- Documentation hub (`docs/README.md`), narrative getting-started tutorial (`docs/getting-started.md`), and developer-notes naming conventions.
- Three example consumer programs (`examples/ai-code-exec`, `examples/ci-sandbox`, `examples/sec-research`), each with its own Go module and `kubectl apply`-able manifest.
- Grafana dashboard (`charts/setec/grafana/setec-operator.json`) and Prometheus recording rules + alerts (`charts/setec/prometheus/*.yaml`).
- `setec-pool-vm` companion binary (`cmd/setec-pool-vm/`) and pool.Launcher/ExecLauncher wiring for real pre-warm pool VM boots.
- Pool reconcile tick goroutine in the node-agent (configurable via `--pool-reconcile-interval`, default 30s).
- Supply-chain workflows: CodeQL (push/PR/weekly), OSSF Scorecard (weekly), Dependabot config for gomod/actions/docker and each example.
- REUSE-compliance baseline (`LICENSES/Apache-2.0.txt`, `.reuse/dep5`) and SPDX headers on Phase 4 markdown.
- v0.1.0 launch content: release notes draft, smoke-test result template, HN post draft, blog post draft, tweet thread, README tagline audit.

### Known Limitations

- Requires Kata Containers with the Firecracker runtime on every worker node. `kata-deploy` is the supported installer.
- Nodes must expose `/dev/kvm`. Nested virtualization works if the outer hypervisor permits it.
- The `kata-fc` `RuntimeClass` is a hard prerequisite; the operator will start without it but `Sandbox` resources will stay `Pending`.
- Pre-warmed pool cold starts are observed sub-100ms P50 on prepared bare-metal hosts; clusters without KVM will not hit that number.
- The frontend gRPC API is `v1alpha1` and subject to change before `v1`.

[Unreleased]: https://github.com/zero-day-ai/setec/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/zero-day-ai/setec/releases/tag/v0.1.0
