#!/usr/bin/env bash
# bake.sh — run packer for the kata-fc AMI with the kata pin from kata.env.
#
# kata.env at the repo root is the one place the kata release is pinned
# (setec#26). The packer variables kata_version and kata_sha256 have no
# default, so a bare `packer build .` fails; this wrapper is the supported
# entry point. Extra arguments go to packer unchanged, for example
#   ./bake.sh -var 'region=us-east-1'
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${HERE}/../.." && pwd)"
# shellcheck source=../../kata.env
. "${REPO_ROOT}/kata.env"
: "${KATA_VERSION:?kata.env did not set KATA_VERSION}"
: "${KATA_SHA256:?kata.env did not set KATA_SHA256}"
cd "${HERE}"
packer init .
packer validate -var "kata_version=${KATA_VERSION}" -var "kata_sha256=${KATA_SHA256}" "$@" .
exec packer build -var "kata_version=${KATA_VERSION}" -var "kata_sha256=${KATA_SHA256}" "$@" .
