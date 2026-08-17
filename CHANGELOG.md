<!-- SPDX-License-Identifier: Apache-2.0 -->
# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog 1.1.0](https://keepachangelog.com/en/1.1.0/), and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.112.2](https://github.com/zeroroot-ai/setec/compare/v0.112.1...v0.112.2) (2026-08-17)


### Bug Fixes

* **e2e:** give every suite SandboxClass the sandbox-host toleration ([#341](https://github.com/zeroroot-ai/setec/issues/341)) ([f71a916](https://github.com/zeroroot-ai/setec/commit/f71a91629f439583ee433db9089767a5567ba9e6))
* **snapshot:** close the three S3 checkpoint failure modes ([#343](https://github.com/zeroroot-ai/setec/issues/343)) ([48f540b](https://github.com/zeroroot-ai/setec/commit/48f540b9d46efdf7ec554c894bf30eb1d23d0d3e))

## [0.112.1](https://github.com/zeroroot-ai/setec/compare/v0.112.0...v0.112.1) (2026-08-16)


### Bug Fixes

* **ci:** run release-please as the zeroday-sdk-fanout App ([#339](https://github.com/zeroroot-ai/setec/issues/339)) ([3cea39b](https://github.com/zeroroot-ai/setec/commit/3cea39b467e481c8814d8aa2a1eac499c6a3331a))

## [0.112.0](https://github.com/zeroroot-ai/setec/compare/v0.111.0...v0.112.0) (2026-08-16)


### Features

* **abi:** add SandboxService.Exec for in-session command execution ([#333](https://github.com/zeroroot-ai/setec/issues/333)) ([cc0e3c7](https://github.com/zeroroot-ai/setec/commit/cc0e3c7e7d01884bb84d6c035d8b92026b888944))


### Bug Fixes

* **chart:** refuse to render when the node-agent CA Secret is not provided ([#326](https://github.com/zeroroot-ai/setec/issues/326)) ([e55b946](https://github.com/zeroroot-ai/setec/commit/e55b946b9cc6e49a323082822c62b8da09281a4b))
* **controller:** collect orphaned Sandboxes instead of letting them accumulate ([#314](https://github.com/zeroroot-ai/setec/issues/314)) ([83be68a](https://github.com/zeroroot-ai/setec/commit/83be68a4f78d95f8f1c139fdb46183dcd5ef18af)), closes [#299](https://github.com/zeroroot-ai/setec/issues/299)
* **controller:** fail a Sandbox terminally when its class never resolves ([#307](https://github.com/zeroroot-ai/setec/issues/307)) ([3fad352](https://github.com/zeroroot-ai/setec/commit/3fad352d52bfb036acdb304c3ff2dbd10cffddb3))
* **e2e:** give the install room for a cold metal node, and dump state before tearing it down ([#322](https://github.com/zeroroot-ai/setec/issues/322)) ([19065d5](https://github.com/zeroroot-ai/setec/commit/19065d5587f6734d6bc8f66005d9cf2e6ac11a77))
* **e2e:** unblock the suite install by making snapshots opt-in until the chart can carry them ([#321](https://github.com/zeroroot-ai/setec/issues/321)) ([7592dcc](https://github.com/zeroroot-ai/setec/commit/7592dcc24e8fd0ebfdfd5bed867a7a5ef99dccfb))
* **frontend:** serve terminated-container logs instead of failing the attach ([#313](https://github.com/zeroroot-ai/setec/issues/313)) ([28c2910](https://github.com/zeroroot-ai/setec/commit/28c29107808a5b020403a2a2d98421e21419f837)), closes [#263](https://github.com/zeroroot-ai/setec/issues/263)
* **guest-agent:** make Serve wait for in-flight handlers before returning ([#324](https://github.com/zeroroot-ai/setec/issues/324)) ([9b0cd61](https://github.com/zeroroot-ai/setec/commit/9b0cd61d8f013ae7d5070087b0f1053a1a28f548))
* **pool-vm:** resolve the firecracker binary from one shared path ([#305](https://github.com/zeroroot-ai/setec/issues/305)) ([3f863d7](https://github.com/zeroroot-ai/setec/commit/3f863d72f75d73b68a951df0485dd930bce4442b))

## [0.111.0](https://github.com/zeroroot-ai/setec/compare/v0.110.2...v0.111.0) (2026-08-15)


### Features

* **chart:** let RuntimeClasses publish scheduling.tolerations ([#306](https://github.com/zeroroot-ai/setec/issues/306)) ([4d9700a](https://github.com/zeroroot-ai/setec/commit/4d9700a5fa351d75c87d3cf62766bd2e9af2feea))
* **session:** make the S3 checkpoint path runnable against real infrastructure ([#294](https://github.com/zeroroot-ai/setec/issues/294)) ([1e5fa2e](https://github.com/zeroroot-ai/setec/commit/1e5fa2e34a30551b3274e01b5ecc5ff6dffa9446))


### Bug Fixes

* **deps:** bump x/net and x/text in the four satellite Go modules ([#290](https://github.com/zeroroot-ai/setec/issues/290)) ([8a4e167](https://github.com/zeroroot-ai/setec/commit/8a4e16719c7abbc282bb6bfc9c3db78935828978))
* **installer:** drop the unused kata CLI payload and bump kata to 3.32.0 ([#283](https://github.com/zeroroot-ai/setec/issues/283)) ([3c1384e](https://github.com/zeroroot-ai/setec/commit/3c1384e3cf3da4cbaa7add992ab3fef78893ea50))
* **operator:** create the Pod when no node is capable yet, so autoscalers can provision ([#301](https://github.com/zeroroot-ai/setec/issues/301)) ([6ba6823](https://github.com/zeroroot-ai/setec/commit/6ba68231bafd437e5df54f2e2a9133130651ca92))
* **supply-chain:** pin the example base images and the k3s installer by digest ([#291](https://github.com/zeroroot-ai/setec/issues/291)) ([dda2248](https://github.com/zeroroot-ai/setec/commit/dda2248af9e958f213e330e800790f30ef479066))

## [0.110.2](https://github.com/zeroroot-ai/setec/compare/v0.110.1...v0.110.2) (2026-08-15)


### Bug Fixes

* **ci:** bump reusable-image-build.yml pin to pick up main-branch Trivy scanning ([#279](https://github.com/zeroroot-ai/setec/issues/279)) ([109c995](https://github.com/zeroroot-ai/setec/commit/109c9954685eb93290a36aadfc6f8040bef1507d))
* **runtime-agent:** follow containerd imports so kata-deploy nodes probe truthfully ([#282](https://github.com/zeroroot-ai/setec/issues/282)) ([710e0b5](https://github.com/zeroroot-ai/setec/commit/710e0b5e7c72924b4ad4510c1c3e7d427bec7a16))

## [0.110.1](https://github.com/zeroroot-ai/setec/compare/v0.110.0...v0.110.1) (2026-08-15)


### Bug Fixes

* **build:** re-pin builder base to mirror/golang 1.26.6 — go.mod requires &gt;= 1.26.6 ([#262](https://github.com/zeroroot-ai/setec/issues/262)) ([11e4435](https://github.com/zeroroot-ai/setec/commit/11e4435611131ea13a3653ab0220e8f048a6b850)), closes [#261](https://github.com/zeroroot-ai/setec/issues/261)
* **ci:** pin actions to SHAs, least-privilege permissions, bump vulnerable deps ([#278](https://github.com/zeroroot-ai/setec/issues/278)) ([3aaf3a5](https://github.com/zeroroot-ai/setec/commit/3aaf3a5cdb87547c18d9db051abd1213df16ac9a))
* **ci:** verify the lint config against a vendored schema, not the network ([#265](https://github.com/zeroroot-ai/setec/issues/265)) ([3a22b24](https://github.com/zeroroot-ai/setec/commit/3a22b24e8f7739fba831ef2fe5bfdffeb3f5595c))
* **deps:** bump Go to 1.26.6 — two newer stdlib govulncheck findings ([#259](https://github.com/zeroroot-ai/setec/issues/259)) ([9fa573d](https://github.com/zeroroot-ai/setec/commit/9fa573d703dc69c881c260600d8106adf8ca1a67)), closes [#254](https://github.com/zeroroot-ai/setec/issues/254)
* **deps:** bump grpc to v1.83.0, x/text to v0.39.0 and Go to 1.26.5 for govulncheck ([#256](https://github.com/zeroroot-ai/setec/issues/256)) ([b7e7a44](https://github.com/zeroroot-ai/setec/commit/b7e7a4471167a98fc8db3757f728fa847b5c1179)), closes [#254](https://github.com/zeroroot-ai/setec/issues/254)
* **e2e:** teardown that survives cancellation, plus a stale-namespace sweep ([#269](https://github.com/zeroroot-ai/setec/issues/269)) ([807f161](https://github.com/zeroroot-ai/setec/commit/807f161d72c3954c8f03e999ea26c3ae0757050c))
* **operator:** treat NoEligibleNode as Pending with requeue so scale-from-zero can work ([#267](https://github.com/zeroroot-ai/setec/issues/267)) ([3697265](https://github.com/zeroroot-ai/setec/commit/36972652aaa4feff590c3a258489fb70326692ed)), closes [#230](https://github.com/zeroroot-ai/setec/issues/230)
* **runtime-agent:** require a configured containerd runtime handler before labeling kata capability ([#266](https://github.com/zeroroot-ai/setec/issues/266)) ([6a17a89](https://github.com/zeroroot-ai/setec/commit/6a17a89825117de345ac1ea48f1cbc647a192253)), closes [#243](https://github.com/zeroroot-ai/setec/issues/243)

## [0.110.0](https://github.com/zeroroot-ai/setec/compare/v0.109.3...v0.110.0) (2026-08-13)


### Features

* **abi:** report the resolved SandboxClass and runtime backend on setec.v1 ([#235](https://github.com/zeroroot-ai/setec/issues/235)) ([fc292c5](https://github.com/zeroroot-ai/setec/commit/fc292c5aefe71e2ce390c1247fc503dcaba9d12c))
* **chart:** install-wide credential-mode switch for frontend, node-agent, and operator dialer ([#236](https://github.com/zeroroot-ai/setec/issues/236)) ([d51e061](https://github.com/zeroroot-ai/setec/commit/d51e0619fd609df92bf1661c06f38ab57d8bd527)), closes [#183](https://github.com/zeroroot-ai/setec/issues/183)


### Bug Fixes

* **ci:** disable the installer DaemonSet in the e2e shadow install ([#234](https://github.com/zeroroot-ai/setec/issues/234)) ([091f7ae](https://github.com/zeroroot-ai/setec/commit/091f7ae83d847c40e5a6a089797e5f60bb04cff2))
* **ci:** gate the pre-warm on kata-deploy's own completion label ([#244](https://github.com/zeroroot-ai/setec/issues/244)) ([3ced456](https://github.com/zeroroot-ai/setec/commit/3ced456f2f05e7360325eaa2ecd1c61916a77fee))
* **ci:** pre-warm a sandbox-host metal node before the roundtrip Sandbox ([#231](https://github.com/zeroroot-ai/setec/issues/231)) ([04ef875](https://github.com/zeroroot-ai/setec/commit/04ef875d07c9dbe2c67ed4b19f82ee0c1b33da90))
* **ci:** tolerate the sandbox-host taint on the roundtrip SandboxClass ([#233](https://github.com/zeroroot-ai/setec/issues/233)) ([6ff72bb](https://github.com/zeroroot-ai/setec/commit/6ff72bb90165af0a535e6605ce69cd59b2b93f43))
* **frontend:** configurable tenant-namespace routing - label key override and fixed sandbox namespace ([#237](https://github.com/zeroroot-ai/setec/issues/237)) ([928cc0c](https://github.com/zeroroot-ai/setec/commit/928cc0ccecc090db43216d033f07d0c0612d4f75)), closes [#158](https://github.com/zeroroot-ai/setec/issues/158)
* **lint:** clear the two findings the merge queue let onto main ([#241](https://github.com/zeroroot-ai/setec/issues/241)) ([76159ab](https://github.com/zeroroot-ai/setec/commit/76159ab04983f27aaeb3f9f25bfc0a0ce95ccbc6))

## [0.109.3](https://github.com/zeroroot-ai/setec/compare/v0.109.2...v0.109.3) (2026-08-12)


### Bug Fixes

* **installer:** verify the installed tree, not the machine running the tests ([#220](https://github.com/zeroroot-ai/setec/issues/220)) ([#227](https://github.com/zeroroot-ai/setec/issues/227)) ([195bba1](https://github.com/zeroroot-ai/setec/commit/195bba17c641a7814da37f8aefadf3624a058d97))
* **lint:** clear the 41 golangci-lint findings blocking the gate ([#228](https://github.com/zeroroot-ai/setec/issues/228)) ([6c7bfa8](https://github.com/zeroroot-ai/setec/commit/6c7bfa8cf52710501580e92f69132f77ed4aabdd))
* **operator:** serve health probes without waiting for leadership ([#225](https://github.com/zeroroot-ai/setec/issues/225)) ([#226](https://github.com/zeroroot-ai/setec/issues/226)) ([257133a](https://github.com/zeroroot-ai/setec/commit/257133a07d37a3f3a0c025efdd32269c53f0b75e))

## [0.109.2](https://github.com/zeroroot-ai/setec/compare/v0.109.1...v0.109.2) (2026-08-12)


### Bug Fixes

* **chart:** put the leader-election grant in the operator's namespace ([#217](https://github.com/zeroroot-ai/setec/issues/217)) ([#223](https://github.com/zeroroot-ai/setec/issues/223)) ([3e320b3](https://github.com/zeroroot-ai/setec/commit/3e320b3e8e80ca1ccb51c7216f4c40f0dddd3b39))

## [0.109.1](https://github.com/zeroroot-ai/setec/compare/v0.109.0...v0.109.1) (2026-08-12)


### Bug Fixes

* **chart:** grant leader-election leases RBAC so the operator can start ([#219](https://github.com/zeroroot-ai/setec/issues/219)) ([0bac442](https://github.com/zeroroot-ai/setec/commit/0bac442e0494588815843ccfa3c2f2fb68ccbc82))
* **ci:** install kubectl and helm on the ephemeral e2e runner ([#215](https://github.com/zeroroot-ai/setec/issues/215)) ([93606be](https://github.com/zeroroot-ai/setec/commit/93606be2d912bc41ec5d99f16c091c9408f605f7))

## [0.109.0](https://github.com/zeroroot-ai/setec/compare/v0.108.0...v0.109.0) (2026-08-12)

> **Behavior change (chart):** since this release, a chart-managed
> RuntimeClass (`runtimes.<backend>.install: true`) publishes
> `overhead.podFixed` from the backend's `defaultOverhead` (landed in
> [#199](https://github.com/zeroroot-ai/setec/issues/199); previously the
> rendered RuntimeClass carried no `overhead` at all). Kubernetes'
> RuntimeClass admission plugin compares a declared overhead against every
> Pod that names the class: a Pod carrying a *different* overhead is now
> **rejected** (`Pod's Overhead doesn't match RuntimeClass's defined
> Overhead`) where it was previously admitted. Operator-created Sandbox
> Pods already stamp the same value, so they match; audit any Pod created
> against the class by another route before upgrading across this
> release, or set `install: false` and manage the RuntimeClass
> externally (the operator then stamps no overhead, per
> [#78](https://github.com/zeroroot-ai/setec/issues/78)). Noted
> retroactively via [#169](https://github.com/zeroroot-ai/setec/issues/169).


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
