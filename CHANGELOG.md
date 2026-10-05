# Changelog

## [0.119.0](https://github.com/zeroroot-ai/setec/compare/v0.118.3...v0.119.0) (2026-10-05)


### Features

* **network:** an egress rule names ports, port ranges and a protocol ([#200](https://github.com/zeroroot-ai/setec/issues/200)) ([934ae14](https://github.com/zeroroot-ai/setec/commit/934ae14f1e16cab89143f9e087014351088584f3))

## [0.118.3](https://github.com/zeroroot-ai/setec/compare/v0.118.2...v0.118.3) (2026-10-05)


### Bug Fixes

* **ci:** the session affinity exit test runs off the pull request lane ([#171](https://github.com/zeroroot-ai/setec/issues/171)) ([b83b5a5](https://github.com/zeroroot-ai/setec/commit/b83b5a53059031cff570a32d9a57b27fdd7ee2c1)), closes [#170](https://github.com/zeroroot-ai/setec/issues/170)

## [0.118.2](https://github.com/zeroroot-ai/setec/compare/v0.118.1...v0.118.2) (2026-10-05)


### Bug Fixes

* **build:** no build file names a platform other than linux/amd64 ([#166](https://github.com/zeroroot-ai/setec/issues/166)) ([f22aa38](https://github.com/zeroroot-ai/setec/commit/f22aa388599fa6d68174ba688739747528608bee)), closes [#160](https://github.com/zeroroot-ai/setec/issues/160)
* **rework:** the agent sees the gvisor the installer laid ([#157](https://github.com/zeroroot-ai/setec/issues/157)) ([aa198a8](https://github.com/zeroroot-ai/setec/commit/aa198a88a978ccda68d3badb1d29005ea6468070))
* **rework:** the kata pin reaches the e2e suites again ([#150](https://github.com/zeroroot-ai/setec/issues/150)) ([eb50285](https://github.com/zeroroot-ai/setec/commit/eb50285574546fdb50f03d39bff0fd8603a1e247)), closes [#147](https://github.com/zeroroot-ai/setec/issues/147)
* **rework:** the kata-qemu e2e prep reaches the node and uses the kata 4.x layout ([#152](https://github.com/zeroroot-ai/setec/issues/152)) ([54385ab](https://github.com/zeroroot-ai/setec/commit/54385ab5e8a0eb6a979dce0d5b55051b8fc37286))

## [0.118.1](https://github.com/zeroroot-ai/setec/compare/v0.118.0...v0.118.1) (2026-10-04)


### Bug Fixes

* **deps:** the grpc advisory ignore covered one module of four ([#142](https://github.com/zeroroot-ai/setec/issues/142)) ([80ba36e](https://github.com/zeroroot-ai/setec/commit/80ba36e3d30efbcb2ebf17f7f7818e35349d95f6)), closes [#125](https://github.com/zeroroot-ai/setec/issues/125)
* **images:** pin the installer to v0.10.5, which is the first release that can match ([#146](https://github.com/zeroroot-ai/setec/issues/146)) ([a045f3d](https://github.com/zeroroot-ai/setec/commit/a045f3da7c12a2f44c9ff2668906985cae919a5b)), closes [#89](https://github.com/zeroroot-ai/setec/issues/89)
* **images:** the installer pin missed the reachability fix, so the gate read 350 ids ([#143](https://github.com/zeroroot-ai/setec/issues/143)) ([70048bc](https://github.com/zeroroot-ai/setec/commit/70048bcce9040d0570533ce68db22c4f86005f55)), closes [#89](https://github.com/zeroroot-ai/setec/issues/89)
* **rework:** the crd field gate is the shared one in ast-checks ([#149](https://github.com/zeroroot-ai/setec/issues/149)) ([96548af](https://github.com/zeroroot-ai/setec/commit/96548afac52742fc17faa49ba9c3a2761a8e0768)), closes [#148](https://github.com/zeroroot-ai/setec/issues/148)

## [0.118.0](https://github.com/zeroroot-ai/setec/compare/v0.117.0...v0.118.0) (2026-10-02)


### Features

* **crd:** a served field must have a consumer, and nine did not ([#140](https://github.com/zeroroot-ai/setec/issues/140)) ([3de44cc](https://github.com/zeroroot-ai/setec/commit/3de44cc3908e30f11cb796e9cbb1ddebdc548fc1)), closes [#121](https://github.com/zeroroot-ai/setec/issues/121)


### Bug Fixes

* **images:** declare the gVisor and kata binaries setec does not compile ([#133](https://github.com/zeroroot-ai/setec/issues/133)) ([6af81c0](https://github.com/zeroroot-ai/setec/commit/6af81c0a82df8b89c72dd99364626ac436a85e9d)), closes [#89](https://github.com/zeroroot-ai/setec/issues/89)
* **images:** re-measure reachability on .github v0.10.0 ([#135](https://github.com/zeroroot-ai/setec/issues/135)) ([1b16a46](https://github.com/zeroroot-ai/setec/commit/1b16a46f3d7cf9c55b03d5939fc267e69287af3a)), closes [#89](https://github.com/zeroroot-ai/setec/issues/89)
* **runtime:** spec.runtime.params was documented, validated, and never delivered ([#141](https://github.com/zeroroot-ai/setec/issues/141)) ([0e72ae1](https://github.com/zeroroot-ai/setec/commit/0e72ae109e12658ba9f3152f75a1689a68afe479)), closes [#121](https://github.com/zeroroot-ai/setec/issues/121)
* **sandboxclass:** a class naming its own kernel booted the operator default ([#139](https://github.com/zeroroot-ai/setec/issues/139)) ([799f587](https://github.com/zeroroot-ai/setec/commit/799f587c36c5e7b76afc7539e261a116214bb015)), closes [#126](https://github.com/zeroroot-ai/setec/issues/126)
* **snapshot:** a Snapshot could only ever report Ready ([#137](https://github.com/zeroroot-ai/setec/issues/137)) ([817f0f6](https://github.com/zeroroot-ai/setec/commit/817f0f6ed681458c35ec7dd3ce3af9377a084b03))

## [0.117.0](https://github.com/zeroroot-ai/setec/compare/v0.116.0...v0.117.0) (2026-10-02)


### Features

* **installer:** the installer lays gvisor, so a plain helm install gives working sandboxes ([#131](https://github.com/zeroroot-ai/setec/issues/131)) ([5c34814](https://github.com/zeroroot-ai/setec/commit/5c3481459d6a61d26645f76e48a0a13877f3e4cb))


### Bug Fixes

* **adr-0027:** the legacy runtime-selection path goes, and the dead chart value fails loudly ([#128](https://github.com/zeroroot-ai/setec/issues/128)) ([6226a0b](https://github.com/zeroroot-ai/setec/commit/6226a0b198d68e908edcc95ce3b0fd4c7479134e))
* **deps:** a published example selected a grpc release OSV reports as affected ([#122](https://github.com/zeroroot-ai/setec/issues/122)) ([d0b2a07](https://github.com/zeroroot-ai/setec/commit/d0b2a07bc9df141c7a5c5c6f3b0d595b58607194))
* **lint:** goconst and modernize come back on, full-tree ([#127](https://github.com/zeroroot-ai/setec/issues/127)) ([a80715a](https://github.com/zeroroot-ai/setec/commit/a80715a953b27fd906b6f10f74ebbd5b5d4865d3)), closes [#118](https://github.com/zeroroot-ai/setec/issues/118)

## [0.116.0](https://github.com/zeroroot-ai/setec/compare/v0.115.0...v0.116.0) (2026-10-01)


### Features

* **go:** move the toolchain floor to 1.27.1 ([#117](https://github.com/zeroroot-ai/setec/issues/117)) ([3451dee](https://github.com/zeroroot-ai/setec/commit/3451dee2be0dec591857d2f1ca08709cd516b7ef))


### Bug Fixes

* **ci:** link-check checks only the Markdown a PR touched (.github v0.7.2) ([#112](https://github.com/zeroroot-ai/setec/issues/112)) ([d5444c5](https://github.com/zeroroot-ai/setec/commit/d5444c5a6dab2bc70dd537ff81e36c7dd5b3793c))
* **ci:** retry go mod download too, and make the backoff knob work ([#119](https://github.com/zeroroot-ai/setec/issues/119)) ([46bc76c](https://github.com/zeroroot-ai/setec/commit/46bc76cbed698ae2baab17efe5d9d2506bd0936f))
* **ci:** survive a sum.golang.org blip instead of failing the job ([#114](https://github.com/zeroroot-ai/setec/issues/114)) ([a70ab17](https://github.com/zeroroot-ai/setec/commit/a70ab1733f20376d8ac70dae56b614e56fc2be69)), closes [#98](https://github.com/zeroroot-ai/setec/issues/98)
* **dev:** install gVisor from the release tarball, and fail closed on the checksum ([#113](https://github.com/zeroroot-ai/setec/issues/113)) ([5d3176d](https://github.com/zeroroot-ai/setec/commit/5d3176d9cdaac9e3dd0679968082e014671b72ec)), closes [#90](https://github.com/zeroroot-ai/setec/issues/90)
* **e2e:** run the kata-fc e2e suites on a hosted runner with nested KVM ([#94](https://github.com/zeroroot-ai/setec/issues/94)) ([d3bc173](https://github.com/zeroroot-ai/setec/commit/d3bc173eb71586667fbed61c0bfa157c18997d8a))
* **installer:** the payload gate is a positive inventory, proven to fail ([#79](https://github.com/zeroroot-ai/setec/issues/79)) ([5279237](https://github.com/zeroroot-ai/setec/commit/5279237d602605bd54ab8c208325000565b6e11a))
* **kata-fc:** a session's workspace survives a VM restart ([#100](https://github.com/zeroroot-ai/setec/issues/100)) ([4719261](https://github.com/zeroroot-ai/setec/commit/47192619ab9e87f636012cf57e183f74f1c6c816))
* **snapshot:** dial the node-agent Pod IP instead of a DNS name that never resolves ([#95](https://github.com/zeroroot-ai/setec/issues/95)) ([8467643](https://github.com/zeroroot-ai/setec/commit/84676436bf7bf4f9329c81894da8e0fdefda678f))
* **snapshot:** pause, snapshot and TTL work against kata's Firecracker ([#104](https://github.com/zeroroot-ai/setec/issues/104)) ([a09aaaa](https://github.com/zeroroot-ai/setec/commit/a09aaaaf4357160299679c2a6820e2f10081b4c6))

## [0.115.0](https://github.com/zeroroot-ai/setec/compare/v0.114.6...v0.115.0) (2026-09-19)


### Features

* **api:** boot a session with the setec keepalive when spec.command is empty ([#66](https://github.com/zeroroot-ai/setec/issues/66)) ([1ec5d55](https://github.com/zeroroot-ai/setec/commit/1ec5d5568e65a636d913c2d00b85a58bd41e3ddb))


### Bug Fixes

* **chart:** issue the node-agent mTLS CA and chain both leaves to it ([#65](https://github.com/zeroroot-ai/setec/issues/65)) ([7e14432](https://github.com/zeroroot-ai/setec/commit/7e14432c9b84cb4dc19f851c4a379a880e0c8bbe))
* **ci:** pin every zeroroot-ai/.github reference to v0.5.1 ([#61](https://github.com/zeroroot-ai/setec/issues/61)) ([64f6819](https://github.com/zeroroot-ai/setec/commit/64f6819abe81fd60fd4f2962d62e13a23913ca2f))
* **ci:** pin the org tree guards to a commit SHA ([#49](https://github.com/zeroroot-ai/setec/issues/49)) ([8d7af57](https://github.com/zeroroot-ai/setec/commit/8d7af57580fcd9cfb48f807294d2ba898bd4ecfa))
* **ci:** unbreak the image build ([#55](https://github.com/zeroroot-ai/setec/issues/55)) ([ef0d6d9](https://github.com/zeroroot-ai/setec/commit/ef0d6d911ee333605613f4e0255beb1e15473ded))
* **installer:** supply the devmapper snapshotter beside a foreign kata-fc handler ([#73](https://github.com/zeroroot-ai/setec/issues/73)) ([392eff1](https://github.com/zeroroot-ai/setec/commit/392eff1628add09c2a1ae5fd2d6c0c3229735c35))
* **kata:** bump to 4.2.0, clearing all four shim CVEs ([#50](https://github.com/zeroroot-ai/setec/issues/50)) ([8b8f798](https://github.com/zeroroot-ai/setec/commit/8b8f7980e31a02fa41878f9bb5002456939d0ab8))
* **leasepool:** reserve pool slots so concurrent replenishes do not over-fill ([#67](https://github.com/zeroroot-ai/setec/issues/67)) ([aa4615f](https://github.com/zeroroot-ai/setec/commit/aa4615fc0adacdb0dc6f31ec59b6652f48aedbbf))
* **license:** ship the Apache-2.0 text inside every published image ([#53](https://github.com/zeroroot-ai/setec/issues/53)) ([a99b02e](https://github.com/zeroroot-ai/setec/commit/a99b02e4143a2658c65fb3a1684a86bb54769935))
* **netpol:** reserve IPv6 ranges and refuse a one-family reserved list ([#63](https://github.com/zeroroot-ai/setec/issues/63)) ([da0dcaa](https://github.com/zeroroot-ai/setec/commit/da0dcaa230fdc6346d431f202bf3e21bdb8ff231))
* **nodeagent:** validate snapshot_id before it names a host path ([#62](https://github.com/zeroroot-ai/setec/issues/62)) ([42e0d72](https://github.com/zeroroot-ai/setec/commit/42e0d72c210f3148b4f04ff71c949a9b510460df))
* **rework:** treat renames and cross-file moves as moves in the silent-revert guard ([#70](https://github.com/zeroroot-ai/setec/issues/70)) ([8d4e8f3](https://github.com/zeroroot-ai/setec/commit/8d4e8f339bde67547590a915069cb5b8950ee00f))

## Changelog

This repository restarted from a fresh baseline on 2026-09-06. Release notes before that date are archived offline and do not resolve on GitHub. release-please adds each release below this line.
