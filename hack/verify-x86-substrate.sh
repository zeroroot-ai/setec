#!/usr/bin/env bash
# Copyright 2026 The Setec Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
#
# Guards the x86-only substrate (docs/design/runtime.md, setec#186).
#
# The sandbox substrate is x86 exclusively: every published image is
# single-arch linux/amd64 and every node-facing component pins
# kubernetes.io/arch=amd64. Both halves regress silently — a multi-arch
# `platforms:` line publishes arm64 layers nobody tests, and a dropped
# nodeSelector strands DaemonSet pods on arm64 nodes in ImagePullBackOff
# (the setec#132 failure mode). This script fails when either is
# reintroduced; re-adding arm64 requires a new ADR.
#
# The third arch surface — the Sandbox Pod's required node affinity — is
# guarded by Go unit tests (internal/runtime/*_test.go asserting the
# kubernetes.io/arch=amd64 MatchExpression), not here.
#
# Usage: hack/verify-x86-substrate.sh [chart-dir]

set -euo pipefail

CHART_DIR="${1:-charts/setec}"
HELM="${HELM:-helm}"
# The launcher values that the chart requires (hack/chart-launcher-values.yaml).
LAUNCHER_VALUES="$(dirname "$0")/chart-launcher-values.yaml"
WORKFLOW_DIR=".github/workflows"
# The files a person builds an image from by hand. Only the fixture in
# hack/verify-x86-substrate.test.sh sets X86_BUILD_FILES.
BUILD_FILES="${X86_BUILD_FILES:-Makefile Dockerfile Dockerfile.launcher Dockerfile.disk-builder}"

fail_count=0

note() { printf '  %s\n' "$*"; }
pass() { printf '  ok   %s\n' "$*"; }
fail() {
	printf '  FAIL %s\n' "$*" >&2
	fail_count=$((fail_count + 1))
}

# assert_contains <haystack-file> <description> <fixed-string...>
assert_contains() {
	local file="$1" desc="$2"
	shift 2
	local needle
	for needle in "$@"; do
		if ! grep -qF -- "$needle" "$file"; then
			fail "$desc — missing: $needle"
			return
		fi
	done
	pass "$desc"
}

# assert_absent <haystack-file> <description> <fixed-string>
assert_absent() {
	local file="$1" desc="$2" needle="$3"
	if grep -qF -- "$needle" "$file"; then
		fail "$desc — unexpectedly present: $needle"
		return
	fi
	pass "$desc"
}

printf 'verify-x86-substrate: checking %s and %s\n' "$WORKFLOW_DIR" "$CHART_DIR"

# ---------------------------------------------------------------------------
# 1. Image publishing is single-arch linux/amd64.
#
# Every `platforms:` input handed to the org reusable image-build workflow
# must be exactly linux/amd64. A count check backs the content check so a
# renamed key or restructured workflow cannot skip the gate silently.
# ---------------------------------------------------------------------------
note "single-arch image publishing (docs/design/runtime.md)"

platform_lines="$(grep -rhE '^\s*platforms:' "$WORKFLOW_DIR" || true)"
platform_count="$(printf '%s' "$platform_lines" | grep -c 'platforms:' || true)"

if [ "$platform_count" -lt 1 ]; then
	fail "no 'platforms:' inputs found under $WORKFLOW_DIR — image-build wiring moved; update this guard"
else
	pass "found $platform_count 'platforms:' input(s)"
	bad_lines="$(printf '%s\n' "$platform_lines" | grep -vE '^\s*platforms:\s*linux/amd64\s*$' || true)"
	if [ -n "$bad_lines" ]; then
		fail "non-amd64-only platforms input(s): $(printf '%s' "$bad_lines" | tr '\n' ' ')"
	else
		pass "every platforms input is exactly linux/amd64"
	fi
fi

# ---------------------------------------------------------------------------
# 1b. No build file names another platform.
#
# The workflows are not the only way to build an image. The Makefile carried a
# kubebuilder `docker-buildx` target whose default was
# linux/arm64,linux/amd64,linux/s390x,linux/ppc64le, and this guard could not
# see it because it read the workflow directory only (setec#160). Every
# linux/<arch> token in a build file must be linux/amd64, comments included: a
# comment that describes an arm64 build is how the next one gets written.
# ---------------------------------------------------------------------------
note "no build file names a platform other than linux/amd64 (docs/design/runtime.md)"

for build_file in $BUILD_FILES; do
	if [ ! -s "$build_file" ]; then
		fail "$build_file is missing or empty, so its platforms were not checked"
		continue
	fi
	other="$(grep -noE 'linux/[a-z0-9_]+' "$build_file" | grep -vE ':linux/amd64$' || true)"
	if [ -n "$other" ]; then
		fail "$build_file names a platform other than linux/amd64: $(printf '%s' "$other" | tr '\n' ' ')"
	else
		pass "$build_file names no platform other than linux/amd64"
	fi
done

# ---------------------------------------------------------------------------
# 2. Chart DaemonSets hardcode the amd64 nodeSelector.
#
# The selector must survive a values override: the templates omit the
# kubernetes.io/arch key from user-provided nodeSelector maps, so setting
# it to arm64 in values must not leak into the render.
# ---------------------------------------------------------------------------
workdir="$(mktemp -d)"
trap 'rm -rf "$workdir"' EXIT

render() {
	local out="$1"
	shift
	"$HELM" template setec "$CHART_DIR" -f "$LAUNCHER_VALUES" \
		--set webhook.certManager.enabled=true \
		--set "sandboxNamespaces={sandbox-workloads}" \
		"$@" >"$out"
}

note "device plugin DaemonSet arch gating"
render "$workdir/device-plugin.yaml" \
	--show-only templates/device-plugin-daemonset.yaml
assert_contains "$workdir/device-plugin.yaml" "device plugin pins amd64" \
	"kubernetes.io/arch: amd64"

note "node-agent DaemonSet arch gating"
render "$workdir/node-agent.yaml" \
	--set nodeAgent.enabled=true \
	--show-only templates/daemonset.yaml
assert_contains "$workdir/node-agent.yaml" "node-agent pins amd64" \
	"kubernetes.io/arch: amd64"

if [ "$fail_count" -gt 0 ]; then
	printf 'verify-x86-substrate: %d assertion(s) FAILED\n' "$fail_count" >&2
	exit 1
fi
printf 'verify-x86-substrate: all assertions passed\n'
