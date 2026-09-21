# Changelog

## [0.115.1](https://github.com/zeroroot-ai/setec/compare/v0.115.0...v0.115.1) (2026-09-21)


### Bug Fixes

* **installer:** the payload gate is a positive inventory, proven to fail ([#79](https://github.com/zeroroot-ai/setec/issues/79)) ([5279237](https://github.com/zeroroot-ai/setec/commit/5279237d602605bd54ab8c208325000565b6e11a))

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
