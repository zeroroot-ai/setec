<!-- SPDX-License-Identifier: Apache-2.0 -->
# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog 1.1.0](https://keepachangelog.com/en/1.1.0/), and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.109.0](https://github.com/zeroroot-ai/setec/compare/v0.108.0...v0.109.0) (2026-08-12)


### Features

* **api:** sandbox lifecycle ephemeral|session with durable per-session CSI workspace ([#197](https://github.com/zeroroot-ai/setec/issues/197)) ([6f16b5f](https://github.com/zeroroot-ai/setec/commit/6f16b5fce4b575e1a7d824e030bc4241b80d7299)), closes [#192](https://github.com/zeroroot-ai/setec/issues/192)
* **chart:** portable installer DaemonSet for Kata+FC+devmapper node prep ([#199](https://github.com/zeroroot-ai/setec/issues/199)) ([f883e0c](https://github.com/zeroroot-ai/setec/commit/f883e0c68206ea40b8a8faf4f901bd769628bed0)), closes [#187](https://github.com/zeroroot-ai/setec/issues/187)
* **ci:** publish amd64-only images and gate sandbox components to x86 nodes ([#196](https://github.com/zeroroot-ai/setec/issues/196)) ([6d2876e](https://github.com/zeroroot-ai/setec/commit/6d2876e53d770e7c4b263b0d4c48fb6a54e9b2de))
* **class:** declarative pre-warm snapshot pool (PreWarmPoolSize/PreWarmImage/PreWarmTTL) ([#200](https://github.com/zeroroot-ai/setec/issues/200)) ([8e62f60](https://github.com/zeroroot-ai/setec/commit/8e62f60dd5db597c18233ab423348ca016b2cb4a))
* **credentials:** add SPIFFE mode to the frontend, with peer authorization ([#182](https://github.com/zeroroot-ai/setec/issues/182)) ([50c282c](https://github.com/zeroroot-ai/setec/commit/50c282c9a9108bc2f349f7cf62ef9a951939dd38)), closes [#172](https://github.com/zeroroot-ai/setec/issues/172)
* **credentials:** own mTLS credential acquisition in one module ([#176](https://github.com/zeroroot-ai/setec/issues/176)) ([e29929c](https://github.com/zeroroot-ai/setec/commit/e29929ce1666c37fecd3b2af63181a1b02cd1f7f)), closes [#170](https://github.com/zeroroot-ai/setec/issues/170)
* **credentials:** route the client dialers through the credential module and authorize the server SPIFFE ID ([#179](https://github.com/zeroroot-ai/setec/issues/179)) ([808570e](https://github.com/zeroroot-ai/setec/commit/808570e0de98c99758cd2d385af3681b5f542e40))
* **credguard:** fail the build on an mTLS credential built outside the module ([#184](https://github.com/zeroroot-ai/setec/issues/184)) ([6318bb1](https://github.com/zeroroot-ai/setec/commit/6318bb118f742768ae600b52e053f106bcf1ed89)), closes [#175](https://github.com/zeroroot-ai/setec/issues/175)
* **frontend:** session reattach by handle and active-session eviction exemption ([#203](https://github.com/zeroroot-ai/setec/issues/203)) ([73a0bf5](https://github.com/zeroroot-ai/setec/commit/73a0bf5dfb83aa04c1e402d7f5841345d0a10bfc)), closes [#193](https://github.com/zeroroot-ai/setec/issues/193)
* **node-agent:** route server mTLS through the credential module and add SPIFFE mode ([#178](https://github.com/zeroroot-ai/setec/issues/178)) ([e142f64](https://github.com/zeroroot-ai/setec/commit/e142f647afaebffee9534bb21ac491dfc164e769))
* **session:** memory checkpoints on S3-compatible storage with suspend-idle and resume-on-drain ([#205](https://github.com/zeroroot-ai/setec/issues/205)) ([2fbddbb](https://github.com/zeroroot-ai/setec/commit/2fbddbb2a9319104c8b0ec502700b3114ec65a07)), closes [#194](https://github.com/zeroroot-ai/setec/issues/194)
* **snapshot:** encrypt snapshots at rest with per-pool keys and enforce template provenance ([#201](https://github.com/zeroroot-ai/setec/issues/201)) ([77cd5ff](https://github.com/zeroroot-ai/setec/commit/77cd5ff1e112a865a33476d578a4895a0fbfa23e)), closes [#190](https://github.com/zeroroot-ai/setec/issues/190)
* **snapshot:** fail-closed invariant gate for warm-start and resume outside dev ([#207](https://github.com/zeroroot-ai/setec/issues/207)) ([1fecc2f](https://github.com/zeroroot-ai/setec/commit/1fecc2f873c7ce3b81d90c8bbe43e8867f35bcec)), closes [#191](https://github.com/zeroroot-ai/setec/issues/191)
* **snapshot:** per-restore uniquification — vsock CID, network identity, machine/boot-id, fail-closed verify ([#204](https://github.com/zeroroot-ai/setec/issues/204)) ([877e0d5](https://github.com/zeroroot-ai/setec/commit/877e0d5f953544a7a2429279e5cb8e4885f7e94d)), closes [#189](https://github.com/zeroroot-ai/setec/issues/189)
* **snapshot:** record the secret-scan verdict in pool entries as the clean-base attestation ([#212](https://github.com/zeroroot-ai/setec/issues/212)) ([b80117c](https://github.com/zeroroot-ai/setec/commit/b80117cf829cd98c1f905173edd5bb64c8c3550f)), closes [#206](https://github.com/zeroroot-ai/setec/issues/206)


### Bug Fixes

* **build:** tidy go.mod after aws-sdk-go-v2 became a direct dependency ([#208](https://github.com/zeroroot-ai/setec/issues/208)) ([6828836](https://github.com/zeroroot-ai/setec/commit/6828836d89ea34f9eedc87acd94380755f84e600))
* **operator:** enforce SandboxClass maxPauseDuration on suspended sandboxes ([#214](https://github.com/zeroroot-ai/setec/issues/214)) ([8bbbe16](https://github.com/zeroroot-ai/setec/commit/8bbbe1625b6fef5bdd3ebe9640851f74d9354ef4)), closes [#202](https://github.com/zeroroot-ai/setec/issues/202)
* **packer:** rebake eks-kata-fc AMI on x86 and fetch kata .tar.zst payloads ([#213](https://github.com/zeroroot-ai/setec/issues/213)) ([c479eb0](https://github.com/zeroroot-ai/setec/commit/c479eb0bc4e4508710ef6e746310dadd0a6ca8d2))

## [0.108.0](https://github.com/zeroroot-ai/setec/compare/v0.107.0...v0.108.0) (2026-08-08)


### ⚠ BREAKING CHANGES

* the SetecRuntimes Node condition is no longer written. Anything reading it must read the setec.zeroroot.ai/runtime-probe annotation instead; the JSON body is identical. The setec.zeroroot.ai/runtime.<backend> labels are unchanged.
* an egress-allow-list entry naming a host now permits only the addresses that host resolves to, where it previously permitted every address on the declared port. A workload relying on the old behaviour will lose egress to destinations it was never declared to reach. An entry whose host does not resolve from the operator loses its rule entirely; pin it with allow[].cidr.
* the chart's kubeVersion floor moves from 1.28 to 1.30. ValidatingAdmissionPolicy reached admissionregistration.k8s.io/v1 in 1.30, and rendering the guard conditionally on the API version would make containment depend on which cluster happened to install the chart.

### Features

* deny host-namespace and host-path Pods in Sandbox namespaces ([#163](https://github.com/zeroroot-ai/setec/issues/163)) ([dd51f1d](https://github.com/zeroroot-ai/setec/commit/dd51f1de8eacb77c4d9f8f44e68690c6a049b70f))


### Bug Fixes

* drop the runtime-agent's node/status grant and narrow its node writes ([#164](https://github.com/zeroroot-ai/setec/issues/164)) ([8003696](https://github.com/zeroroot-ai/setec/commit/8003696bceb7b834098225cd0f9771b95feaf08f))
* gate devOnly backends out of the cluster runtime defaults ([#165](https://github.com/zeroroot-ai/setec/issues/165)) ([f3e35c1](https://github.com/zeroroot-ai/setec/commit/f3e35c14285e991ee699de2c7517be05f7f4f8f5))
* resolve declared egress hosts instead of permitting the whole internet on their port ([#166](https://github.com/zeroroot-ai/setec/issues/166)) ([a625609](https://github.com/zeroroot-ai/setec/commit/a6256096ecad8b79fa9453c2a1ba47f9e11e6ae7))

## [0.107.0](https://github.com/zeroroot-ai/setec/compare/v0.106.0...v0.107.0) (2026-08-06)


### ⚠ BREAKING CHANGES

* `NetworkMode` no longer accepts `full`. The enum is now `external-only`, `egress-allow-list`, `none`, and `Network.mode` defaults to `none`. `SandboxClass.spec.defaultNetworkMode` drops `full` likewise, and an unset value now resolves to `none` rather than to unrestricted egress. Callers relying on the previous default must select a SandboxClass whose `defaultNetworkMode` is `external-only`, or declare `spec.network` explicitly. Chart value `defaultClass` is replaced by `sandboxClasses`.

### Features

* make sandbox egress default-deny and scope the allow-list to declared destinations ([#157](https://github.com/zeroroot-ai/setec/issues/157)) ([6103192](https://github.com/zeroroot-ai/setec/commit/6103192a2ce1d153e507e8ea8b1bbffa65d13951))

## [0.106.0](https://github.com/zeroroot-ai/setec/compare/v0.105.0...v0.106.0) (2026-07-04)


### Features

* **ci:** migrate e2e onto ephemeral ARC runner, drop self-hosted metal ([#114](https://github.com/zeroroot-ai/setec/issues/114)) ([3a678b8](https://github.com/zeroroot-ai/setec/commit/3a678b84d0b26b93e36e2a39a4a0c3b9c063a987))
* entropy reseed on snapshot restore via in-guest vsock agent ([#128](https://github.com/zeroroot-ai/setec/issues/128)) ([38e9d08](https://github.com/zeroroot-ai/setec/commit/38e9d089cb852090494823bc499b39ea30e055f7))
* **helm:** Karpenter scale-to-zero NodePool for the baked kata metal AMI ([#127](https://github.com/zeroroot-ai/setec/issues/127)) ([8c170c1](https://github.com/zeroroot-ai/setec/commit/8c170c1951886c9dc82afa44ef887beffd5d952b)), closes [#77](https://github.com/zeroroot-ai/setec/issues/77)
* packer-baked kata/Firecracker Graviton-metal EKS AMI (no kata-deploy) ([#125](https://github.com/zeroroot-ai/setec/issues/125)) ([2fed608](https://github.com/zeroroot-ai/setec/commit/2fed60851a07c9719e0b6e472f07868f4f288f85)), closes [#76](https://github.com/zeroroot-ai/setec/issues/76)
* propagate SandboxClass tolerations to Sandbox pods ([#116](https://github.com/zeroroot-ai/setec/issues/116)) ([6b97eba](https://github.com/zeroroot-ai/setec/commit/6b97eba81e81a1680d0bd29d00db47bf76b0817e)), closes [#115](https://github.com/zeroroot-ai/setec/issues/115)


### Bug Fixes

* **chart:** default component tags to AppVersion + publish runtime-agent image ([#104](https://github.com/zeroroot-ai/setec/issues/104)) ([ace9263](https://github.com/zeroroot-ai/setec/commit/ace9263b87cfd3905a45b747132dafb2d8e094c2)), closes [#81](https://github.com/zeroroot-ai/setec/issues/81)
* **chart:** retired bitnami kubectl default + runtimeAgent.nodeSelector ([#133](https://github.com/zeroroot-ai/setec/issues/133)) ([e5fe948](https://github.com/zeroroot-ai/setec/commit/e5fe948a818f0d994d4f8926a0f5c79b89d12942)), closes [#129](https://github.com/zeroroot-ai/setec/issues/129) [#132](https://github.com/zeroroot-ai/setec/issues/132)
* **ci:** stop merge_group heavy go-ci and CodeQL red checks ([#118](https://github.com/zeroroot-ai/setec/issues/118)) ([d42a3e6](https://github.com/zeroroot-ai/setec/commit/d42a3e6eee6acbddffed5404732f88e717869a76))
* **deps:** bump containerd to v2.3.2 (GO-2026-5758) ([#120](https://github.com/zeroroot-ai/setec/issues/120)) ([c1154d9](https://github.com/zeroroot-ai/setec/commit/c1154d9d3a17a32d4f15265d86dfe983eab416f3))
* **e2e:** accept Succeeded phase in waitForPodReady + poll port-forward readiness ([#101](https://github.com/zeroroot-ai/setec/issues/101)) ([68b5850](https://github.com/zeroroot-ai/setec/commit/68b5850af5d4aacf23a2f3e80ca2f658b0e3a01c)), closes [#94](https://github.com/zeroroot-ai/setec/issues/94)
* **helm:** make pre-upgrade runtime-check hook resilient to slow image pulls ([#100](https://github.com/zeroroot-ai/setec/issues/100)) ([d3d6dc3](https://github.com/zeroroot-ai/setec/commit/d3d6dc335347e07dc503b90b50e1edfeccfb8280)), closes [#80](https://github.com/zeroroot-ai/setec/issues/80)

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
