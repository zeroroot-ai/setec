<!-- SPDX-License-Identifier: Apache-2.0 -->
# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog 1.1.0](https://keepachangelog.com/en/1.1.0/), and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.105.0](https://github.com/zeroroot-ai/setec/compare/v0.104.3...v0.105.0) (2026-06-25)


### ⚠ BREAKING CHANGES

* **api:** import path github.com/zeroroot-ai/setec/api/grpc/v1alpha1 → .../api/grpc/v1; proto package setec.v1alpha1 → setec.v1. Intentional per the open-core relayout (ADR-0027 wholesale-flip).

### Features

* **api:** graduate SandboxService v1alpha1 → v1 (WIRE-stable, no datastore bindings) ([#70](https://github.com/zeroroot-ai/setec/issues/70)) ([8137952](https://github.com/zeroroot-ai/setec/commit/81379529086960002b45f3cbf10ab18b6cf354c8)), closes [#64](https://github.com/zeroroot-ai/setec/issues/64)
* **frontend:** warm-pool lease layer over SandboxService + Snapshot ([#71](https://github.com/zeroroot-ai/setec/issues/71)) ([9e7242f](https://github.com/zeroroot-ai/setec/commit/9e7242ffbb194e1ac028e6b39df9a652e801792f))
* **k3s:** consume the published gibson-executor image (setec[#62](https://github.com/zeroroot-ai/setec/issues/62)) ([#95](https://github.com/zeroroot-ai/setec/issues/95)) ([1a6319d](https://github.com/zeroroot-ai/setec/commit/1a6319d916af7e61c35676b1225c92ceeec05395))
* **node-agent:** reap orphaned kata sandboxes whose microVM leaked on teardown ([#90](https://github.com/zeroroot-ai/setec/issues/90)) ([90b4266](https://github.com/zeroroot-ai/setec/commit/90b426661f4da0ee01889fd403a40a0f86c4d791)), closes [#86](https://github.com/zeroroot-ai/setec/issues/86)
* **snapshot:** attach virtio-rng entropy device for snapshot RNG-safety ([#66](https://github.com/zeroroot-ai/setec/issues/66)) ([#74](https://github.com/zeroroot-ai/setec/issues/74)) ([5a326aa](https://github.com/zeroroot-ai/setec/commit/5a326aa3745772e5c7ee0673c06fc06797c50cf5))
* **snapshot:** no-secrets-in-snapshot gate + default-deny egress per SandboxClass ([#73](https://github.com/zeroroot-ai/setec/issues/73)) ([9c7a42a](https://github.com/zeroroot-ai/setec/commit/9c7a42a3683c02f42b969787587f8dd17bbf6b31)), closes [#66](https://github.com/zeroroot-ai/setec/issues/66)


### Bug Fixes

* **chart:** reject nodeCapabilitiesMode=static at render time ([#98](https://github.com/zeroroot-ai/setec/issues/98)) ([aec3342](https://github.com/zeroroot-ai/setec/commit/aec3342f722e36d676a66f341fb53bea5cb23375))
* **dev/k3s:** install containerd-shim-runsc-v1 for the gvisor runtime ([#92](https://github.com/zeroroot-ai/setec/issues/92)) ([83aa793](https://github.com/zeroroot-ai/setec/commit/83aa793783e433d256c7a38847fd97e6e7ace27d)), closes [#89](https://github.com/zeroroot-ai/setec/issues/89)
* don't stamp pod overhead for externally-managed RuntimeClasses ([#97](https://github.com/zeroroot-ai/setec/issues/97)) ([2e932d3](https://github.com/zeroroot-ai/setec/commit/2e932d362910bd86e33fbeb2f1004b1ad339ecf5)), closes [#78](https://github.com/zeroroot-ai/setec/issues/78)
* **e2e:** don't let the chart fight kata-deploy for the kata-fc RuntimeClass ([#85](https://github.com/zeroroot-ai/setec/issues/85)) ([cbb708e](https://github.com/zeroroot-ai/setec/commit/cbb708eebd96d688d2e306f5ba3693c849ace727))
* **e2e:** isolate runs + fix operator-pod selector for runtime-agent ([#87](https://github.com/zeroroot-ai/setec/issues/87)) ([eaa4daf](https://github.com/zeroroot-ai/setec/commit/eaa4daf9553470ae02082d504f7a89027690b609))
* make k3s dev-env bring-up and runtime-agent probe work end-to-end ([#75](https://github.com/zeroroot-ai/setec/issues/75)) ([8880353](https://github.com/zeroroot-ai/setec/commit/888035384069ee48234d319b011e2ba1cfa2e256))
* remove the never-implemented "static" node-capabilities mode ([#96](https://github.com/zeroroot-ai/setec/issues/96)) ([1540761](https://github.com/zeroroot-ai/setec/commit/1540761ab1dec57ed4bc9ca9b677e3e71a31205c))
* route base images through mirror, digest-pin, pin toolchain to 1.26.4 ([#67](https://github.com/zeroroot-ai/setec/issues/67)) ([a5b0ce4](https://github.com/zeroroot-ai/setec/commit/a5b0ce42dbadbd1d9b8e887a1338b98bf19f6316)), closes [#61](https://github.com/zeroroot-ai/setec/issues/61)
* **webhook:** complete serving-cert wiring + enable the admission webhook in e2e ([#93](https://github.com/zeroroot-ai/setec/issues/93)) ([c183e6f](https://github.com/zeroroot-ai/setec/commit/c183e6f72067d776a9f351e27838f93339d96844))

## [0.104.3](https://github.com/zeroroot-ai/setec/compare/v0.104.2...v0.104.3) (2026-05-26)


### Bug Fixes

* **deps:** update ast-checks to v0.1.2 at new module path github.com/zeroroot-ai/ast-checks ([#54](https://github.com/zeroroot-ai/setec/issues/54)) ([0e7395f](https://github.com/zeroroot-ai/setec/commit/0e7395f455500eda0e40eabbb9fd13cf2809a2d1))

## [0.104.2](https://github.com/zero-day-ai/setec/compare/v0.104.1...v0.104.2) (2026-05-24)


### Bug Fixes

* **ci:** add actions:read to images.yml — resolves startup_failure ([#48](https://github.com/zero-day-ai/setec/issues/48)) ([0c42217](https://github.com/zero-day-ai/setec/commit/0c422172ba0db1db3da4d41283b1c364f73a5c35)), closes [#40](https://github.com/zero-day-ai/setec/issues/40)
* **ci:** remove PR trigger and use security-extended for CodeQL ([#51](https://github.com/zero-day-ai/setec/issues/51)) ([2b80ea2](https://github.com/zero-day-ai/setec/commit/2b80ea2b515465d7ebaefb17f8862b3498d35f8f)), closes [#50](https://github.com/zero-day-ai/setec/issues/50)

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
